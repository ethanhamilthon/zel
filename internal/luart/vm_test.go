package luart

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	r := (&Runner{Env: env}).Run(ctx, "sleep(10)")
	if !r.IsError || !strings.Contains(r.Output, ECANCELED) {
		t.Fatalf("%q", r.Output)
	}
}

func TestFS(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
fs.write("a/b.txt", "one\ntwo\nthree\n")
fs.write("a/b.txt", "four\n", {append=true})
local out = {}
out[#out+1] = slice(fs.read("a/b.txt"), 2, 2) .. "\n"
out[#out+1] = fs.edit("a/b.txt", "two", "2")
out[#out+1] = select(2, fs.exists("a")) .. select(2, fs.exists("a/b.txt"))
local ok, e = pcall(fs.edit, "a/b.txt", "zzz", "x"); out[#out+1] = e
fs.write("a/c.txt", "x x")
ok, e = pcall(fs.edit, "a/c.txt", "x", "y"); out[#out+1] = e
out[#out+1] = fs.edit("a/c.txt", "x", "y", {all=true})
ok, e = pcall(fs.read, "nope"); out[#out+1] = e
out[#out+1] = json.encode(fs.glob("a/*.txt"))
out[#out+1] = json.encode(fs.list("a"))
out[#out+1] = fs.patch("a/c.txt", {{"y y", "z"}, {old = "z", new = "patched"}})
ok, e = pcall(fs.patch, "a/c.txt", {{"patched", "q"}, {"missing", "w"}}); out[#out+1] = e
return out`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	for _, want := range []string{`two\nthree\n`, `dirfile`, `fs.edit: ENOMATCH`, `fs.edit: EAMBIG`, `fs.read: ENOENT: nope`, `[\"a/b.txt\",\"a/c.txt\"]`, `\"name\":\"b.txt\"`, `fs.patch: ENOMATCH: edit #2`} {
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
	r := run(t, env, nil, `local g = fs.grep("Foo", {glob="*.go"}); return {#g, g[1].file, g[1].line}`)
	if r.Output != `[1,"src/a.go",2]` {
		t.Fatalf("%q", r.Output)
	}
	rows := goGrep(env.cwd, "Foo", "", false, 10)
	if len(rows) != 2 {
		t.Fatalf("%v", rows)
	}
	r = run(t, env, nil, `local p = fs.tmp("x.json"); fs.write(p, "1"); return p`)
	if !strings.HasPrefix(r.Output, env.cwd+"/tmp/") || !strings.HasSuffix(r.Output, "/x.json") {
		t.Fatalf("%q", r.Output)
	}
}

func TestState(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
state.set("a", {n=1, list={1,2}})
state.set("b", "x", "project")
state.set("c", 3)
state.set("c", nil)
return {state.get("a"), state.get("b", "project"), state.get("b"), state.list(), state.list("", "project")}`)
	if r.Output != `[{"list":[1,2],"n":1},"x",null,["a"],["b"]]` && !strings.Contains(r.Output, `"x"`) {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `local ok, e = pcall(state.set, "k", string.rep("x", 70000)); return e`)
	if !strings.Contains(r.Output, "state.set: ELIMIT") {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `local ok, e = pcall(state.set, "k", function() end); return e`)
	if !strings.Contains(r.Output, "state.set: EINVAL") {
		t.Fatalf("%q", r.Output)
	}
	r = run(t, env, nil, `local ok, e = pcall(state.get, "k", "world"); return e`)
	if !strings.Contains(r.Output, "EINVAL") {
		t.Fatalf("%q", r.Output)
	}
}

func TestJobNamespaces(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
local n = 41
local t = {x = {1, 2}}
t.self = t
local function inc(v) return v + 1 end
local id = tasks.spawn(function()
  print("hi from task")
  t.x[3] = 3; n = n + 100
  return {inc(n), #t.x, string.format("%d", 7), t.self == t, type(leaked)}
end)
leaked = 1
local res = tasks.wait(id, 5)
local slow = tasks.spawn(function() sleep(30) end, {name = "slow"})
local st = tasks.status(slow)
local r2, why = tasks.wait(slow, 0.05)
tasks.stop(slow)
local a = agents.run{prompt="hello", model="m"}
local aid = agents.spawn{prompt="p2"}
local rs = agents.wait_all({aid}, 1)
local ok, e = pcall(agents.status, id) -- a task id is not an agent
local co = coroutine.create(function() end)
local ok2, e2 = pcall(tasks.spawn, function() return co end)
return {type(id), res.value, res.out, n, #t.x, st, tostring(r2), why, tasks.result(slow).ok, a.answer, rs[1].answer, #tasks.list(), #agents.list(), e, e2, tasks.run == nil}`)
	want := `["string",[142,3,"7",true,"nil"],"hi from task\n",41,2,"running","nil","timeout",false,"re: hello","re: p2",2,2,"agents.status: ENOENT: no such job","tasks.spawn: EINVAL: cannot copy upvalue 'co' (a thread) into the task",true]`
	if r.Output != want {
		t.Fatalf("%q", r.Output)
	}
	if len(env.spawned) != 2 || env.spawned[0].Model != "m" {
		t.Fatalf("%+v", env.spawned)
	}
	r = run(t, env, nil, `local ok, e = pcall(agents.spawn, {prompt="boom"}); return e`)
	if !strings.Contains(r.Output, "agents.spawn: EFAIL: no budget") {
		t.Fatalf("%q", r.Output)
	}
	env.depth = 1
	r = run(t, env, nil, `local ok, e = pcall(agents.spawn, {prompt="x"}); return e`)
	if !strings.Contains(r.Output, "agents.spawn: EDENIED") {
		t.Fatalf("subagent spawned an agent: %q", r.Output)
	}
	if p := Builtin(1); strings.Contains(p, "agents.spawn") || strings.Contains(p, "agents.run") || !strings.Contains(p, "tasks.spawn(fn") {
		t.Fatalf("subagent prompt: %s", p)
	}
}

func TestSessionModels(t *testing.T) {
	env := newEnv(t)
	env.models = []ModelInfo{{Provider: "p", Model: "m1", ContextWindow: 8000}, {Provider: "p", Model: "m2"}}
	env.failures = map[string]string{"bad": "401 unauthorized"}
	r := run(t, env, nil, `local m, f = session.models(); return {#m, m[1].model, m[1].context_window, m[2].context_window, f.bad}`)
	if r.IsError || r.Output != `[2,"m1",8000,0,"401 unauthorized"]` {
		t.Fatalf("%q", r.Output)
	}
	// Without models or failures both values are still tables, so pairs(failures) is safe.
	env.models, env.failures = nil, nil
	r = run(t, env, nil, `local m, f = session.models(); local n = 0; for _ in pairs(f) do n = n + 1 end; return {#m, n}`)
	if r.IsError || r.Output != `[0,0]` {
		t.Fatalf("%q", r.Output)
	}
}

func TestBuiltinPromptNamesSessionFields(t *testing.T) {
	for _, depth := range []int{0, 1} {
		p := Builtin(depth)
		for _, want := range []string{
			"session.{id,cwd,root,parent,depth,model,effort}; session.rename(name)",
			"session.models() -> models, failures",
			"log.info/warn/notice(msg)",
			"stopped when the session changes (new/resume) or init.lua reloads",
		} {
			if !strings.Contains(p, want) {
				t.Fatalf("depth %d prompt misses %q", depth, want)
			}
		}
	}
	if !isBuiltin("session.models") {
		t.Fatal("session.models can be overridden by a plugin")
	}
}

func TestMiscHost(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `view("a.png", "b.png"); session.rename("name"); log.notice("hi"); log.warn("w"); return {session.id, session.model, json.decode('{"a":[1,2]}').a[2], json.encode({1}, {indent=2})}`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	if len(env.viewed) != 2 || env.viewed[0] != env.cwd+"/a.png" || env.renamed != "name" || env.notices[0] != "hi" || env.logs[0] != "warn: w" {
		t.Fatalf("%+v", env)
	}
	if !strings.HasPrefix(r.Output, `["s1","m",2,`) {
		t.Fatalf("%q", r.Output)
	}
}

func TestWeb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", r.Method)
		if r.URL.Path == "/missing" {
			w.WriteHeader(404)
		}
		_, _ = w.Write([]byte(r.Method + ":" + string(body)))
	}))
	defer srv.Close()
	env := newEnv(t)
	r := run(t, env, nil, `
local g = web.get("`+srv.URL+`/x")
local p = web.post("`+srv.URL+`/x", {json = {a = 1}})
local m = web.get("`+srv.URL+`/missing")
local bad = web.get("http://127.0.0.1:1/")
local q = web.request{url = "`+srv.URL+`/y", method = "PUT", body = "zz", headers = {["X-A"] = "b"}}
print(g.ok, g.status, g.body, p.body, m.ok, m.status, bad.ok, bad.status, q.body)`)
	if r.IsError || r.Output != "true\t200\tGET:\t"+`POST:{"a":1}`+"\tfalse\t404\tfalse\t0\tPUT:zz" {
		t.Fatalf("%q", r.Output)
	}
}

// The main agent's built-in prompt says what the config-side hooks do; subagents
// get neither that line nor the agents.* functions.
func TestBuiltinPromptLifecycleByDepth(t *testing.T) {
	main, sub := Builtin(0), Builtin(1)
	for _, want := range []string{"init.lua registers hooks and completions, not tool calls", "subagent_start and subagent_end run around each agents.* child", "#name in a user message arrives already expanded", "agents.spawn{prompt,model"} {
		if !strings.Contains(main, want) {
			t.Fatalf("main prompt misses %q", want)
		}
	}
	for _, unwanted := range []string{"subagent_start", "#name", "agents.spawn", "agents.run", "agents.* child"} {
		if strings.Contains(sub, unwanted) {
			t.Fatalf("subagent prompt mentions %q", unwanted)
		}
	}
	if !strings.Contains(sub, "agents.* is not available") {
		t.Fatalf("subagent prompt lost the agents restriction: %q", sub[max(0, len(sub)-200):])
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
		`return fs.read("pipe")`,
		`return fs.list("pipe")`,
		`return fs.write("pipe", "x")`,
		`return fs.edit("pipe", "a", "b")`,
		`return fs.patch("pipe", {{"a", "b"}})`,
		`return fs.grep("x", {path = "pipe"})`,
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
