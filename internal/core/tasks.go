package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"zel/internal/entities"
	"zel/internal/store"

	"zel/internal/shell"
)

const (
	taskResultLines = 40
	taskResultBytes = 8192
)

type tasks struct {
	db                *store.Store
	session, dir, log string
	env               map[string]string
	queue             *Steering
	mu                sync.Mutex
	running           map[string]*exec.Cmd
	stopped           map[string]bool
	closed            bool
	wg                sync.WaitGroup
	report            func(string)
	steps             map[string]int
	jobs              map[string]*job
	order             []string
	hook              func(kind, id string, result map[string]any) (text string, drop bool)
}

func (a *Runtime) newTasks() *tasks {
	// Tasks of an earlier run of this session died with its process.
	_ = a.db.LoseTasks(a.id)
	return &tasks{db: a.db, session: a.id, dir: a.Shell.Dir, log: filepath.Join(a.home, "tasks"), env: a.shellEnv(),
		queue: a.queue, report: func(text string) { a.LogEvent("error", text) }, running: map[string]*exec.Cmd{}, stopped: map[string]bool{}, steps: map[string]int{}, jobs: map[string]*job{}}
}

func (t *tasks) run(command string) (string, error) {
	j, err := t.spawn(command, startOpts{})
	if err != nil {
		return "", err
	}
	return j.id, nil
}

// startOpts describe a background job: a plain task or a subagent.
type startOpts struct {
	kind, name, prompt, model, report string
	cwd                               string
	env                               map[string]string
	timeout                           time.Duration
	waited                            bool // the caller waits: no result message is queued
}

// spawn runs command in the background as a task or subagent job.
func (t *tasks) spawn(command string, o startOpts) (*job, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("tasks run requires a command")
	}
	dir := t.dir
	if o.cwd != "" {
		dir = o.cwd
	}
	var j *job
	_, err := t.start(command, o, func(file io.Writer, jb *job) (*exec.Cmd, func() error, error) {
		j = jb
		env := map[string]string{}
		for k, v := range t.env {
			env[k] = v
		}
		for k, v := range o.env {
			env[k] = v
		}
		cmd := shell.Command(context.Background(), dir, env, command)
		cmd.Stdout, cmd.Stderr = file, file
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return cmd, func() error { return shell.Reap(cmd) }, nil
	})
	return j, err
}

func (t *tasks) adopt(command string, bg *shell.Background) (string, error) {
	return t.start(command, startOpts{}, func(file io.Writer, _ *job) (*exec.Cmd, func() error, error) {
		bg.Redirect(file)
		return bg.Cmd, bg.Wait, nil
	})
}

func (t *tasks) start(command string, o startOpts, launch func(io.Writer, *job) (*exec.Cmd, func() error, error)) (string, error) {
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
		report: o.report, started: started, waited: o.waited, done: make(chan struct{}), status: "running"}
	cmd, wait, err := launch(&taskLog{file: file}, j)
	if err != nil {
		file.Close()
		return "", errors.Join(err, t.finish(id, "failed", nil))
	}
	t.running[id] = cmd
	t.jobs[id] = j
	t.order = append(t.order, id)
	if o.timeout > 0 {
		timer := time.AfterFunc(o.timeout, func() {
			t.mu.Lock()
			j.timedOut = true
			t.mu.Unlock()
			_ = cmd.Cancel()
		})
		j.timer = timer
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		err := wait()
		file.Close()
		if j.timer != nil {
			j.timer.Stop()
		}
		t.mu.Lock()
		stopped, timedOut := t.stopped[id], j.timedOut
		t.mu.Unlock()
		status, code := "done", cmd.ProcessState.ExitCode()
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
		result := j.complete(status, code, took)
		text := j.noticeText(result, status, exit, took)
		drop := false
		if t.hook != nil {
			if rewritten, d := t.hook(j.kind, id, result); d {
				drop = true
			} else if rewritten != "" {
				text = rewritten
			}
		}
		notice := Input{Text: text, Notice: true}
		if persistErr != nil {
			warning := "Task " + id + " completion could not be persisted; saved status may be stale: " + persistErr.Error()
			notice.Text += "\n" + warning
			if t.report != nil {
				t.report(warning)
			}
		}
		// The result is queued before the task stops counting as running, so a waiting
		// consumer cannot observe neither a task nor its result.
		t.mu.Lock()
		if !t.closed && !drop && !j.waited {
			t.queue.PushNotice(notice)
			j.pushed = notice.Text
		}
		delete(t.running, id)
		t.mu.Unlock()
		close(j.done)
	}()
	return id, nil
}

func (t *tasks) finish(id, status string, exit *int) error {
	return t.db.FinishTask(t.session, id, status, exit)
}

func (t *tasks) stop(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cmd, ok := t.running[id]
	if !ok {
		return fmt.Errorf("task %s is not running", id)
	}
	t.stopped[id] = true
	return cmd.Cancel()
}

func (t *tasks) Running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.running)
}

func (t *tasks) close() {
	t.mu.Lock()
	t.closed = true
	for id, cmd := range t.running {
		t.stopped[id] = true
		_ = cmd.Cancel()
	}
	t.mu.Unlock()
	t.wg.Wait()
}

func taskResult(id, command, status string, exit *int, took time.Duration, logPath string) string {
	attrs := fmt.Sprintf(`id=%q status=%q`, id, status)
	if exit != nil {
		attrs += fmt.Sprintf(` exit_code="%d"`, *exit)
	}
	attrs += fmt.Sprintf(` duration=%q`, took.Round(time.Second).String())
	body := xmlText.Replace("$ " + command + "\n" + tail(logPath))
	return "<task-result " + attrs + ">\n" + strings.TrimRight(body, "\n") + "\n[full output: " + logPath + "]\n</task-result>"
}

// xmlText escapes text inside a tag; unlike xml.EscapeText it keeps newlines and quotes.
var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

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

func (a *Runtime) RunningTasks() int {
	if a.tasks == nil {
		return 0
	}
	return a.tasks.Running()
}

// EventsLog is the per-session file that keeps the notices and errors the UI showed.
func EventsLog(home, session string) string {
	return filepath.Join(home, "events", session+".log")
}

// LogEvent appends one notice or error to the session's events log; failures are ignored.
func (a *Runtime) LogEvent(kind, text string) {
	if a.home == "" || a.id == "" {
		return
	}
	path := EventsLog(a.home, a.id)
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

func SessionTasks(home, session string) ([]Task, error) {
	db, err := openDB(DBPath(home))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return sessionTasks(db, session)
}

func (a *Runtime) Tasks() ([]Task, error) { return sessionTasks(a.db, a.id) }

func (a *Runtime) StopTask(id string) error {
	if a.tasks == nil {
		return fmt.Errorf("task %s is not running", id)
	}
	return a.tasks.stop(id)
}

func (a *Runtime) SetTaskSteps(id string, steps int) {
	if a.tasks != nil {
		a.tasks.mu.Lock()
		a.tasks.steps[id] = steps
		a.tasks.mu.Unlock()
	}
}

func (a *Runtime) TaskSteps(id string) int {
	if a.tasks != nil {
		a.tasks.mu.Lock()
		defer a.tasks.mu.Unlock()
		return a.tasks.steps[id]
	}
	return 0
}

func (a *Runtime) StartSubagent(command string) *SubagentHandle {
	h := StartSubagent(a.home, a.id, ParentSession(), command)
	s := a.Settings()
	h.SetModel(s.Model, s.Effort)
	return h
}

// ActiveSubagents lists the running zel -p children of this session tree. A child
// is seen twice: as the shell command that launched it (tool call or background
// task) and as its own registered run. The two views are paired by start order so
// each child shows once. A registered run's step count is authoritative, even at zero.
func (a *Runtime) ActiveSubagents() []SubagentRun {
	rootID := a.rootID()
	byStart := func(a, b SubagentRun) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	}
	registered := ActiveSubagentsForParent(a.home, rootID)
	var launched []SubagentRun
	if a.tasks != nil {
		tasks, _ := a.Tasks()
		for _, t := range tasks {
			if t.Status == "running" && strings.Contains(t.Command, "zel -p") {
				launched = append(launched, SubagentRun{ID: t.ID, Parent: rootID, Started: t.Started, Steps: a.TaskSteps(t.ID), Command: t.Command})
			}
		}
	}
	slices.SortFunc(registered, byStart)
	slices.SortFunc(launched, byStart)
	list := registered
	for i, l := range launched {
		if i >= len(registered) {
			list = append(list, l)
		}
	}
	slices.SortFunc(list, byStart)
	return list
}

func sessionTasks(db *store.Store, session string) ([]Task, error) { return db.Tasks(session) }

func TaskOutput(task Task) string { return tail(task.Log) }

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
