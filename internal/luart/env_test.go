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
	cancel   func()
}

func (j *fakeJob) state() string {
	select {
	case <-j.done:
		return "done"
	default:
		return "running"
	}
}

type fakeEnv struct {
	cwd      string
	state    *memState
	mu       sync.Mutex
	jobs     map[string]*fakeJob
	notices  []string
	logs     []string
	viewed   []string
	renamed  string
	spawned  []AgentOpts
	depth    int
	models   []ModelInfo
	failures map[string]string
}

func (e *fakeEnv) Models(ctx context.Context) ([]ModelInfo, map[string]string) {
	return e.models, e.failures
}

func newEnv(t *testing.T) *fakeEnv {
	return &fakeEnv{cwd: t.TempDir(), state: &memState{m: map[string]map[string]string{}}, jobs: map[string]*fakeJob{}}
}

func (e *fakeEnv) Info() SessionInfo {
	return SessionInfo{ID: "s1", Cwd: e.cwd, Root: e.cwd, Model: "m", Effort: "low", Depth: e.depth, Home: e.cwd + "/.zel", TmpDir: e.cwd + "/tmp", LuaPath: []string{e.cwd + "/.zel/lua"}}
}
func (e *fakeEnv) ShellEnv() map[string]string { return map[string]string{"ZEL_SESSION_ID": "s1"} }
func (e *fakeEnv) State() StateStore           { return e.state }

// fakeSet is one namespace of the fake env's jobs.
type fakeSet struct {
	e    *fakeEnv
	kind string
}

func (e *fakeEnv) Tasks() Tasks   { return fakeTasks{fakeSet{e, "task"}} }
func (e *fakeEnv) Agents() Agents { return fakeAgents{fakeSet{e, "agent"}} }

func (s fakeSet) find(id string) (*fakeJob, error) {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	if j, ok := s.e.jobs[id]; ok && j.kind == s.kind {
		return j, nil
	}
	return nil, ErrNoJob
}
func (s fakeSet) List() []JobInfo {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	var out []JobInfo
	for _, j := range s.e.jobs {
		if j.kind == s.kind {
			out = append(out, JobInfo{ID: j.id, State: j.state()})
		}
	}
	return out
}
func (s fakeSet) Get(id string) (JobInfo, bool) {
	j, err := s.find(id)
	if err != nil {
		return JobInfo{}, false
	}
	return JobInfo{ID: j.id, State: j.state()}, true
}
func (s fakeSet) Wait(ctx context.Context, id string, timeout time.Duration) (map[string]any, bool, error) {
	j, err := s.find(id)
	if err != nil {
		return nil, false, err
	}
	var limit <-chan time.Time
	if timeout > 0 {
		limit = time.After(timeout)
	}
	select {
	case <-j.done:
		return j.result, true, nil
	case <-limit:
	case <-ctx.Done():
	}
	return nil, false, nil
}
func (s fakeSet) Result(id string) (map[string]any, bool, error) {
	j, err := s.find(id)
	if err != nil {
		return nil, false, err
	}
	select {
	case <-j.done:
		return j.result, true, nil
	default:
		return nil, false, nil
	}
}
func (s fakeSet) Tail(id string, n int) (string, error) {
	if _, err := s.find(id); err != nil {
		return "", err
	}
	return fmt.Sprint("tail ", n), nil
}
func (s fakeSet) Stop(id string) error {
	j, err := s.find(id)
	if err != nil {
		return err
	}
	j.stopped = true
	if j.cancel != nil {
		j.cancel()
		<-j.done
		return nil
	}
	close(j.done)
	return nil
}

type fakeTasks struct{ fakeSet }

func (s fakeTasks) Spawn(run TaskFunc, o TaskOpts) (string, error) {
	e := s.e
	e.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	j := &fakeJob{id: fmt.Sprintf("t%d", len(e.jobs)+1), kind: "task", done: make(chan struct{}), cancel: cancel}
	e.jobs[j.id] = j
	e.mu.Unlock()
	go func() {
		var out strings.Builder
		v, err := run(ctx, &out)
		res := map[string]any{"ok": err == nil, "value": v, "out": out.String()}
		if err != nil {
			res["error"] = err.Error()
		}
		j.result = res
		close(j.done)
	}()
	return j.id, nil
}

type fakeAgents struct{ fakeSet }

func (s fakeAgents) Spawn(o AgentOpts) (string, error) {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if o.Prompt == "boom" {
		return "", errors.New("no budget")
	}
	e.spawned = append(e.spawned, o)
	j := &fakeJob{id: fmt.Sprintf("a%d", len(e.jobs)+1), kind: "agent", done: make(chan struct{}), result: map[string]any{"ok": true, "answer": "re: " + o.Prompt}}
	close(j.done)
	e.jobs[j.id] = j
	return j.id, nil
}
func (e *fakeEnv) View(path string) error { e.viewed = append(e.viewed, path); return nil }
func (e *fakeEnv) Notice(msg string)      { e.notices = append(e.notices, msg) }
func (e *fakeEnv) Log(level, msg string)  { e.logs = append(e.logs, level+": "+msg) }
func (e *fakeEnv) Rename(name string) error {
	e.renamed = name
	return nil
}
