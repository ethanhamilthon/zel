package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"zel/internal/entities"
	"zel/internal/shell"
	"zel/internal/store"
)

// sessionClosedTimeout is the shutdown budget of a session: stopping its tasks
// and running its session_closed hooks.
const sessionClosedTimeout = 5 * time.Second

const (
	taskResultLines = 40
)

type tasks struct {
	db               *store.Store
	project, session string
	s                *session
	queue            *Steering
	mu               sync.Mutex
	running          map[string]*proc
	stopped          map[string]bool
	closed           bool
	wg               sync.WaitGroup
	report           func(string)
	jobs             map[string]*job
	changed          func() // called after a job started or ended; never nil
	order            []string
	// life is the lifecycle context of the session; it ends with the session's
	// shutdown budget (see startBudget).
	life     context.Context
	stopLife context.CancelFunc
	budget   sync.Once
}

func (s *session) newTasks() *tasks {
	// Tasks of an earlier run of this session died with its process.
	_ = s.db.LoseTasks(s.project, s.id)
	life, stopLife := context.WithCancel(context.Background())
	return &tasks{db: s.db, project: s.project, session: s.id, s: s,
		queue: s.queue, report: func(text string) { s.LogEvent("error", text) }, running: map[string]*proc{}, stopped: map[string]bool{}, jobs: map[string]*job{},
		life: life, stopLife: stopLife, changed: func() { s.changes.notify(); s.checkIdle() }}
}

// proc is one running task: a bash command in its own process group.
type proc struct {
	cancel func()       // end it now
	term   func()       // ask it to end; the caller cancels it later if it lingers
	wait   func() error // blocks until it ended
	exit   func() int   // exit code after wait; negative when unknown
}

// startOpts describe a background task.
type startOpts struct {
	name, cwd string
	timeout   time.Duration
}

// spawn starts command with `bash -c` as a background task.
func (t *tasks) spawn(command string, o startOpts) (*job, error) {
	var j *job
	_, err := t.start(command, o, func(log io.Writer, jb *job) (*proc, error) {
		j = jb
		env := t.s.shellEnv()
		env["ZEL_TASK"] = jb.id
		cmd := shell.Command(context.Background(), cmp.Or(o.cwd, t.s.bashCwd()), env, command)
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		done := make(chan error, 1)
		go func() { done <- shell.Reap(cmd) }()
		var waitErr error
		signal := func(sig syscall.Signal) { _ = syscall.Kill(-cmd.Process.Pid, sig) }
		return &proc{cancel: func() { signal(syscall.SIGKILL) }, term: func() { signal(syscall.SIGTERM) },
			wait: func() error { waitErr = <-done; return waitErr },
			exit: func() int { return exitCode(waitErr) }}, nil
	})
	if err == nil {
		t.changed()
	}
	return j, err
}

func (t *tasks) start(command string, o startOpts, launch func(io.Writer, *job) (*proc, error)) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return "", errors.New("session has ended")
	}
	if len(t.running)+t.queue.noticeCount() >= maxQueued {
		return "", errors.New("too many unresolved task results; wait for the model to read them")
	}
	list, err := t.db.Tasks(t.project, t.session)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("t%d", len(list)+1)
	logPath := t.db.TaskLog(t.project, t.session, id)
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	started := time.Now()
	if err := t.db.CreateTask(t.project, t.session, id, o.name, command, started); err != nil {
		file.Close()
		return "", err
	}
	j := &job{t: t, id: id, name: o.name, cmd: command, log: logPath,
		started: started, done: make(chan struct{}), status: "running"}
	p, err := launch(&taskLog{file: file}, j)
	if err != nil {
		file.Close()
		return "", errors.Join(err, t.finish(id, "failed", nil))
	}
	t.running[id] = p
	t.jobs[id] = j
	t.order = append(t.order, id)
	if o.timeout > 0 {
		timer := time.AfterFunc(o.timeout, func() {
			t.mu.Lock()
			j.timedOut = true
			t.mu.Unlock()
			p.cancel()
		})
		j.timer = timer
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		err := p.wait()
		file.Close()
		if j.timer != nil {
			j.timer.Stop()
		}
		t.mu.Lock()
		stopped, timedOut := t.stopped[id], j.timedOut
		t.mu.Unlock()
		status, code := "done", p.exit()
		switch {
		case timedOut:
			status = "timeout"
		case stopped:
			status = "stopped"
		case err != nil || code != 0:
			status = "failed"
		}
		exit := &code
		if code < 0 {
			exit = nil
		}
		persistErr := t.finish(id, status, exit)
		took := time.Since(started)
		if exit == nil {
			code = -1
		}
		j.complete(status, code, took)
		notice := notification(j.noticeText(status))
		if persistErr != nil {
			warning := "Task " + id + " completion could not be persisted; saved status may be stale: " + persistErr.Error()
			if t.report != nil {
				t.report(warning)
			}
		}
		// The result is queued before the task stops counting as running, so a waiting
		// consumer cannot observe neither a task nor its result.
		t.mu.Lock()
		if !t.closed && !j.waited {
			t.queue.PushNotice(notice)
			j.pushed = notice.Text
		}
		delete(t.running, id)
		t.mu.Unlock()
		close(j.done)
		t.changed()
	}()
	return id, nil
}

func (t *tasks) finish(id, status string, exit *int) error {
	return t.db.FinishTask(t.project, t.session, id, status, exit)
}

func (t *tasks) Running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.running)
}

// startBudget starts the session's shutdown budget, once: after
// sessionClosedTimeout the lifecycle context ends, so session_closed hooks still
// pending are skipped and close stops waiting for jobs.
func (t *tasks) startBudget() {
	t.budget.Do(func() { time.AfterFunc(sessionClosedTimeout, t.stopLife) })
}

// close ends the session's jobs and then runs its session_closed hooks (sessionClosed may
// be nil), all within one shutdown budget that starts here: running jobs are
// cancelled, close waits for them until the budget ends, and sessionClosed gets what
// is left. A job still running when the budget ends is abandoned and its result is
// not queued. Close is idempotent; a later call only waits for what is left.
func (t *tasks) close(sessionClosed func(ctx context.Context)) {
	t.startBudget()
	t.begin()
	t.drain()
	if sessionClosed != nil {
		sessionClosed(t.life)
	}
	t.stopLife()
}

// begin stops new jobs and cancels the running ones.
func (t *tasks) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for id, p := range t.running {
		t.stopped[id] = true
		p.cancel()
	}
}

// drain waits until every job has ended or the budget ends.
// No job can start after begin, so the wait group cannot grow while it runs.
func (t *tasks) drain() {
	drained := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-t.life.Done():
	}
}

var IsNotice = entities.IsNotice

func (a *Runtime) RunningTasks() int { return a.session().RunningTasks() }

func (s *session) RunningTasks() int {
	if s.tasks == nil {
		return 0
	}
	return s.tasks.Running()
}

// eventsLog is the file of the session that keeps the notices, errors and hook runs.
func (s *session) eventsLog() string { return filepath.Join(s.sessionDir(), "trace.log") }

func (a *Runtime) LogEvent(kind, text string) { a.session().LogEvent(kind, text) }

// LogEvent appends one notice or error to the session's events log; failures are ignored.
func (s *session) LogEvent(kind, text string) {
	if s.home == "" || s.id == "" || s.db == nil {
		return
	}
	path := s.eventsLog()
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	text = strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n    ")
	fmt.Fprintf(file, "%s %s %s\n", time.Now().Format(time.RFC3339Nano), kind, text)
}

// Task logs retain their first 8 MiB plus a marker; further output is drained
// without growing the file or blocking the child on an unread pipe.
const taskLogLimit = 8 << 20
const taskLogTruncated = "\n[task log truncated at 8 MiB; further output discarded]\n"

type taskLog struct {
	mu        sync.Mutex
	file      io.Writer
	written   int
	truncated bool
}

func (w *taskLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(p)
	keep := min(size, taskLogLimit-w.written)
	if keep > 0 {
		n, err := w.file.Write(p[:keep])
		w.written += n
		if err != nil {
			return n, err
		}
		if n != keep {
			return n, io.ErrShortWrite
		}
	}
	if keep < size && !w.truncated {
		if _, err := io.WriteString(w.file, taskLogTruncated); err != nil {
			return keep, err
		}
		w.truncated = true
	}
	return size, nil
}

// ActiveTask is a background task that is running, as the chat shows it.
type ActiveTask struct {
	ID, Name string
	Started  time.Time
}

// ActiveTasks lists the running background tasks, oldest first.
func (a *Runtime) ActiveTasks() []ActiveTask { return a.session().ActiveTasks() }

func (s *session) ActiveTasks() []ActiveTask {
	t := s.tasks
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []ActiveTask
	for _, id := range t.order {
		if j := t.jobs[id]; j != nil && t.running[id] != nil {
			out = append(out, ActiveTask{ID: j.id, Name: j.name, Started: j.started})
		}
	}
	return out
}
