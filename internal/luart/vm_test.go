package luart

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zel/internal/config"
	"zel/internal/defaults"
)

type fakeEnv struct {
	info SessionInfo
	log  chan string
}

func (e *fakeEnv) Info() SessionInfo { return e.info }
func (e *fakeEnv) ShellEnv() []string {
	return []string{"ZEL_SESSION_ID=" + e.info.ID, "TEST_ENV=base", "ZEL_MODE=main"}
}
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

func TestResultRule(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()
	for code, want := range map[string]string{
		`return 1, 2`:                    "1\n2",
		`return nil`:                     "",
		`return 1, nil, 3`:               "1\n3",
		`1 + 1`:                          "2",
		`"text"`:                         "text",
		`x = 4; return {z=2,a={3,4}}, x`: "{\"a\":[3,4],\"z\":2}\n4",
		`local function f() return "a", "b" end f()`: "a\nb",
		`y = 5`:                          "",
		`string.rep("ab", 2) -- comment`: "abab",
		`local t = {} ; t.x = 1`:         "",
		`return true, false`:             "true\nfalse",
	} {
		if got := run(t, a, code); got != want {
			t.Errorf("%q: got %q, want %q", code, got, want)
		}
	}
	r := a.Run(t.Context(), `local x = 1; error("broken", 0)`, time.Second)
	if r.OK || r.Out != "error: broken" {
		t.Fatalf("%+v", r)
	}
	if r := a.Run(t.Context(), `x = = 1`, time.Second); r.OK || !strings.HasPrefix(r.Out, "error: ") {
		t.Fatalf("%+v", r)
	}
	// Globals survive between calls; a reset starts fresh.
	if got := run(t, a, `y`); got != "5" {
		t.Fatal(got)
	}
	a.Reset()
	if got := run(t, a, `return y`); got != "[lua: fresh VM]\n" {
		t.Fatal(got)
	}
}

func TestLimitedREPLWithoutConfig(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()

	// Denied direct globals (privileged libs, host functions, introspection, loaders).
	got := run(t, a, `
local denied = {
  "os", "io", "debug", "require", "package", "module",
  "dofile", "loadfile", "getfenv", "setfenv", "print",
  "channel", "newproxy", "_printregs", "_GOPHER_LUA_VERSION",
  "sh", "read", "task", "session", "cd",
}
local out = {}
for _, name in ipairs(denied) do
  if _G[name] ~= nil or rawget(_G, name) ~= nil then
    out[#out+1] = name .. "=" .. type(_G[name])
  end
end
return #out, type(os), type(io), type(debug), type(require), type(package), type(getfenv), type(setfenv), type(loadfile), type(dofile), _G == _G._G, _VERSION`)
	if got != "0\nnil\nnil\nnil\nnil\nnil\nnil\nnil\nnil\nnil\ntrue\nLua 5.1" {
		t.Fatalf("denied globals leaked: %q", got)
	}

	// Useful pure Lua standard libs and session globals persistence.
	got = run(t, a, `
persisted = { nums = {3, 1, 2} }
table.sort(persisted.nums)
table.insert(persisted.nums, 4)
local mt = setmetatable({}, { __index = function(_, k) return "v:" .. k end })
local ok, err = pcall(error, "boom", 0)
local xok, xval = xpcall(function() error("xboom", 0) end, function(e) return "caught:" .. e end)
local sum = 0
for _, v in ipairs(persisted.nums) do sum = sum + v end
return string.upper("ok"), ("ab"):rep(2), math.floor(3.9), sum, mt.hi, ok, err, xok, xval`)
	if got != "OK\nabab\n3\n10\nv:hi\nfalse\nboom\nfalse\ncaught:xboom" {
		t.Fatalf("stdlib failed: %q", got)
	}
	if got := run(t, a, `return table.concat(persisted.nums, ",")`); got != "1,2,3,4" {
		t.Fatalf("persistence failed: %q", got)
	}

	// Indirect escapes via load / loadstring / metatables / coroutines are blocked.
	got = run(t, a, `
saved_G = _G
local f1 = assert(loadstring("loaded_val = 7; return os, io, debug, require, package, getfenv, setfenv, loadfile, dofile, _G == saved_G"))
local a, b, c, d, e, f, g, h, i, same1 = f1()
local parts = {"return os, _G == saved_G"}
local idx = 0
local f2 = assert(load(function() idx = idx + 1 return parts[idx] end))
local l_os, same2 = f2()
local sm_str = pcall(setmetatable, "", {})
local sm_num = pcall(setmetatable, 1, {})
local sm_fn = pcall(setmetatable, function() end, {})
local str_mt = getmetatable("")
local mut_str_mt = pcall(function() str_mt.upper = nil end)
return a, b, c, d, e, f, g, h, i, same1, l_os, same2, loaded_val, sm_str, sm_num, sm_fn, str_mt, mut_str_mt`)
	if got != "true\ntrue\n7\nfalse\nfalse\nfalse\nprotected\nfalse" {
		t.Fatalf("indirect escape check failed: %q", got)
	}

	// Coroutines work across calls and retain the allowlisted REPL environment.
	got = run(t, a, `
co = coroutine.create(function(start)
  local inner = assert(loadstring("return os, io, _G == saved_G"))
  local l_os, l_io, same = inner()
  local cur = coroutine.running()
  local sm_th = pcall(setmetatable, cur, {})
  local y1 = coroutine.yield(start + 1, type(os), type(io), type(debug), type(getfenv), _G == saved_G, l_os, l_io, same, type(cur), coroutine.status(cur), sm_th)
  co_persisted = y1 * 2
  return co_persisted, type(require), type(package)
end)
return coroutine.resume(co, 10)`)
	if got != "true\n11\nnil\nnil\nnil\nnil\ntrue\ntrue\nthread\nrunning\nfalse" {
		t.Fatalf("coroutine step 1: %q", got)
	}
	if got := run(t, a, `return coroutine.resume(co, 21)`); got != "true\n42\nnil\nnil" {
		t.Fatalf("coroutine step 2: %q", got)
	}
	if got := run(t, a, `
local w = coroutine.wrap(function() return type(os), type(getfenv), co_persisted end)
return w(), coroutine.status(co)`); got != "nil\ndead" {
		t.Fatalf("coroutine wrap/status: %q", got)
	}

	// Coroutines created in the REPL still observe call timeouts.
	r := a.Run(t.Context(), `local c = coroutine.create(function() while true do end end); return coroutine.resume(c)`, 200*time.Millisecond)
	if !r.TimedOut || !r.Reset {
		t.Fatalf("coroutine timeout did not reset VM: %+v", r)
	}
	if got := run(t, a, `return type(persisted), type(saved_G)`); got != "[lua: fresh VM]\nnil\nnil" {
		t.Fatalf("reset did not clear REPL globals: %q", got)
	}
}

func TestConfiguredFunctionsRetainPrivateLibraryEnv(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `
local open = io.open
local getenv = os.getenv
local my_os = os
local my_io = io
local custom_env_fn = function()
  return getenv("TEST_ENV"), my_os.getenv("ZEL_SESSION_ID"), type(io), type(os)
end
setfenv(custom_env_fn, setmetatable({ tag = "custom" }, { __index = _G }))
return { functions = {
  write_via_alias = { description = "write", fn = function(name, text)
    local f = assert(open(name, "w"))
    f:write(text)
    f:close()
    return my_io.open(name):read("*a")
  end },
  check_trusted = { description = "check", fn = function()
    local a, b, c, d = custom_env_fn()
    local t = {"a", "b"}
    table.insert(t, "c")
    return a, b, c, d, string.upper("ok"), ("hi"):upper(), table.concat(t, "-"), math.floor(4.7)
  end },
  check_sink = { description = "sink", fn = function()
    io.write("x"); io.stdout:write("y"); io.stderr:write("z")
    return io.read("*a"), io.type(io.stdout), io.stdin:read("*l")
  end },
  check_getenv = { description = "getenv", fn = function(...)
    local out = {}
    for i = 1, select("#", ...) do
      out[i] = os.getenv(select(i, ...))
    end
    return unpack(out, 1, select("#", ...))
  end },
  check_co = { description = "co", fn = function()
    local c = coroutine.create(function() return os.getenv("TEST_ENV") end)
    local ok, val = coroutine.resume(c)
    return ok, val
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()

	// REPL still cannot access os/io/getfenv directly, while configured aliases and closures work.
	if got := run(t, a, `return type(os), type(io), type(getfenv), write_via_alias("alias.txt", "hello")`); got != "nil\nnil\nnil\nhello" {
		t.Fatalf("%q", got)
	}
	if got := run(t, a, `return check_sink()`); got != "\nfile" {
		t.Fatalf("sink: %q", got)
	}
	if got := run(t, a, `return check_getenv("TEST_ENV", "ZEL_SESSION_ID", "NOPE_NOT_SET")`); got != "base\ns1" {
		t.Fatalf("getenv: %q", got)
	}

	// Mutating REPL stdlib tables does not mutate the trusted stdlib tables used by config functions.
	got := run(t, a, `
string.upper = function() return "REPL" end
table.concat = function() return "REPL" end
table.insert = function() end
math.floor = function() return -1 end
coroutine.resume = function() return false, "REPL" end
local co_ok, co_val = check_co()
return string.upper("ok"), ("hi"):upper(), co_ok, co_val, check_trusted()`)
	if got != "REPL\nHI\ntrue\nbase\nbase\ns1\ntable\ntable\nOK\nHI\na-b-c\n4" {
		t.Fatalf("trusted stdlib mutated from REPL: %q", got)
	}
}

func TestDefaultFunctionsInIsolatedREPL(t *testing.T) {
	env := newEnv(t)
	home := filepath.Join(t.TempDir(), "home")
	for rel, content := range defaults.Files() {
		fixture(t, filepath.Join(home, "defaults"), rel, content)
	}
	init := fixture(t, home, "init.lua", `
local d = require("defaults")
return { functions = {
  read = d.read,
  write = d.write,
  edit = d.edit,
  slice = d.slice,
  list = d.list,
  exists = d.exists,
  cd = d.cd,
} }`)
	paths := config.Paths{Files: []string{init}, Connect: filepath.Join(home, "connect.json")}
	c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	a := NewAgent(env, c)
	defer a.Close()
	got := run(t, a, `
write("sub/file.txt", "alpha\nbeta\ngamma\n")
cd("sub")
local n = edit("file.txt", "beta", "BETA")
local ok, kind = exists("file.txt")
local txt = read("file.txt")
local items = list(".")
cd()
return type(os), type(io), type(require), n, ok, kind, slice(txt, 2, 1), #items, items[1].name`)
	if got != "nil\nnil\nnil\n1\ntrue\nfile\nBETA\n\n1\nfile.txt" {
		t.Fatalf("defaults in isolated REPL: %q", got)
	}
}

func TestIOFiles(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `return { functions = {
  test_io = { description = "test io", fn = function()
    local f = assert(io.open("a.txt", "w"))
    f:write("one\ntwo\n", 3, "\n"):close()
    local out = {}
    for l in io.lines("a.txt") do out[#out+1] = l end
    local g = io.open("a.txt")
    local first, n = g:read("*l", "*n")
    local rest = g:read("*a")
    g:close()
    local missing, msg = io.open("nope/x")
    return table.concat(out, ","), first, n, #rest, io.type(g), missing, msg
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()
	got := run(t, a, `return test_io()`)
	if got != "one,two,3\none\n\n1\nclosed file\nnope/x: no such file or directory" && !strings.HasPrefix(got, "one,two,3\none\n") {
		t.Fatalf("%q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(env.info.Cwd, "a.txt")); string(data) != "one\ntwo\n3\n" {
		t.Fatalf("file: %q", data)
	}
}

func TestPopenAndExecuteAreHardened(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `return { functions = {
  test_popen = { description = "popen", fn = function()
    local p = io.popen('pwd; echo $ZEL_SESSION_ID $TEST_ENV; [ -t 0 ] && echo tty || echo notty; cat; echo $(ps -o pgid= -p $$ | tr -d " ") $$; echo err >&2')
    local out = p:read("*a")
    local ok, how, code = p:close()
    return out, ok, how, code
  end },
  test_exec = { description = "exec", fn = function()
    return os.execute("echo hidden; exit 3"), os.execute("true"), (io.popen("exit 5"):close())
  end },
  test_pipe_write = { description = "pipe", fn = function()
    local p = io.popen("cat > w.txt", "w")
    p:write("via pipe")
    local ok = p:close()
    return ok, io.open("w.txt"):read("*a")
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()
	// cwd is the session folder, the environment is the session's, no TTY,
	// stdin is /dev/null, the child leads its own process group.
	got := run(t, a, `return test_popen()`)
	lines := strings.Split(got, "\n")
	real, _ := filepath.EvalSymlinks(env.info.Cwd)
	ids := strings.Fields(lines[3])
	if len(lines) < 8 || lines[0] != real || lines[1] != "s1 base" || lines[2] != "notty" || len(ids) != 2 || ids[0] != ids[1] ||
		strings.Contains(got, "err\n") || lines[len(lines)-3] != "true" || lines[len(lines)-1] != "0" {
		t.Fatalf("%q", got)
	}
	if got := run(t, a, `return test_exec()`); got != "3\n0" {
		t.Fatalf("%q", got)
	}
	if got := run(t, a, `return test_pipe_write()`); got != "true\nvia pipe" {
		t.Fatalf("%q", got)
	}
}

func TestTimeoutKillsChildren(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `return { functions = {
  popen_read = { description = "popen", fn = function(cmd, mode)
    local p = io.popen(cmd)
    return p:read(mode)
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()
	pidFile := filepath.Join(env.info.Cwd, "pid")
	start := time.Now()
	r := a.Run(t.Context(), `return popen_read("echo $$ > pid; exec sleep 30", "*a")`, 400*time.Millisecond)
	if !r.TimedOut || !r.Reset || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d is still alive", pid)
		}
	}
	// Ending the VM kills what is left of a normal call, too.
	if got := run(t, a, `return popen_read("echo $$ > pid2; echo ready; exec sleep 30", "*l")`); got != "[lua: fresh VM]\nready" {
		t.Fatal(got)
	}
	a.Reset()
	data, _ = os.ReadFile(filepath.Join(env.info.Cwd, "pid2"))
	pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d survived the VM", pid)
		}
	}
}

// loadFixture loads one config file and returns the config.
func loadFixture(t *testing.T, env Env, files ...string) *Config {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	paths := config.Paths{Files: files, Connect: filepath.Join(home, "connect.json")}
	c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestFunctionsAreCopiedInOnePass(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `
local n = 0
local function bump() n = n + 1 return n end
local lib = {calls = 0}
return {functions = {
  inc = {description = "inc", fn = function() lib.calls = lib.calls + 1 return bump() end},
  get = {description = "get", fn = function() return n, lib.calls, string.upper("ok") end},
}}`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()
	// The two functions share their upvalues in the copy.
	if got := run(t, a, `inc(); inc(); return get()`); got != "2\n2\nOK" {
		t.Fatalf("%q", got)
	}
	// A new VM copies again from the untouched originals.
	a.Reset()
	if got := run(t, a, `return get()`); got != "[lua: fresh VM]\n0\n0\nOK" {
		t.Fatalf("%q", got)
	}
	a2 := NewAgent(env, c)
	defer a2.Close()
	if got := run(t, a2, `inc(); return get()`); got != "1\n1\nOK" {
		t.Fatalf("%q", got)
	}
}

func TestCopyErrorIsConfigError(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `
local handle = io.tmpfile()
return {functions = {f = {description = "f", fn = function() return handle end}}}`)
	_, err := LoadConfig(t.Context(), env, config.Paths{Files: []string{init}}, config.DefaultSettings())
	if err == nil || !strings.Contains(err.Error(), "functions.f") || !strings.Contains(err.Error(), "userdata") {
		t.Fatalf("got %v", err)
	}
}

func TestStringMetatableAndIndexIsolation(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `return { functions = {
  check_str = { description = "str", fn = function()
    return string.upper("ok"), ("hi"):upper(), ("ab"):rep(2)
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()

	// Direct value indexing, metatable reads, rawget/rawset, and mutation attempts
	// cannot reach or mutate the private string library or string method table.
	got := run(t, a, `
local direct_idx = ("").__index
local lit_idx = ("abc").__index
local mt_field = ("").__metatable
local tbl_idx = string.__index
local str_mt = getmetatable("")
local tbl_mt = getmetatable(string)
local mut_idx = pcall(function() ("").__index.upper = function() return "HACK" end end)
local mut_val = pcall(function() ("").upper = function() return "HACK" end end)
local set_mt = pcall(setmetatable, "", {})
local rg_idx = pcall(rawget, "", "__index")
local rg_up = pcall(rawget, "", "upper")
local rs_up = pcall(rawset, "", "upper", nil)
local loaded_idx = assert(loadstring('return ("").__index == nil'))()
local co_idx = coroutine.wrap(function() return ("").__index == nil end)()
string.upper = function() return "REPL" end
local m1, m2 = ("a,b"):match("(%a),(%a)")
return direct_idx == nil, lit_idx == nil, mt_field == nil, tbl_idx == nil, str_mt, tbl_mt == nil,
  mut_idx, mut_val, set_mt, rg_idx, rg_up, rs_up, loaded_idx, co_idx,
  string.upper("ok"), ("hi"):upper(), ("hello"):sub(2, 4), m1, m2,
  check_str()`)

	want := strings.Join([]string{
		"true", "true", "true", "true", "protected", "true",
		"false", "false", "false", "false", "false", "false", "true", "true",
		"REPL", "HI", "ell", "a", "b",
		"OK", "HI", "abab",
	}, "\n")
	if got != want {
		t.Fatalf("string isolation failed:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestConfiguredFunctionSiblingAndRecursiveCalls(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `
local state = { count = 0 }
return { functions = {
  bump = { description = "bump", fn = function(n)
    state.count = state.count + (n or 1)
    priv_total = (priv_total or 0) + (n or 1)
    return state.count, priv_total
  end },
  bump_twice = { description = "bump twice", fn = function()
    bump(1)
    return bump(2)
  end },
  write_file = { description = "write", fn = function(name, text)
    local f = assert(io.open(name, "w"))
    f:write(text)
    f:close()
    return true
  end },
  read_via_sibling = { description = "read", fn = function(name, text)
    assert(write_file(name, text .. ":" .. os.getenv("TEST_ENV")))
    return assert(io.open(name)):read("*a")
  end },
  fact = { description = "fact", fn = function(n)
    if n <= 1 then return 1 end
    return n * fact(n - 1)
  end },
  get_state = { description = "get_state", fn = function()
    return state.count, priv_total, type(os), type(io), os.getenv("ZEL_SESSION_ID")
  end },
} }`)
	c := loadFixture(t, env, init)
	a := NewAgent(env, c)
	defer a.Close()

	// Sibling calls, shared upvalue + private global state, recursive calls, and absent REPL introspection.
	got := run(t, a, `
local c, p = bump_twice()
local content = read_via_sibling("sib.txt", "hi")
local f5 = fact(5)
return c, p, content, f5, type(os), type(io), type(debug), type(getfenv), type(setfenv), type(priv_total)`)
	if got != "3\n3\nhi:base\n120\nnil\nnil\nnil\nnil\nnil\nnil" {
		t.Fatalf("sibling/recursive failed: %q", got)
	}

	// Shadowing functions or stdlib names in the REPL does not override trusted bindings.
	got = run(t, a, `
local orig_fact = fact
bump = function() return -999, -999 end
fact = function() return -1 end
os = { getenv = function() return "FAKE" end }
io = { open = function() error("fake io") end }
priv_total = 999
local repl_b1, repl_b2 = bump(1)
local repl_f = fact(5)
local sc, sp = bump_twice()
local trusted_f = orig_fact(4)
local gc, gp, tos, tio, sid = get_state()
local sib_content = read_via_sibling("sib2.txt", "ok")
return repl_b1, repl_b2, repl_f, sc, sp, trusted_f, gc, gp, tos, tio, sid, sib_content`)
	if got != "-999\n-999\n-1\n6\n6\n24\n6\n6\ntable\ntable\ns1\nok:base" {
		t.Fatalf("REPL shadowing overrode trusted binding: %q", got)
	}

	// Reset starts with a fresh copy of shared state.
	a.Reset()
	if got := run(t, a, `return bump_twice()`); got != "[lua: fresh VM]\n3\n3" {
		t.Fatalf("reset failed: %q", got)
	}
}
