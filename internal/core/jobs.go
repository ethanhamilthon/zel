package core

import (
	"cmp"
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"zel/internal/luart"
)

// job is one background task: a Lua function started with task.spawn.
type job struct {
	t             *tasks
	id, name, cmd string
	log           string
	started       time.Time
	timer         *time.Timer
	done          chan struct{}

	// Guarded by t.mu.
	waited   bool
	timedOut bool
	pushed   string // text of the queued result message

	mu      sync.Mutex
	status  string
	code    int
	took    time.Duration
	value   string // what the function returned
	failure string // the error it raised
}

func (j *job) Status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// complete records the final result; it runs before done is closed.
func (j *job) complete(status string, code int, took time.Duration) {
	j.mu.Lock()
	j.status, j.code, j.took = status, code, took
	j.mu.Unlock()
}

// noticeText is the notification the model gets when nobody waited for the
// job: its state and the start of its result or error.
func (j *job) noticeText(status string) string {
	word := map[string]string{"done": "finished", "timeout": "timed out"}[status]
	text := j.id + " " + cmp.Or(word, status)
	j.mu.Lock()
	detail := cmp.Or(j.failure, j.value)
	j.mu.Unlock()
	if detail = strings.TrimSpace(detail); detail != "" {
		if r := []rune(detail); len(r) > noticeDetail {
			detail = string(r[:noticeDetail]) + "… (task.get(\"" + j.id + "\") has the rest)"
		}
		text += ": " + detail
	}
	return text
}

// noticeDetail bounds the result in a task notification, in runes.
const noticeDetail = 2000

// info is the row of task.list and task.get.
func (j *job) info() luart.TaskInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	took := j.took
	if j.status == "running" {
		took = time.Since(j.started)
	}
	return luart.TaskInfo{ID: j.id, Name: j.name, State: j.status, Ms: took.Milliseconds(), Result: j.value, Error: j.failure}
}

// Wait blocks until the job ends, the timeout passes (zero: no limit) or ctx
// ends. ok is false when it gave up. A wait claims the result: no notification
// is queued for it (one already queued is withdrawn).
func (j *job) Wait(ctx context.Context, timeout time.Duration) bool {
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
		return true
	case <-limit:
	case <-ctx.Done():
	}
	if claimed {
		t.mu.Lock()
		defer t.mu.Unlock()
		select {
		case <-j.done:
			return true
		default:
			j.waited = false
		}
	}
	return false
}

// Stop asks the job to end (SIGTERM to its group) and kills it after 2 seconds.
func (j *job) Stop() {
	t := j.t
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.running[j.id]
	if !ok {
		return
	}
	t.stopped[j.id] = true
	p.term()
	time.AfterFunc(2*time.Second, p.cancel)
}

func (t *tasks) find(id string) (*job, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j, ok := t.jobs[id]; ok {
		return j, nil
	}
	return nil, fail(CodeNotFound, "ENOENT: no task %s in this session", id)
}

func (t *tasks) list() []*job {
	t.mu.Lock()
	defer t.mu.Unlock()
	var jobs []*job
	for _, id := range t.order {
		if j := t.jobs[id]; j != nil {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

func tailBytes(path string, limit int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	size := min(info.Size(), int64(max(0, limit)))
	data := make([]byte, size)
	n, _ := f.ReadAt(data, info.Size()-size)
	return strings.ToValidUTF8(string(data[:n]), "")
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
