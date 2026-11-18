package luart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEnv struct {
	info     SessionInfo
	attached string
	tasks    Tasks
	log      chan string
}

func (e *fakeEnv) Info() SessionInfo { return e.info }
func (e *fakeEnv) ShellEnv() []string {
	return []string{"ZEL_SESSION_ID=" + e.info.ID, "TEST_ENV=base"}
}
func (e *fakeEnv) Cd(dir string) error {
	if dir == "" {
		dir = e.info.Root
	}
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	e.info.Cwd = dir
	return nil
}
func (e *fakeEnv) Attach(path string) error                { e.attached = path; return nil }
func (e *fakeEnv) Choice(string, []string) (string, error) { return "c1", nil }
func (e *fakeEnv) Rename(name string) error                { e.info.Name = name; return nil }
func (e *fakeEnv) Models(context.Context) ([]ModelInfo, map[string]string) {
	return []ModelInfo{{"p", "m", 128}}, map[string]string{"x": "failure"}
}
func (e *fakeEnv) Tasks() Tasks { return e.tasks }
func (e *fakeEnv) Log(_, msg string) {
	if e.log != nil {
		e.log <- msg
	}
}

func newEnv(t *testing.T) *fakeEnv {
	dir := t.TempDir()
	return &fakeEnv{info: SessionInfo{ID: "s1", Cwd: dir, Root: dir, Mode: "main"}}
}

func run(t *testing.T, a *Agent, code string) string {
	t.Helper()
	r := a.Run(t.Context(), code, 3*time.Second)
	if !r.OK {
		t.Fatalf("%s: %+v", code, r)
	}
	return r.Out
}

func TestAgentOutputAndReset(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()
	if got := run(t, a, `x = 4; print("first", 2); return {z=2,a={3,4}}, x, nil`); got != "first\t2\n{\"a\":[3,4],\"z\":2}\n4\nnil" {
		t.Fatal(got)
	}
	r := a.Run(t.Context(), `print("before"); error("broken", 0)`, time.Second)
	if r.OK || r.Out != "before\nerror: broken" {
		t.Fatalf("%+v", r)
	}
	if got := run(t, a, `return x`); got != "4" {
		t.Fatal(got)
	}
	r = a.Run(t.Context(), `print("working"); while true do end`, 40*time.Millisecond)
	if !r.Reset || !r.TimedOut || r.OK || r.Out != "working\n[timeout after 40ms; VM reset]" {
		t.Fatalf("%+v", r)
	}
	if got := run(t, a, `return x`); got != "[lua: fresh VM]\nnil" {
		t.Fatal(got)
	}
	a.Reset()
	got := run(t, a, `print("HEAD" .. string.rep("x", 1500000)); return "TAIL"`)
	if len(got) > maxOutput || !strings.HasPrefix(got, "[lua: fresh VM]\nHEAD") || !strings.Contains(got, cutMarker) || !strings.HasSuffix(got, "\nTAIL") {
		t.Fatalf("unexpected capped output: %d bytes", len(got))
	}
}

func TestAgentHostCalls(t *testing.T) {
	env := newEnv(t)
	a := NewAgent(env, nil)
	defer a.Close()
	got := run(t, a, `
write("sub/f.txt", "one\ntwo\n")
write("sub/f.txt", "three\n", {append=true})
local ok,err = pcall(edit,"sub/f.txt",{{"one","first"},{"missing","other"}})
assert(not ok and err:match("^edit: ENOMATCH:"))
assert(read("sub/f.txt"):match("^one"))
assert(edit("sub/f.txt",{{"one","first"},{"two","second"}}) == 2)
assert(slice(read("sub/f.txt"),-2,1) == "second\n")
write("sub/a.PNG", "fake image")
assert(read("sub/a.PNG") == nil)
local s = session
cd("sub")
assert(session.cwd == s.cwd and s.cwd:match("/sub$"))
local present,kind = exists("f.txt")
assert(present and kind == "file" and #list() == 2)
assert(sh('read -r line; printf "%s:%s:%s" "$ZEL_SESSION_ID" "$TEST_ENV" "$line"', {env={TEST_ENV="over"},stdin="input\n",check=true}).out == "s1:over:input")
local r = sh("sleep 5", {timeout=.04})
assert(r.timed_out and not r.ok)
local ok,err = pcall(sh,"echo no >&2; exit 8",{check=true})
assert(not ok and err == "sh: EFAIL: exit status 8: no")
local rows,fails = models()
assert(rows[1].context_window == 128 and fails.x == "failure")
assert(choice("test",{"yes"}) == "c1")
session.rename("new")
cd()
return session.name`)
	if got != "new" || env.attached != filepath.Join(env.info.Root, "sub/a.PNG") || env.info.Cwd != env.info.Root {
		t.Fatalf("%q: %+v", got, env)
	}
	r := a.Run(t.Context(), `sh("sleep 5")`, 50*time.Millisecond)
	if !r.TimedOut || r.Ms > 1000 {
		t.Fatalf("%+v", r)
	}
}

func TestSearch(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()
	got := run(t, a, `
write(".gitignore", "*.log\n!keep.log\n/root.txt\nignored/\n")
write("sub/.gitignore", "nested.txt\n!keep.log\n")
write("ignored/keep.log", "needle")
write(".git/keep.log", "needle")
write("bad.log", "needle")
write("keep.log", "needle")
write("root.txt", "needle")
write("sub/root.txt", "zero\nneedle\n")
write("sub/nested.txt", "needle")
write("sub/keep.log", "needle")
write("binary", "needle\n\0")
assert(#glob("sub/**/*.txt") == 2)
local rows = grep("needle", {fixed=true})
assert(#rows == 3 and rows[1].file == "keep.log" and rows[3].line == 2)
assert(#grep("needle", {path="sub", glob="*.txt"}) == 1)
assert(#grep("needle", {path="ignored"}) == 0)
assert(#grep("needle", {max=1}) == 1)
return rows[3].file`)
	if got != "sub/root.txt" {
		t.Fatal(got)
	}
}

type fakeTasks struct {
	job     TaskFunc
	info    TaskInfo
	log     bytes.Buffer
	timeout time.Duration
	onWait  func(context.Context)
}

func (f *fakeTasks) Spawn(name string, timeout time.Duration, job TaskFunc) (string, error) {
	f.job, f.timeout = job, timeout
	f.info = TaskInfo{ID: "t1", Name: name, State: "running"}
	return f.info.ID, nil
}
func (f *fakeTasks) Get(id string) (TaskInfo, bool) { return f.info, id == f.info.ID }
func (f *fakeTasks) List() []TaskInfo               { return []TaskInfo{f.info} }
func (f *fakeTasks) Wait(ctx context.Context, ids []string, _ time.Duration) []TaskInfo {
	if f.onWait != nil {
		f.onWait(ctx)
		return nil
	}
	if f.job != nil {
		result, err := f.job(ctx, &f.log)
		f.info.State, f.info.Result = "done", result
		if err != nil {
			f.info.State, f.info.Error = "failed", err.Error()
		}
		f.job = nil
	}
	return []TaskInfo{f.info}
}
func (f *fakeTasks) Tail(id string, n int) (string, error) {
	if id != f.info.ID {
		return "", os.ErrNotExist
	}
	return f.log.String(), nil
}
func (f *fakeTasks) Stop(id string) error {
	if id != f.info.ID {
		return errors.New("ENOENT: no such task")
	}
	f.info.State = "stopped"
	return nil
}

func TestTaskCopiesClosures(t *testing.T) {
	env := newEnv(t)
	tasks := &fakeTasks{}
	env.tasks = tasks
	a := NewAgent(env, nil)
	defer a.Close()
	got := run(t, a, `
parent_global = 42
local t = {4}
t.self = t
local function inc() t[1] = t[1]+1 end
local function rec(n) if n > 0 then inc(); rec(n-1) end end
local format = string.format
id = task.spawn("copy", function()
 assert(parent_global == nil and t.self == t)
 rec(2)
 print(format("n=%d",t[1]))
 return {value=t[1]}
end, {timeout=8})
t[1] = 100
local a = task.wait({"unknown", id})
assert(a[1] == nil and a[2] == '{"value":6}')
assert(t[1] == 100 and task.list()[1].result == nil)
assert(task.get(id).result == a[2] and task.get("unknown") == nil)
return task.tail(id)`)
	if got != "n=6\n" || tasks.timeout != 8*time.Second {
		t.Fatalf("%q %s", got, tasks.timeout)
	}
	tasks.onWait = func(ctx context.Context) { <-ctx.Done() }
	r := a.Run(t.Context(), `task.wait("t1")`, 50*time.Millisecond)
	if !r.TimedOut {
		t.Fatalf("%+v", r)
	}
}

var _ io.Writer = (*output)(nil)

func TestCoroutinesSurviveAcrossCalls(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()
	run(t, a, `co = coroutine.create(function() return 7 end)
 wrap = coroutine.wrap(function() coroutine.yield(1); return 2 end)
 assert(wrap() == 1)`)
	if got := run(t, a, `local ok,n = coroutine.resume(co); return ok,n,wrap()`); got != "true\n7\n2" {
		t.Fatal(got)
	}
}
