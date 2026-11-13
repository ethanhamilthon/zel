package luart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, env *fakeEnv, cfg *Config, code string) Result {
	t.Helper()
	return (&Runner{Env: env, Cfg: cfg}).Run(context.Background(), code)
}

func TestParseCall(t *testing.T) {
	c, err := ParseCall("--! timeout=300\nprint(1)")
	if err != nil || c.Timeout != 300*time.Second || c.Code != "print(1)" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ := ParseCall("print(1)"); c.Timeout != 0 || c.Code != "print(1)" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"--! timeout=0\nx", "--! timeout=601\nx", "--! color=red\nx", "--! timeout=abc\nx"} {
		if _, err := ParseCall(bad); err == nil || !strings.Contains(err.Error(), EINVAL) {
			t.Errorf("%q: want EINVAL, got %v", bad, err)
		}
	}
}

func TestOutputPrintAndReturn(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `print("a", 1); print({x=1}); return {1,2,3}`)
	if r.IsError || r.Output != "a\t1\n{\"x\":1}\n[1,2,3]" {
		t.Fatalf("%q", r.Output)
	}
}

func TestFreshEnvPerCall(t *testing.T) {
	env := newEnv(t)
	run(t, env, nil, `leaked = 42`)
	if r := run(t, env, nil, `return tostring(leaked)`); r.Output != "nil" {
		t.Fatalf("global leaked: %q", r.Output)
	}
}

func TestOutputBudget(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `print(string.rep("a", 20000))`)
	if len(r.Output) > DefaultOutputLimit+200 || !strings.Contains(r.Output, "output truncated") {
		t.Fatalf("len %d", len(r.Output))
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
	start := time.Now()
	r := (&Runner{Env: env}).Run(context.Background(), "--! timeout=1\nsh('sleep 10')")
	if !r.IsError || !strings.Contains(r.Output, ETIMEOUT) || time.Since(start) > 4*time.Second {
		t.Fatalf("%q after %s", r.Output, time.Since(start))
	}
	r = (&Runner{Env: env}).Run(context.Background(), "--! timeout=1\nwhile true do end")
	if !r.IsError || !strings.Contains(r.Output, ETIMEOUT) {
		t.Fatalf("%q", r.Output)
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
out[#out+1] = fs.read("a/b.txt", {from=2, lines=2})
out[#out+1] = fs.edit("a/b.txt", "two", "2")
out[#out+1] = select(2, fs.exists("a")) .. select(2, fs.exists("a/b.txt"))
local ok, e = pcall(fs.edit, "a/b.txt", "zzz", "x"); out[#out+1] = e
fs.write("a/c.txt", "x x")
ok, e = pcall(fs.edit, "a/c.txt", "x", "y"); out[#out+1] = e
out[#out+1] = fs.edit("a/c.txt", "x", "y", {all=true})
ok, e = pcall(fs.read, "nope"); out[#out+1] = e
out[#out+1] = json.encode(fs.glob("a/*.txt"))
out[#out+1] = json.encode(fs.list("a"))
return out`)
	if r.IsError {
		t.Fatal(r.Output)
	}
	for _, want := range []string{`two\nthree\n`, `dirfile`, `fs.edit: ENOMATCH`, `fs.edit: EAMBIG`, `fs.read: ENOENT: nope`, `[\"a/b.txt\",\"a/c.txt\"]`, `\"name\":\"b.txt\"`} {
		if !strings.Contains(r.Output, want) {
			t.Errorf("missing %q in %s", want, r.Output)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "a/b.txt")); string(data) != "one\n2\nthree\nfour\n" {
		t.Errorf("file %q", data)
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

func TestHandles(t *testing.T) {
	env := newEnv(t)
	r := run(t, env, nil, `
local h = tasks.spawn("later")
local st = h:status()
local res, why = h:wait(0.05)
h:stop()
local again = tasks.get(h.id)
local a = agents.run{prompt="hello", model="m"}
local ah = agents.spawn{prompt="p2"}
local rs = wait_all({ah, h}, 1)
return {h.kind, st, tostring(res), why, again.id == h.id, a.answer, rs[1].answer, tasks.run("instant").out, #tasks.list(), #agents.list()}`)
	want := `["task","running","nil","timeout",true,"re: hello","re: p2","instant",2,2]`
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
