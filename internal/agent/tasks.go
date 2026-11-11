package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zel/internal/shell"
)

type Task struct {
	ID, Command, Status, Log string
	ExitCode                 *int
	Started, Ended           time.Time
}

const (
	taskResultLines = 40
	taskResultBytes = 8192
)

type tasks struct {
	db                *sql.DB
	session, dir, log string
	env               map[string]string
	queue             *Steering
	mu                sync.Mutex
	running           map[string]*exec.Cmd
	stopped           map[string]bool
	closed            bool
	wg                sync.WaitGroup
	report            func(string)
}

func (a *Agent) newTasks() *tasks {
	// Tasks of an earlier run of this session died with its process.
	_, _ = a.db.Exec(`UPDATE tasks SET status = 'lost', ended = ? WHERE session = ? AND status = 'running'`, time.Now().UnixNano(), a.id)
	return &tasks{db: a.db, session: a.id, dir: a.Shell.Dir, log: filepath.Join(a.home, "tasks"), env: a.shellEnv(),
		queue: a.queue, report: func(text string) { a.LogEvent("error", text) }, running: map[string]*exec.Cmd{}, stopped: map[string]bool{}}
}

func (t *tasks) run(command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("tasks run requires a command")
	}
	return t.start(command, func(file io.Writer) (*exec.Cmd, func() error, error) {
		cmd := shell.Command(context.Background(), t.dir, t.env, command)
		cmd.Stdout, cmd.Stderr = file, file
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return cmd, func() error { return shell.Reap(cmd) }, nil
	})
}

func (t *tasks) adopt(command string, bg *shell.Background) (string, error) {
	return t.start(command, func(file io.Writer) (*exec.Cmd, func() error, error) {
		bg.Redirect(file)
		return bg.Cmd, bg.Wait, nil
	})
}

func (t *tasks) start(command string, launch func(io.Writer) (*exec.Cmd, func() error, error)) (string, error) {
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
	if err := t.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session = ?`, t.session).Scan(&n); err != nil {
		return "", err
	}
	id := fmt.Sprintf("t%d", n+1)
	logPath := filepath.Join(t.log, t.session+"-"+id+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	started := time.Now()
	if _, err := t.db.Exec(`INSERT INTO tasks (session, id, command, status, started, log) VALUES (?, ?, ?, 'running', ?, ?)`,
		t.session, id, command, started.UnixNano(), logPath); err != nil {
		file.Close()
		return "", err
	}
	cmd, wait, err := launch(&taskLog{file: file})
	if err != nil {
		file.Close()
		return "", errors.Join(err, t.finish(id, "failed", nil))
	}
	t.running[id] = cmd
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		err := wait()
		file.Close()
		t.mu.Lock()
		stopped := t.stopped[id]
		t.mu.Unlock()
		status, code := "done", cmd.ProcessState.ExitCode()
		switch {
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
		result := Input{Text: taskResult(id, command, status, exit, time.Since(started), logPath), Notice: true}
		if persistErr != nil {
			warning := "Task " + id + " completion could not be persisted; saved status may be stale: " + persistErr.Error()
			result.Text += "\n" + warning
			if t.report != nil {
				t.report(warning)
			}
		}
		// The result is queued before the task stops counting as running, so a waiting
		// consumer cannot observe neither a task nor its result.
		t.mu.Lock()
		if !t.closed {
			t.queue.PushNotice(result)
		}
		delete(t.running, id)
		t.mu.Unlock()
	}()
	return id, nil
}

func (t *tasks) finish(id, status string, exit *int) error {
	_, err := t.db.Exec(`UPDATE tasks SET status = ?, exit_code = ?, ended = ? WHERE session = ? AND id = ?`,
		status, exit, time.Now().UnixNano(), t.session, id)
	return err
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

func IsNotice(text string) bool {
	return strings.HasPrefix(text, "<task-result ") || strings.HasPrefix(text, "<view>") || strings.HasPrefix(text, "<hook-output")
}

func (a *Agent) RunningTasks() int {
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
func (a *Agent) LogEvent(kind, text string) {
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

func (a *Agent) Tasks() ([]Task, error) { return sessionTasks(a.db, a.id) }

func (a *Agent) StopTask(id string) error {
	if a.tasks == nil {
		return fmt.Errorf("task %s is not running", id)
	}
	return a.tasks.stop(id)
}

func sessionTasks(db *sql.DB, session string) ([]Task, error) {
	rows, err := db.Query(`SELECT id, command, status, log, exit_code, started, ended FROM tasks WHERE session = ? ORDER BY started`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Task
	for rows.Next() {
		var task Task
		var code, ended sql.NullInt64
		var started int64
		if err := rows.Scan(&task.ID, &task.Command, &task.Status, &task.Log, &code, &started, &ended); err != nil {
			return nil, err
		}
		if code.Valid {
			c := int(code.Int64)
			task.ExitCode = &c
		}
		task.Started = time.Unix(0, started)
		if ended.Valid {
			task.Ended = time.Unix(0, ended.Int64)
		}
		list = append(list, task)
	}
	return list, rows.Err()
}

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
