package luart

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type memState struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func (s *memState) Get(scope, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[scope][key]
	return v, ok, nil
}
func (s *memState) Set(scope, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[scope] == nil {
		s.m[scope] = map[string]string{}
	}
	s.m[scope][key] = value
	return nil
}
func (s *memState) Delete(scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[scope], key)
	return nil
}
func (s *memState) List(scope, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.m[scope] {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
func (s *memState) Size(scope string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.m[scope] {
		n += len(k) + len(v)
	}
	return n, nil
}

type fakeJob struct {
	id, kind string
	done     chan struct{}
	result   map[string]any
	stopped  bool
}

func (j *fakeJob) ID() string   { return j.id }
func (j *fakeJob) Kind() string { return j.kind }
func (j *fakeJob) Status() string {
	select {
	case <-j.done:
		return "done"
	default:
		return "running"
	}
}
func (j *fakeJob) Wait(ctx context.Context, timeout time.Duration) (map[string]any, bool) {
	var limit <-chan time.Time
	if timeout > 0 {
		limit = time.After(timeout)
	}
	select {
	case <-j.done:
		return j.result, true
	case <-limit:
	case <-ctx.Done():
	}
	return nil, false
}
func (j *fakeJob) Result() (map[string]any, bool) {
	select {
	case <-j.done:
		return j.result, true
	default:
		return nil, false
	}
}
func (j *fakeJob) Tail(n int) string { return fmt.Sprint("tail ", n) }
func (j *fakeJob) Stop()             { j.stopped = true; close(j.done) }

type fakeEnv struct {
	cwd     string
	state   *memState
	mu      sync.Mutex
	jobs    map[string]*fakeJob
	notices []string
	logs    []string
	viewed  []string
	renamed string
	spawned []AgentOpts
}

func newEnv(t *testing.T) *fakeEnv {
	return &fakeEnv{cwd: t.TempDir(), state: &memState{m: map[string]map[string]string{}}, jobs: map[string]*fakeJob{}}
}

func (e *fakeEnv) Info() SessionInfo {
	return SessionInfo{ID: "s1", Cwd: e.cwd, Root: e.cwd, Model: "m", Effort: "low", Home: e.cwd + "/.zel", TmpDir: e.cwd + "/tmp", LuaPath: []string{e.cwd + "/.zel/lua"}}
}
func (e *fakeEnv) ShellEnv() map[string]string { return map[string]string{"ZEL_SESSION_ID": "s1"} }
func (e *fakeEnv) State() StateStore           { return e.state }
func (e *fakeEnv) SpawnTask(cmd string, o TaskOpts) (Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &fakeJob{id: fmt.Sprintf("t%d", len(e.jobs)+1), kind: "task", done: make(chan struct{}), result: map[string]any{"ok": true, "out": cmd}}
	e.jobs[j.id] = j
	if cmd == "instant" {
		close(j.done)
	}
	return j, nil
}
func (e *fakeEnv) SpawnAgent(o AgentOpts) (Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o.Prompt == "boom" {
		return nil, errors.New("no budget")
	}
	e.spawned = append(e.spawned, o)
	j := &fakeJob{id: fmt.Sprintf("a%d", len(e.jobs)+1), kind: "agent", done: make(chan struct{}), result: map[string]any{"ok": true, "answer": "re: " + o.Prompt}}
	close(j.done)
	e.jobs[j.id] = j
	return j, nil
}
func (e *fakeEnv) GetJob(kind, id string) (Job, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok || j.kind != kind {
		return nil, false
	}
	return j, true
}
func (e *fakeEnv) ListJobs(kind string) []JobInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []JobInfo
	for _, j := range e.jobs {
		if j.kind == kind {
			out = append(out, JobInfo{ID: j.id, Kind: kind, State: j.Status()})
		}
	}
	return out
}
func (e *fakeEnv) View(path string) error { e.viewed = append(e.viewed, path); return nil }
func (e *fakeEnv) Notice(msg string)      { e.notices = append(e.notices, msg) }
func (e *fakeEnv) Log(level, msg string)  { e.logs = append(e.logs, level+": "+msg) }
func (e *fakeEnv) Rename(name string) error {
	e.renamed = name
	return nil
}
