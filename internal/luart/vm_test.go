package luart

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, env *fakeEnv, cfg *Config, code string) Result {
	t.Helper()
	return (&Runner{Env: env, Cfg: cfg}).Run(context.Background(), code)
}

func TestOutputPrintAndReturn(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `print("a", 1); print({x=1}); return {1,2,3}`)
	if r.IsError || r.Output != "a\t1\n{\"x\":1}\n[1,2,3]" {
		t.Fatalf("%q", r.Output)
	}
}

func TestMainVMKeepsGlobals(t *testing.T) {
	env := newEnv(t)
	r := &Runner{Env: env}
	defer r.Close()
	ctx := context.Background()
	r.Run(ctx, `kept = 42; function twice(x) return 2 * x end`)
	if out := r.Run(ctx, `return twice(kept)`).Output; out != "84" {
		t.Fatalf("globals lost: %q", out)
	}
	r.Reset("init.lua was reloaded")
	out := r.Run(ctx, `return tostring(kept)`).Output
	if !strings.HasPrefix(out, "[lua: fresh VM, init.lua was reloaded") || !strings.HasSuffix(out, "\nnil") {
		t.Fatalf("reset: %q", out)
	}
	if out := r.Run(ctx, `return 1`).Output; out != "1" {
		t.Fatalf("reset note repeated: %q", out)
	}
}

func TestOutputBudget(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `print(string.rep("a", 20000))`)
	if len(r.Output) != 20000 {
		t.Fatalf("small output truncated: len %d", len(r.Output))
	}
	r = run(t, env, nil, `return string.rep("a", 2 * 1024 * 1024)`)
	if len(r.Output) > maxOutput+200 || !strings.Contains(r.Output, "output truncated") {
		t.Fatalf("len %d", len(r.Output))
	}
	r = run(t, env, nil, `local s = "1\n2\n3\n4\n5\n"; return slice(s, 2, 2) .. "|" .. slice(s, -2) .. "|" .. slice(s, 9)`)
	if r.Output != "2\n3|4\n5|" {
		t.Fatalf("slice: %q", r.Output)
	}
}

func TestErrorOutput(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `print("before"); error("bad thing")`)
	if !r.IsError || !strings.HasPrefix(r.Output, "before\nerror: ") || !strings.Contains(r.Output, "bad thing") {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `x = =`)
	if !r.IsError || !strings.HasPrefix(r.Output, "error: ") {
		t.Fatalf("%q", r.Output)
	}
}

func TestShAndErrors(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `local r = sh("echo hi; echo err >&2; exit 3"); return {r.ok, r.code, r.out}`)
	if r.Output != `[false,3,"hi\nerr\n"]` {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `local ok, e = pcall(sh, "false", {check=true}); return {ok, e}`)
	if !strings.Contains(r.Output, "sh: EFAIL: exit status 1") {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `local r = sh("sleep 5", {timeout=0.2}); return {r.ok, r.code, r.timed_out}`)
	if r.Output != `[false,124,true]` {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `return sh("pwd; echo $ZEL_SESSION_ID $X; cat", {env={X="y"}, stdin="in"}).out`)
	if !strings.Contains(r.Output, env.cwd) || !strings.Contains(r.Output, "s1 y") || !strings.HasSuffix(r.Output, "in") {
		t.Fatalf("%q", r.Output)
	}
}

func TestCallTimeout(t *testing.T) {
	env := newEnv(t)
	old := callTimeout
	callTimeout = time.Second
	defer func() { callTimeout = old }()
	start := time.Now()
	r := (&Runner{Env: env}).Run(context.Background(), "sh('sleep 10')")
	if !r.IsError || !strings.Contains(r.Output, ETIMEOUT) || time.Since(start) > 4*time.Second {
		t.Fatalf("%q after %s", r.Output, time.Since(start))
	}
	main := &Runner{Env: env}
	defer main.Close()
	main.Run(context.Background(), "x = 1")
	r = main.Run(context.Background(), "while true do end")
	if !r.IsError || !strings.Contains(r.Output, ETIMEOUT) {
		t.Fatalf("%q", r.Output)
	}
	// The VM is replaced after a timeout, and the next call says so.
	if out := main.Run(context.Background(), "return tostring(x)").Output; !strings.Contains(out, "timeout: globals and functions from earlier calls are gone]\nnil") {
		t.Fatalf("after timeout: %q", out)
	}
}

func TestCancel(t *testing.T) {
	env := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	r := (&Runner{Env: env}).Run(ctx, "while true do end")
	if !r.IsError || !strings.Contains(r.Output, ECANCELED) {
		t.Fatalf("%q", r.Output)
	}
}

func TestFS(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
write("a/b.txt", "one\ntwo\nthree\n")
write("a/b.txt", "four\n", {append=true})
local out = {}
out[#out+1] = slice(read("a/b.txt"), 2, 2) .. "\n"
out[#out+1] = edit("a/b.txt", "two", "2")
out[#out+1] = select(2, exists("a")) .. select(2, exists("a/b.txt"))
local ok, e = pcall(edit, "a/b.txt", "zzz", "x"); out[#out+1] = e
write("a/c.txt", "x x")
ok, e = pcall(edit, "a/c.txt", "x", "y"); out[#out+1] = e
out[#out+1] = edit("a/c.txt", "x", "y", {all=true})
ok, e = pcall(read, "nope"); out[#out+1] = e
out[#out+1] = glob("a/*.txt")
out[#out+1] = list("a")
out[#out+1] = patch("a/c.txt", {{"y y", "z"}, {old = "z", new = "patched"}})
ok, e = pcall(patch, "a/c.txt", {{"patched", "q"}, {"missing", "w"}}); out[#out+1] = e
return out`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	for _, want := range []string{`two\nthree\n`, `dirfile`, `edit: ENOMATCH`, `edit: EAMBIG`, `read: ENOENT: nope`, `["a/b.txt","a/c.txt"]`, `"name":"b.txt"`, `patch: ENOMATCH: edit #2`} {
		if !strings.Contains(r.Output, want) {
			t.Errorf("missing %q in %s", want, r.Output)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "a/b.txt")); string(data) != "one\n2\nthree\nfour\n" {
		t.Errorf("file %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "a/c.txt")); string(data) != "patched" {
		t.Errorf("patched file %q", data)
	}
}

func TestGrepAndTmp(t *testing.T) {
	env := newEnv(t)
	os.MkdirAll(filepath.Join(env.cwd, "src"), 0o755)
	os.WriteFile(filepath.Join(env.cwd, "src/a.go"), []byte("package a\nfunc Foo() {}\n"), 0o644)
	os.WriteFile(filepath.Join(env.cwd, "src/b.txt"), []byte("Foo here\n"), 0o644)
	r := run(t, env, nil, `local g = grep("Foo", {glob="*.go"}); return {#g, g[1].file, g[1].line}`)
	if r.Output != `[1,"src/a.go",2]` {
		t.Fatalf("%q", r.Output)
	}
	rows := goGrep(env.cwd, "Foo", "", false, 10)
	if len(rows) != 2 {
		t.Fatalf("%v", rows)
	}
	r = run(t, env, nil, `local p = tmp("x.json"); write(p, "1"); return p`)
	if !strings.HasPrefix(r.Output, env.cwd+"/tmp/") || !strings.HasSuffix(r.Output, "/x.json") {
		t.Fatalf("%q", r.Output)
	}
}

func TestTasksAndAgent(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
local n = 41
local t = {x = {1, 2}}
t.self = t
local function inc(v) return v + 1 end
local id = tasks.spawn("copy check", function()
  print("hi from task")
  t.x[3] = 3; n = n + 100
  return {inc(n), #t.x, string.format("%d", 7), t.self == t, type(leaked)}
end)
leaked = 1
local res = tasks.wait(id, 5)
local slow = tasks.spawn("slow", function() while true do end end)
local st = tasks.get(slow)
local r2, why = tasks.wait(slow, 0.05)
tasks.stop(slow)
local many = tasks.wait({id, slow}, 1)
local a = new_agent{prompt="hello", model="m"}
local co = coroutine.create(function() end)
local ok1, e1 = pcall(tasks.spawn, function() end)
local ok2, e2 = pcall(tasks.spawn, "co", function() return co end)
local ok3, e3 = pcall(tasks.get, nil)
return {type(id), res.value, res.out, n, #t.x, st.state, st.name, st.result == nil, tostring(r2), why, many[1].ok, tasks.get(id).result.ok, a.answer, #tasks.list(), e1, e2, tasks.status == nil, agents == nil}`)
	want := `["string",[142,3,"7",true,"nil"],"hi from task\n",41,2,"running","slow",true,"nil","timeout",true,true,"re: hello",2,"tasks.spawn: EINVAL: expected a name first: tasks.spawn(\"build checks\", function() ... end)","tasks.spawn: EINVAL: cannot copy upvalue 'co' (a thread) into the task",true,true]`
	if r.Output != want {
		t.Fatalf("%q", r.Output)
	}
	if len(env.spawned) != 1 || env.spawned[0].Model != "m" || !env.spawned[0].Waited {
		t.Fatalf("%+v", env.spawned)
	}
	r = run(t, env, nil, `local ok, e = pcall(new_agent, {prompt="boom"}); return e`)
	if !strings.Contains(r.Output, "new_agent: EFAIL: no budget") {
		t.Fatalf("%q", r.Output)
	}
	// send_agent goes to the subagent of that session and waits for its answer.
	env.spawned = nil
	r = run(t, env, nil, `local a = send_agent("sess1", "more", {timeout = 5}); local ok, e = pcall(send_agent, "unknown", "x"); local ok2, e2 = pcall(agent, {prompt = "x"}); return {a.answer, e, e2 == nil, agent == nil}`)
	if r.IsError || !strings.Contains(r.Output, `"re: more"`) || !strings.Contains(r.Output, "send_agent: ENOENT") {
		t.Fatalf("%q", r.Output)
	}
	if len(env.spawned) != 1 || env.spawned[0].Resume != "sess1" || env.spawned[0].Prompt != "more" || env.spawned[0].Timeout != 5*time.Second || !env.spawned[0].Waited {
		t.Fatalf("%+v", env.spawned)
	}
	env.mode = "agent"
	r = run(t, env, nil, `local ok, e = pcall(new_agent, {prompt="x"}); local ok2, e2 = pcall(send_agent, "s", "x"); return {e, e2}`)
	if !strings.Contains(r.Output, "new_agent: EDENIED") || !strings.Contains(r.Output, "send_agent: EDENIED") {
		t.Fatalf("subagent started an agent: %q", r.Output)
	}
}

func TestChoice(t *testing.T) {
	env := newEnv(t)
	env.pick = 1
	r := run(t, env, nil, `return choice("Какой цвет выбрать", {"Синий", "Красный"})`)
	if r.IsError || r.Output != "Красный" || strings.Join(env.choices, "|") != "Какой цвет выбрать|Синий|Красный" {
		t.Fatalf("%q %v", r.Output, env.choices)
	}
	env.pick = -1
	r = run(t, env, nil, `return {choice("q", {"a"}) == nil}`)
	if r.IsError || r.Output != "[true]" {
		t.Fatalf("cancel is nil: %q", r.Output)
	}
	r = run(t, env, nil, `local ok, e = pcall(choice, "q", {}); local ok2, e2 = pcall(choice, "q", {1}); local ok3, e3 = pcall(choice, "q", "a"); return {e, e2, ok3}`)
	if !strings.Contains(r.Output, "choice: EINVAL: no options") || !strings.Contains(r.Output, "choice: EINVAL: option #1") || !strings.Contains(r.Output, "false") {
		t.Fatalf("bad arguments: %q", r.Output)
	}
	for _, mode := range []string{"agent", "headless"} {
		env.mode = mode
		r = run(t, env, nil, `local ok, e = pcall(choice, "q", {"a"}); return e`)
		if !strings.Contains(r.Output, "choice: EDENIED") {
			t.Fatalf("%s: %q", mode, r.Output)
		}
	}
	env.mode = "main"
	r = run(t, env, nil, `local id = tasks.spawn("ask", function() return select(2, pcall(choice, "q", {"a"})) end); return tasks.wait(id, 5).value`)
	if !strings.Contains(r.Output, "choice: EDENIED") {
		t.Fatalf("task: %q", r.Output)
	}
}

// Waiting for the user does not use up the call's own time.
func TestChoiceDoesNotSpendTheCallTimeout(t *testing.T) {
	old := callTimeout
	callTimeout = 300 * time.Millisecond
	defer func() { callTimeout = old }()
	env := newEnv(t)
	env.pick = 0
	env.chooseDelay = 600 * time.Millisecond
	r := run(t, env, nil, `return choice("q", {"a", "b"})`)
	if r.IsError || r.Output != "a" {
		t.Fatalf("slow answer: %q", r.Output)
	}
	r = run(t, env, nil, `local t = os.clock(); while os.clock() - t < 1 do end; return 1`)
	if !r.IsError || !strings.Contains(r.Output, "timeout") {
		t.Fatalf("the budget still applies: %q", r.Output)
	}
}

func TestModels(t *testing.T) {
	env := newEnv(t)
	env.models = []ModelInfo{{Provider: "p", Model: "m1", ContextWindow: 8000}, {Provider: "p", Model: "m2"}}
	env.failures = map[string]string{"bad": "401 unauthorized"}
	r := run(t, env, nil, `local m, f = models(); return {#m, m[1].model, m[1].context_window, m[2].context_window, f.bad}`)
	if r.IsError || r.Output != `[2,"m1",8000,0,"401 unauthorized"]` {
		t.Fatalf("%q", r.Output)
	}
	// Without models or failures both values are still tables, so pairs(failures) is safe.
	env.models, env.failures = nil, nil
	r = run(t, env, nil, `local m, f = models(); local n = 0; for _ in pairs(f) do n = n + 1 end; return {#m, n}`)
	if r.IsError || r.Output != `[0,0]` {
		t.Fatalf("%q", r.Output)
	}
}

func TestMiscHost(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `view("a.png", "b.png"); session.rename("name"); log("w", 1); return {session.id, session.model, web == nil, json == nil, state == nil, sleep == nil}`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	if len(env.viewed) != 2 || env.viewed[0] != env.cwd+"/a.png" || env.renamed != "name" || env.logs[0] != "info: w 1" {
		t.Fatalf("%+v", env)
	}
	if r.Output != `["s1","m",true,true,true,true]` {
		t.Fatalf("%q", r.Output)
	}
}

// The prompt lists every host global; a subagent's prompt leaves agent{} out.
func TestBuiltinPromptByMode(t *testing.T) {
	main, sub, headless := Builtin("main"), Builtin("agent"), Builtin("headless")
	for name := range builtinNames {
		if !strings.Contains(main, name) {
			t.Errorf("main prompt misses %s", name)
		}
	}
	for _, want := range []string{"session.{id,cwd,root,parent,mode,model,effort}; session.rename(name)", "tasks.spawn(name, fn, o?)", "new_agent{prompt", "send_agent(id, text", "choice(name, options)", "#name in a user message arrives already expanded"} {
		if !strings.Contains(main, want) {
			t.Errorf("main prompt misses %q", want)
		}
	}
	if strings.Contains(sub, "new_agent{") || strings.Contains(sub, "send_agent(") || strings.Contains(sub, "choice(name") ||
		!strings.Contains(sub, "new_agent, send_agent and choice are not available") || !strings.Contains(sub, "tasks.spawn(name, fn") {
		t.Fatalf("subagent prompt: %q", sub)
	}
	if strings.Contains(headless, "choice(name") || !strings.Contains(headless, "new_agent{") || !strings.Contains(headless, "send_agent(") {
		t.Fatalf("headless prompt: %q", headless)
	}
	if !isBuiltin("session.models") || !isBuiltin("read") || isBuiltin("proj.read") {
		t.Fatal("isBuiltin")
	}
}

// mkfifo makes a named pipe for the tests of special files.
func mkfifo(t *testing.T, path string) string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// releasePipe gives a reader blocked on the named pipe its writer, then closes it.
// It must run on its own goroutine: the open waits for the reader to arrive.
func releasePipe(path string) {
	if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		w.Close()
	}
}

// awaitFreed waits until the Lua state of v is closed. A state is only closed by
// the last call that runs in it, so this proves the VM was not freed under a call.
func awaitFreed(t *testing.T, v *vm) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		v.mu.Lock()
		freed := v.freed
		v.mu.Unlock()
		if freed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the VM was never freed after its call returned")
		}
	}
}

// Named pipes and other special files are refused by the file functions: opening or
// reading one would wait for a peer that never comes, so the call fails at once.
func TestSpecialFilesAreRefusedNotWaitedFor(t *testing.T) {
	env := newEnv(t)
	mkfifo(t, filepath.Join(env.cwd, "pipe"))
	for _, code := range []string{
		`return read("pipe")`,
		`return list("pipe")`,
		`return write("pipe", "x")`,
		`return edit("pipe", "a", "b")`,
		`return patch("pipe", {{"a", "b"}})`,
		`return grep("x", {path = "pipe"})`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		res := (&Runner{Env: env}).Run(ctx, code)
		cancel()
		if !res.IsError || !strings.Contains(res.Output, "not a regular file") || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %q after %s", code, res.Output, time.Since(start))
		}
	}
}

// A call whose host operation never returns still ends at its deadline. Run
// returns, the VM is dropped so the next call starts clean, and the VM is freed
// once the blocked Lua returns.
func TestStuckCallReturnsAtDeadlineAndVMIsFreedLater(t *testing.T) {
	env := newEnv(t)
	fifo := mkfifo(t, filepath.Join(env.cwd, "stuck"))
	oldTimeout, oldGrace := callTimeout, abandonGrace
	callTimeout, abandonGrace = 300*time.Millisecond, 50*time.Millisecond
	defer func() { callTimeout, abandonGrace = oldTimeout, oldGrace }()
	main := &Runner{Env: env}
	defer main.Close()
	main.Run(context.Background(), "x = 1")
	parked := main.v
	start := time.Now()
	r := main.Run(context.Background(), `return io.open("`+fifo+`", "r"):read("a")`)
	if !r.IsError || !strings.Contains(r.Output, ETIMEOUT) || time.Since(start) > 5*time.Second {
		t.Fatalf("stuck call: %q after %s", r.Output, time.Since(start))
	}
	if out := main.Run(context.Background(), "return tostring(x)").Output; !strings.Contains(out, "timeout: globals and functions from earlier calls are gone]\nnil") {
		t.Fatalf("after a stuck call: %q", out)
	}
	go releasePipe(fifo)
	awaitFreed(t, parked)
}

// Cancelling a call that stays stuck returns at once as well.
func TestStuckCallCancelReturnsPromptly(t *testing.T) {
	env := newEnv(t)
	fifo := mkfifo(t, filepath.Join(env.cwd, "stuck"))
	oldGrace := abandonGrace
	abandonGrace = 50 * time.Millisecond
	defer func() { abandonGrace = oldGrace }()
	main := &Runner{Env: env}
	defer main.Close()
	main.Run(context.Background(), "x = 1")
	parked := main.v
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := main.Run(ctx, `return io.open("`+fifo+`", "r"):read("a")`)
	if took := time.Since(start); !r.IsError || !strings.Contains(r.Output, ECANCELED) || took > 3*time.Second {
		t.Fatalf("cancelled stuck call: %q after %s", r.Output, took)
	}
	// Reset of a VM whose call is still blocked does not wait either.
	main.Reset("test")
	go releasePipe(fifo)
	awaitFreed(t, parked)
}

// gatedHostEnv embeds fakeEnv to gate supported host operations (session.rename,
// view) and tracks the live foreground activity stack with exact end counts.
type gatedHostEnv struct {
	*fakeEnv
	renameEntered chan struct{}
	renameRelease chan struct{}
	viewEntered   chan struct{}
	viewRelease   chan struct{}

	stackMu  sync.Mutex
	seq      uint64
	live     []gatedAct
	endCalls map[uint64]int
}

type gatedAct struct {
	id    uint64
	label string
}

func newGatedHostEnv(t *testing.T) *gatedHostEnv {
	return &gatedHostEnv{
		fakeEnv:       newEnv(t),
		renameEntered: make(chan struct{}),
		renameRelease: make(chan struct{}),
		viewEntered:   make(chan struct{}),
		viewRelease:   make(chan struct{}),
		endCalls:      map[uint64]int{},
	}
}

func (e *gatedHostEnv) Begin(label string, background bool) func() {
	inner := e.fakeEnv.Begin(label, background)
	if background {
		return inner
	}
	e.stackMu.Lock()
	e.seq++
	id := e.seq
	e.live = append(e.live, gatedAct{id: id, label: label})
	e.stackMu.Unlock()
	return func() {
		inner()
		e.stackMu.Lock()
		e.endCalls[id]++
		e.live = slices.DeleteFunc(e.live, func(x gatedAct) bool { return x.id == id })
		e.stackMu.Unlock()
	}
}

func (e *gatedHostEnv) foreground() string {
	e.stackMu.Lock()
	defer e.stackMu.Unlock()
	if len(e.live) == 0 {
		return ""
	}
	return e.live[len(e.live)-1].label
}

func (e *gatedHostEnv) Rename(name string) error {
	close(e.renameEntered)
	<-e.renameRelease
	return e.fakeEnv.Rename(name)
}

func (e *gatedHostEnv) View(path string) error {
	close(e.viewEntered)
	<-e.viewRelease
	return e.fakeEnv.View(path)
}

// Abandoning a stuck supported host call clears every foreground activity it
// started as soon as Run returns, so later calls see no stale label and the
// eventual host return neither removes another call's activity nor double-closes.
func TestAbandonedHostCallClearsForegroundActivity(t *testing.T) {
	env := newGatedHostEnv(t)
	oldGrace := abandonGrace
	abandonGrace = 50 * time.Millisecond
	defer func() { abandonGrace = oldGrace }()

	main := &Runner{Env: env}
	defer main.Close()
	main.Run(context.Background(), "x = 1")
	parked := main.v

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		done <- main.Run(ctx, `session.rename("stuck")`)
	}()

	select {
	case <-env.renameEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("session.rename was never entered")
	}
	if got := env.foreground(); got != "renaming session" {
		t.Fatalf("foreground while blocked = %q, want %q", got, "renaming session")
	}
	cancel()

	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Output, ECANCELED) {
			t.Fatalf("abandoned call = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
	if got := env.foreground(); got != "" {
		t.Fatalf("foreground activity still live right after abandoned Run returned: %q", got)
	}

	next := main.Run(context.Background(), "return 42")
	if next.IsError || !strings.HasSuffix(next.Output, "\n42") {
		t.Fatalf("subsequent call = %+v", next)
	}
	if got := env.foreground(); got != "" {
		t.Fatalf("foreground activity still live after subsequent call returned: %q", got)
	}

	// Hold a new foreground activity on call 3 while releasing the abandoned call 1:
	// call 1's late return must neither clear call 3's activity nor double-close.
	call3 := make(chan Result, 1)
	go func() {
		call3 <- main.Run(context.Background(), `view("next.png")`)
	}()
	select {
	case <-env.viewEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("view was never entered")
	}
	if got := env.foreground(); got != "viewing next.png" {
		t.Fatalf("foreground during call 3 = %q", got)
	}

	close(env.renameRelease)
	awaitFreed(t, parked)
	if got := env.foreground(); got != "viewing next.png" {
		t.Fatalf("late return of abandoned call disturbed call 3's activity: %q", got)
	}

	close(env.viewRelease)
	select {
	case res := <-call3:
		if res.IsError {
			t.Fatalf("call 3 failed: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call 3 did not finish")
	}
	if got := env.foreground(); got != "" {
		t.Fatalf("foreground after call 3 = %q", got)
	}
	env.stackMu.Lock()
	defer env.stackMu.Unlock()
	for id, count := range env.endCalls {
		if count != 1 {
			t.Errorf("activity %d ended %d times, want 1", id, count)
		}
	}
}

// docs() lists the topics, returns one of them and rejects an unknown name with the list.
func TestDocs(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `local idx = docs(); local ok, err = pcall(docs, "nope"); return {idx:find('docs("hooks")', 1, true) ~= nil, docs("hooks"):find("turn_end", 1, true) ~= nil, tostring(err):find("EINVAL", 1, true) ~= nil, tostring(err):find('docs("init")', 1, true) ~= nil}`)
	if r.IsError || r.Output != `[true,true,true,true]` {
		t.Fatalf("%q", r.Output)
	}
}
