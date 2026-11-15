package core

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zel/internal/entities"
	"zel/internal/luart"
	"zel/internal/store"
)

const (
	taskResultLines = 40
	taskResultBytes = 8192
)

type tasks struct {
	db           *store.Store
	session, log string
	queue        *Steering
	mu           sync.Mutex
	running      map[string]*proc
	stopped      map[string]bool
	closed       bool
	wg           sync.WaitGroup
	report       func(string)
	jobs         map[string]*job
	changed      func() // called after a job started or ended; never nil
	order        []string
	// life is the lifecycle context of the session; it ends with the session's
	// shutdown budget (see startBudget).
	life     context.Context
	stopLife context.CancelFunc
	budget   sync.Once
}

// lifeParent is what the session's lifecycle context derives from. A subagent's
// session ends within the shutdown budget of its parent, so its own session_end
// cannot outlast the parent's shutdown.
func (s *session) lifeParent() context.Context {
	if s.link != nil && s.link.budget != nil {
		return s.link.budget
	}
	return context.Background()
}

func (s *session) newTasks() *tasks {
	// Tasks of an earlier run of this session died with its process.
	_ = s.db.LoseTasks(s.id)
	life, stopLife := context.WithCancel(s.lifeParent())
	return &tasks{db: s.db, session: s.id, log: filepath.Join(s.home, "tasks"),
		queue: s.queue, report: func(text string) { s.LogEvent("error", text) }, running: map[string]*proc{}, stopped: map[string]bool{}, jobs: map[string]*job{},
		life: life, stopLife: stopLife, changed: s.tree.notify}
}

// proc is one running job: a Lua task or an in-process agent.
type proc struct {
	cancel func()       // end it now
	term   func()       // ask it to end; the caller cancels it later if it lingers
	wait   func() error // blocks until it ended
	exit   func() int   // exit code after wait; negative when unknown
}

// startOpts describe a background job: a Lua task or a subagent.
type startOpts struct {
	kind, name, prompt, model string
	timeout                   time.Duration
	waited                    bool // the caller waits: no result message is queued
}

// spawnProc starts a job whose process is created by launch.
func (t *tasks) spawnProc(command string, o startOpts, launch func(io.Writer, *job) (*proc, error)) (*job, error) {
	var j *job
	_, err := t.start(command, o, func(file io.Writer, jb *job) (*proc, error) {
		j = jb
		return launch(file, jb)
	})
	if err == nil {
		t.changed()
	}
	return j, err
}

// spawnLua runs a Lua task body in the background as a task job.
func (t *tasks) spawnLua(run luart.TaskFunc, o startOpts) (*job, error) {
	ctx, cancel := context.WithCancel(context.Background())
	label := cmp.Or(o.name, "lua function")
	j, err := t.spawnProc(label, o, func(log io.Writer, j *job) (*proc, error) {
		done := make(chan error, 1)
		var failed atomic.Bool
		go func() {
			value, err := run(ctx, log)
			j.setTaskOutcome(value, err)
			failed.Store(err != nil)
			done <- err
		}()
		return &proc{cancel: cancel, term: cancel, wait: func() error { return <-done },
			exit: func() int {
				if failed.Load() {
					return 1
				}
				return 0
			}}, nil
	})
	if err != nil {
		cancel()
	}
	return j, err
}

// cancelLua stops the running Lua tasks; init.lua was reloaded, and they run
// with the old snapshot.
func (t *tasks) cancelLua() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, p := range t.running {
		if j := t.jobs[id]; j != nil && j.kind == "task" {
			t.stopped[id] = true
			p.cancel()
		}
	}
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
	if err := os.MkdirAll(t.log, 0700); err != nil {
		return "", err
	}
	var n int
	if err := t.db.TaskCount(t.session, &n); err != nil {
		return "", err
	}
	if o.kind == "" {
		o.kind = "task"
	}
	id := fmt.Sprintf("t%d", n+1)
	if o.kind == "agent" {
		id = fmt.Sprintf("a%d", n+1)
	}
	logPath := filepath.Join(t.log, t.session+"-"+id+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	started := time.Now()
	if err := t.db.CreateTask(t.session, id, command, started, logPath); err != nil {
		file.Close()
		return "", err
	}
	j := &job{t: t, id: id, kind: o.kind, name: o.name, cmd: command, prompt: o.prompt, model: o.model, log: logPath,
		started: started, waited: o.waited, done: make(chan struct{}), status: "running"}
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
		if j.afterEnd != nil {
			j.afterEnd()
		}
	}()
	return id, nil
}

func (t *tasks) finish(id, status string, exit *int) error {
	return t.db.FinishTask(t.session, id, status, exit)
}

func (t *tasks) stop(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.running[id]
	if !ok {
		return fmt.Errorf("task %s is not running", id)
	}
	t.stopped[id] = true
	p.cancel()
	return nil
}

func (t *tasks) Running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.running)
}

// startBudget starts the session's shutdown budget, once: after
// luart.SessionEndTimeout the lifecycle context ends, so session_end hooks still
// pending are skipped and close stops waiting for jobs.
func (t *tasks) startBudget() {
	t.budget.Do(func() { time.AfterFunc(luart.SessionEndTimeout, t.stopLife) })
}

// close ends the session's jobs and then runs its session_end hooks (sessionEnd may
// be nil), all within one shutdown budget that starts here: running jobs are
// cancelled, close waits for them until the budget ends, and sessionEnd gets what
// is left. A job still running when the budget ends is abandoned and its result is
// not queued. Close is idempotent; a later call only waits for what is left.
func (t *tasks) close(sessionEnd func(ctx context.Context)) {
	t.startBudget()
	t.begin()
	t.drain()
	if sessionEnd != nil {
		sessionEnd(t.life)
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

func tail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "(no output: " + err.Error() + ")"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "(no output: " + err.Error() + ")"
	}
	start := max(0, info.Size()-taskResultBytes-1)
	data := make([]byte, info.Size()-start)
	n, err := file.ReadAt(data, start)
	if err != nil && n < len(data) {
		data = data[:n]
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return "(no output)"
	}
	lines := strings.Split(text, "\n")
	switch {
	case len(lines) > taskResultLines:
		lines = append([]string{"... earlier lines"}, lines[len(lines)-taskResultLines:]...)
	case start > 0:
		lines = append([]string{"... earlier output"}, lines...)
	}
	return strings.Join(lines, "\n")
}

var IsNotice = entities.IsNotice

func (a *Runtime) RunningTasks() int { return a.session().RunningTasks() }

func (s *session) RunningTasks() int {
	if s.tasks == nil {
		return 0
	}
	return s.tasks.Running()
}

// EventsLog is the per-session file that keeps the notices and errors the UI showed.
func EventsLog(home, session string) string {
	return filepath.Join(home, "events", session+".log")
}

func (a *Runtime) LogEvent(kind, text string) { a.session().LogEvent(kind, text) }

// LogEvent appends one notice or error to the session's events log; failures are ignored.
func (s *session) LogEvent(kind, text string) {
	if s.home == "" || s.id == "" {
		return
	}
	path := EventsLog(s.home, s.id)
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

// ActiveTasks lists the running background tasks, oldest first. Subagents are
// not tasks; a subagent started from a task shows as that task.
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
		if j := t.jobs[id]; j != nil && j.kind == "task" && t.running[id] != nil {
			out = append(out, ActiveTask{ID: j.id, Name: j.name, Started: j.started})
		}
	}
	return out
}
