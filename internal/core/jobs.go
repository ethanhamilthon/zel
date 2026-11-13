package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"zel/internal/luart"
)

// job is a background task or subagent started from Lua (or the CLI). It
// implements luart.Job on top of the tasks machinery.
type job struct {
	t                          *tasks
	id, kind, name, cmd        string
	prompt, model, log, report string
	started                    time.Time
	timer                      *time.Timer
	done                       chan struct{}

	// Guarded by t.mu.
	waited   bool
	timedOut bool
	pushed   string // text of the queued result message

	mu     sync.Mutex
	status string
	result map[string]any
}

func (j *job) ID() string   { return j.id }
func (j *job) Kind() string { return j.kind }

func (j *job) Status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// complete records the final result; it runs before done is closed.
func (j *job) complete(status string, code int, took time.Duration) map[string]any {
	res := map[string]any{"id": j.id, "ms": took.Milliseconds()}
	if j.kind == "agent" {
		report := j.readReport()
		ok := status == "done" && report.OK
		res["ok"], res["answer"], res["session"] = ok, report.Answer, report.Session
		if !ok {
			msg := report.Error
			if msg == "" {
				msg = fmt.Sprintf("agent %s (exit %d): %s", status, code, strings.TrimSpace(tailLines(j.log, 5)))
			}
			res["error"] = msg
		}
	} else {
		res["ok"] = status == "done"
		res["code"] = code
		res["out"] = tailBytes(j.log, 64<<10)
		res["log"] = j.log
		res["timed_out"] = status == "timeout"
	}
	j.mu.Lock()
	j.status, j.result = status, res
	j.mu.Unlock()
	return res
}

// agentReport is what a `zel -p` child writes to $ZEL_REPORT when it ends.
type agentReport struct {
	OK      bool   `json:"ok"`
	Answer  string `json:"answer"`
	Session string `json:"session"`
	Error   string `json:"error"`
}

func (j *job) readReport() (r agentReport) {
	if j.report == "" {
		return r
	}
	data, err := os.ReadFile(j.report)
	if err == nil {
		_ = json.Unmarshal(data, &r)
	}
	_ = os.Remove(j.report)
	return r
}

// noticeText is the message the model gets when nobody waited for the job.
func (j *job) noticeText(result map[string]any, status string, exit *int, took time.Duration) string {
	if j.kind != "agent" {
		return taskResult(j.id, j.cmd, status, exit, took, j.log)
	}
	attrs := fmt.Sprintf("id=%q status=%q", j.id, status)
	if s, _ := result["session"].(string); s != "" {
		attrs += fmt.Sprintf(" session=%q", s)
	}
	attrs += fmt.Sprintf(" duration=%q", took.Round(time.Second).String())
	body, _ := result["answer"].(string)
	if ok, _ := result["ok"].(bool); !ok {
		body, _ = result["error"].(string)
	}
	return "<agent-result " + attrs + ">\n" + strings.TrimRight(xmlText.Replace(body), "\n") + "\n</agent-result>"
}

func (j *job) Wait(ctx context.Context, timeout time.Duration) (map[string]any, bool) {
	t := j.t
	t.mu.Lock()
	claimed := false
	if !j.waited {
		j.waited, claimed = true, true
		if j.pushed != "" {
			t.queue.RemoveNotice(j.pushed)
			j.pushed = ""
		}
	}
	t.mu.Unlock()
	var limit <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		limit = timer.C
	}
	select {
	case <-j.done:
		return j.result, true
	case <-limit:
	case <-ctx.Done():
	}
	if claimed {
		t.mu.Lock()
		defer t.mu.Unlock()
		select {
		case <-j.done:
			return j.result, true
		default:
			j.waited = false
		}
	}
	return nil, false
}

func (j *job) Result() (map[string]any, bool) {
	select {
	case <-j.done:
		return j.result, true
	default:
		return nil, false
	}
}

func (j *job) Tail(n int) string { return tailLines(j.log, n) }

// Stop asks the process group to end with SIGTERM and kills it after 5 seconds.
func (j *job) Stop() {
	t := j.t
	t.mu.Lock()
	defer t.mu.Unlock()
	cmd, ok := t.running[j.id]
	if !ok {
		return
	}
	t.stopped[j.id] = true
	if cmd.Process != nil && cmd.Process.Pid > 0 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	time.AfterFunc(5*time.Second, func() { _ = cmd.Cancel() })
}

func tailBytes(path string, limit int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > limit {
		data = data[len(data)-limit:]
	}
	return strings.ToValidUTF8(string(data), "")
}

func tailLines(path string, n int) string {
	if n <= 0 {
		n = 20
	}
	text := strings.TrimRight(tailBytes(path, 256<<10), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var _ luart.Job = (*job)(nil)
