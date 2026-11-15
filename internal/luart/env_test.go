package luart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

type fakeJob struct {
	id, kind, name string
	done           chan struct{}
	result         map[string]any
	stopped        bool
	cancel         func()
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
	return &fakeEnv{cwd: t.TempDir(), jobs: map[string]*fakeJob{}}
}

func (e *fakeEnv) Info() SessionInfo {
	return SessionInfo{ID: "s1", Cwd: e.cwd, Root: e.cwd, Model: "m", Effort: "low", Depth: e.depth, Home: e.cwd + "/.zel", TmpDir: e.cwd + "/tmp", LuaPath: []string{e.cwd + "/.zel/lua"}}
}
func (e *fakeEnv) ShellEnv() map[string]string { return map[string]string{"ZEL_SESSION_ID": "s1"} }

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
			out = append(out, JobInfo{ID: j.id, Name: j.name, State: j.state()})
		}
	}
	return out
}
func (s fakeSet) Get(id string) (JobInfo, bool) {
	j, err := s.find(id)
	if err != nil {
		return JobInfo{}, false
	}
	return JobInfo{ID: j.id, Name: j.name, State: j.state()}, true
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
	j := &fakeJob{id: fmt.Sprintf("t%d", len(e.jobs)+1), kind: "task", name: o.Name, done: make(chan struct{}), cancel: cancel}
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
func (e *fakeEnv) Log(level, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logs = append(e.logs, level+": "+msg)
}
func (e *fakeEnv) Rename(name string) error {
	e.renamed = name
	return nil
}

func TestHostActivityLabelsAndEnds(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
write("notes.txt", "x")
print(read("notes.txt"))
print(pcall(read, "missing.txt"))
local id = tasks.spawn("probe", function() return exists("notes.txt") end)
tasks.wait(id, 5)
return "ok"`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	// The main call's operations are sequential; a background task runs beside them,
	// so its operations are compared as a set.
	var fg, bg []string
	env.mu.Lock()
	defer env.mu.Unlock()
	for _, act := range env.acts {
		if !act.ended {
			t.Errorf("activity %q never ended: a failed or finished call must clear it", act.label)
		}
		if act.background {
			bg = append(bg, act.label)
		} else {
			fg = append(fg, act.label)
		}
	}
	if want := "writing notes.txt|reading notes.txt|reading missing.txt|starting task probe|waiting for task t1"; strings.Join(fg, "|") != want {
		t.Errorf("foreground activities = %q, want %q", strings.Join(fg, "|"), want)
	}
	if strings.Join(bg, "|") != "checking notes.txt" {
		t.Errorf("background activities = %q", strings.Join(bg, "|"))
	}
}

// labelOf is the label a host function gets for the given arguments, after the
// same scrubbing the host applies before reporting it.
func labelOf(t *testing.T, name string, args ...lua.LValue) string {
	t.Helper()
	v := newVM(newEnv(t), nil, roleMain)
	defer v.close()
	for _, a := range args {
		v.L.Push(a)
	}
	return scrubLabel(v.describe(name, v.L))
}

// Credentials on a command line, in a URL or in a flag never reach a label:
// shell labels show the program name, and plain paths only for inert programs.
func TestShellLabelsHideArgumentsAndCredentials(t *testing.T) {
	secrets := []string{"abcdef123456", "hunter2hunter2", "two word password", "sk-live-abcdefghijkl", "ghp_abcdefghijklmnop", "alice:hunter2"}
	cases := []struct{ script, want string }{
		{`curl -u alice:hunter2 https://example.com; sleep 5`, "running bash: curl …"},
		{`curl https://alice:hunter2@example.com/private; sleep 5`, "running bash: curl …"},
		{`tool --password 'two word password'`, "running bash: tool …"},
		{"curl -H 'Authorization: Bearer abcdef123456' https://example.com", "running bash: curl …"},
		{`echo token=hunter2hunter2`, "running bash: echo …"},
		{`API_KEY="sk-live-abcdefghijkl" make`, "running bash: …"},
		{`tool --token abcdef123456`, "running bash: tool …"},
		{`echo ghp_abcdefghijklmnop`, "running bash: echo …"},
		{`cat --token abcdef123456`, "running bash: cat …"},
		{`./deploy.sh --token abcdef123456`, "running bash: ./deploy.sh …"},
		{"line one\nline two", "running bash: line …"},
		{`cd ~/projects/zel`, "running bash: cd ~/projects/zel"},
		{`cat ~/.zel/init.lua`, "running bash: cat ~/.zel/init.lua"},
		{`cd $HOME/x`, "running bash: cd …"},
		{`ls`, "running bash: ls"},
	}
	for _, c := range cases {
		got := labelOf(t, "sh", lua.LString(c.script))
		if got != c.want {
			t.Errorf("label(%q) = %q, want %q", c.script, got, c.want)
		}
		for _, secret := range secrets {
			if strings.Contains(got, secret) {
				t.Errorf("label(%q) = %q leaks %q", c.script, got, secret)
			}
		}
	}
	if got := scrubLabel("reading ~/.zel/init.lua"); got != "reading ~/.zel/init.lua" {
		t.Errorf("plain label changed: %q", got)
	}
	if got := scrubLabel(strings.Repeat("é", 300)); utf8.RuneCountInString(got) != maxLabel {
		t.Errorf("long label has %d runes, want %d", utf8.RuneCountInString(got), maxLabel)
	}
}

// Search patterns are free text that may be secrets: a search shows only its directory.
func TestSearchLabelsHideFreeText(t *testing.T) {
	const secret = "hunter2hunter2"
	for _, c := range []struct {
		name string
		args []lua.LValue
		want string
	}{
		{"grep", []lua.LValue{lua.LString(secret)}, "searching files"},
	} {
		if got := labelOf(t, c.name, c.args...); got != c.want || strings.Contains(got, secret) {
			t.Errorf("%s label = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every host function has a label, except the instant bookkeeping functions that
// never block, where a label would only flicker. Session values are not functions.
func TestEveryHostFunctionHasALabelExceptBookkeeping(t *testing.T) {
	bookkeeping := map[string]bool{"print": true, "slice": true, "log": true}
	var names []string
	for name := range builtinNames {
		switch name {
		case "tasks":
			for _, fn := range []string{"spawn", "list", "get", "wait", "tail", "stop"} {
				names = append(names, "tasks."+fn)
			}
		case "session":
			names = append(names, "session.rename")
		default:
			names = append(names, name)
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		label := labelOf(t, name)
		switch {
		case bookkeeping[name] && label != "":
			t.Errorf("%s is bookkeeping but has label %q", name, label)
		case !bookkeeping[name] && label == "":
			t.Errorf("%s has no activity label", name)
		}
	}
	for name := range bookkeeping {
		if !seen[name] {
			t.Errorf("bookkeeping exception %s is not a host function", name)
		}
	}
}

// A cancelled call ends its activity, and a registered extension function shows
// its own name while it runs.
func TestActivityEndsOnCancelAndCoversExtensions(t *testing.T) {
	env := newEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `
return {tools = {["zel.wait"] = {desc = "waits", fn = function() while true do end end}}}`)}})
	defer cfg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if res := (&Runner{Env: env, Cfg: cfg}).Run(ctx, `zel.wait()`); !res.IsError {
		t.Fatalf("a cancelled call must fail: %q", res.Output)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	var sawExtension bool
	for _, act := range env.acts {
		if !act.ended {
			t.Errorf("activity %q never ended after the cancel", act.label)
		}
		sawExtension = sawExtension || act.label == "running zel.wait"
	}
	if !sawExtension {
		t.Fatalf("the extension function shows no activity: %+v", env.acts)
	}
}
