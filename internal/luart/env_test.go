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
	"unicode/utf8"
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
	acts     []fakeAct // every operation reported by Begin, in start order
}

// fakeAct is one reported host operation; ended is set by its end function.
type fakeAct struct {
	label      string
	background bool
	ended      bool
}

func (e *fakeEnv) Begin(label string, background bool) func() {
	e.mu.Lock()
	idx := len(e.acts)
	e.acts = append(e.acts, fakeAct{label: label, background: background})
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.acts[idx].ended = true
			e.mu.Unlock()
		})
	}
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

func TestHostActivityLabelsAndEnds(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
fs.write("notes.txt", "x")
print(fs.read("notes.txt"))
print(pcall(fs.read, "missing.txt"))
sleep(0)
local id = tasks.spawn(function() sleep(0) return 1 end)
tasks.wait(id, 5)
return "ok"`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	want := []struct {
		label      string
		background bool
	}{
		{"writing notes.txt", false}, {"reading notes.txt", false}, {"reading missing.txt", false},
		{"sleeping 0 sec", false}, {"starting task", false}, {"waiting for task t1", false}, {"sleeping 0 sec", true},
	}
	if len(env.acts) != len(want) {
		t.Fatalf("activities = %+v, want %d", env.acts, len(want))
	}
	for i, w := range want {
		got := env.acts[i]
		if got.label != w.label || got.background != w.background {
			t.Errorf("activity %d = %+v, want %+v", i, got, w)
		}
		if !got.ended {
			t.Errorf("activity %d %q never ended: a failed or finished call must clear it", i, got.label)
		}
	}
}

func TestScrubLabelHidesSecrets(t *testing.T) {
	secrets := []string{"abcdef123456", "hunter2hunter2", "sk-live-abcdefghijkl", "ghp_abcdefghijklmnop"}
	for _, in := range []string{
		"running bash: curl -H 'Authorization: Bearer abcdef123456' https://example.com",
		"running bash: echo token=hunter2hunter2",
		`running bash: API_KEY="sk-live-abcdefghijkl" make`,
		"running bash: tool --token abcdef123456",
		"running bash: echo ghp_abcdefghijklmnop",
	} {
		got := scrubLabel(in)
		for _, secret := range secrets {
			if strings.Contains(got, secret) {
				t.Errorf("scrubLabel(%q) = %q leaks %q", in, got, secret)
			}
		}
	}
	if got := safeURL("https://user:pw@example.com/a/b?access=xyz"); got != "https://example.com/a/b?…" {
		t.Errorf("safeURL = %q", got)
	}
	if got := scrubLabel("reading ~/.zel/init.lua"); got != "reading ~/.zel/init.lua" {
		t.Errorf("plain label changed: %q", got)
	}
	if got := scrubLabel("running bash: line one\nline two"); got != "running bash: line one …" {
		t.Errorf("multi-line label = %q", got)
	}
	if got := scrubLabel(strings.Repeat("é", 300)); utf8.RuneCountInString(got) != maxLabel {
		t.Errorf("long label has %d runes, want %d", utf8.RuneCountInString(got), maxLabel)
	}
}
