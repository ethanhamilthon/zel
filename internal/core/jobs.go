package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"zel/internal/luart"
)

// job is a background task or subagent started from Lua (or the CLI). It
// implements luart.Job on top of the tasks machinery.
type job struct {
	t                   *tasks
	id, kind, name, cmd string
	prompt, model, log  string
	started             time.Time
	timer               *time.Timer
	done                chan struct{}

	// Guarded by t.mu.
	waited   bool
	timedOut bool
	pushed   string // text of the queued result message

	mu     sync.Mutex
	status string
	result map[string]any
	agent  agentOutcome // what an in-process agent produced; set before the job ends
}

// agentOutcome is the final report of an in-process agent.
type agentOutcome struct {
	OK      bool
	Answer  string
	Session string
	Error   string
}

func (j *job) setOutcome(o agentOutcome) {
	j.mu.Lock()
	j.agent = o
	j.mu.Unlock()
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
		j.mu.Lock()
		report := j.agent
		j.mu.Unlock()
		ok := status == "done" && report.OK
		res["ok"], res["answer"], res["session"] = ok, report.Answer, report.Session
		if !ok {
			msg := report.Error
			if msg == "" {
				msg = fmt.Sprintf("agent %s", status)
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

// Stop asks the job to end gracefully (SIGTERM for a process, context
// cancellation for an agent) and forces it after 5 seconds.
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
	time.AfterFunc(5*time.Second, p.cancel)
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

// jobSet is one namespace of jobs of a session, "task" or "agent". It
// implements the lifecycle half of luart.Tasks and luart.Agents.
type jobSet struct {
	a    *Runtime
	kind string
}

func (e luaEnv) Tasks() luart.Tasks   { return taskSet{jobSet{e.a, "task"}} }
func (e luaEnv) Agents() luart.Agents { return agentSet{jobSet{e.a, "agent"}} }

func (s jobSet) find(id string) (*job, error) {
	t := s.a.tasks
	if t == nil {
		return nil, luart.ErrNoJob
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if j, ok := t.jobs[id]; ok && j.kind == s.kind {
		return j, nil
	}
	return nil, fmt.Errorf("%w: %q", luart.ErrNoJob, id)
}

func (j *job) info() luart.JobInfo {
	return luart.JobInfo{ID: j.id, Name: j.name, Cmd: j.cmd, Model: j.model, State: j.Status(), Prompt: j.prompt}
}

func (s jobSet) List() []luart.JobInfo {
	t := s.a.tasks
	if t == nil {
		return nil
	}
	t.mu.Lock()
	var jobs []*job
	for _, id := range t.order {
		if j := t.jobs[id]; j != nil && j.kind == s.kind {
			jobs = append(jobs, j)
		}
	}
	t.mu.Unlock()
	out := make([]luart.JobInfo, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.info())
	}
	return out
}

func (s jobSet) Get(id string) (luart.JobInfo, bool) {
	j, err := s.find(id)
	if err != nil {
		return luart.JobInfo{}, false
	}
	return j.info(), true
}

func (s jobSet) Wait(ctx context.Context, id string, timeout time.Duration) (map[string]any, bool, error) {
	j, err := s.find(id)
	if err != nil {
		return nil, false, err
	}
	res, ok := j.Wait(ctx, timeout)
	return res, ok, nil
}

func (s jobSet) Result(id string) (map[string]any, bool, error) {
	j, err := s.find(id)
	if err != nil {
		return nil, false, err
	}
	res, ok := j.Result()
	return res, ok, nil
}

func (s jobSet) Tail(id string, n int) (string, error) {
	j, err := s.find(id)
	if err != nil {
		return "", err
	}
	return j.Tail(n), nil
}

func (s jobSet) Stop(id string) error {
	j, err := s.find(id)
	if err != nil {
		return err
	}
	j.Stop()
	return nil
}

type taskSet struct{ jobSet }

func (s taskSet) Spawn(cmd string, o luart.TaskOpts) (string, error) {
	t := s.a.tasks
	if t == nil {
		return "", errors.New("no active session")
	}
	j, err := t.spawn(cmd, startOpts{kind: "task", name: o.Name, cwd: o.Cwd, env: o.Env, timeout: o.Timeout, waited: o.Waited})
	if err != nil {
		return "", err
	}
	return j.id, nil
}

type agentSet struct{ jobSet }

func (s agentSet) Spawn(o luart.AgentOpts) (string, error) {
	j, err := s.a.spawnAgent(o)
	if err != nil {
		return "", err
	}
	return j.id, nil
}
