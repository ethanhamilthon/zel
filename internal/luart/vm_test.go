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

func TestNoHostAPI(t *testing.T) {
	a := NewAgent(newEnv(t), nil)
	defer a.Close()
	got := run(t, a, `return type(print), type(sh), type(read), type(task), type(session), type(cd), type(os.exit), (pcall(require, "zel"))`)
	if got != "nil\nnil\nnil\nnil\nnil\nnil\nnil\nfalse" {
		t.Fatal(got)
	}
	// Nothing reaches the terminal: output goes to a sink, stdin is empty.
	got = run(t, a, `io.write("x"); io.stdout:write("y"); io.stderr:write("z"); return io.read("*a"), io.type(io.stdout), io.stdin:read("*l")`)
	if got != "\nfile" {
		t.Fatalf("%q", got)
	}
}

func TestIOFiles(t *testing.T) {
	env := newEnv(t)
	a := NewAgent(env, nil)
	defer a.Close()
	got := run(t, a, `
local f = assert(io.open("a.txt", "w"))
f:write("one\ntwo\n", 3, "\n"):close()
local out = {}
for l in io.lines("a.txt") do out[#out+1] = l end
local g = io.open("a.txt")
local first, n = g:read("*l", "*n")
local rest = g:read("*a")
g:close()
local missing, msg = io.open("nope/x")
return table.concat(out, ","), first, n, #rest, io.type(g), missing, msg`)
	if got != "one,two,3\none\n\n1\nclosed file\nnope/x: no such file or directory" && !strings.HasPrefix(got, "one,two,3\none\n") {
		t.Fatalf("%q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(env.info.Cwd, "a.txt")); string(data) != "one\ntwo\n3\n" {
		t.Fatalf("file: %q", data)
	}
}

func TestPopenAndExecuteAreHardened(t *testing.T) {
	env := newEnv(t)
	a := NewAgent(env, nil)
	defer a.Close()
	// cwd is the session folder, the environment is the session's, no TTY,
	// stdin is /dev/null, the child leads its own process group.
	got := run(t, a, `
local p = io.popen('pwd; echo $ZEL_SESSION_ID $TEST_ENV; [ -t 0 ] && echo tty || echo notty; cat; echo $(ps -o pgid= -p $$ | tr -d " ") $$; echo err >&2')
local out = p:read("*a")
local ok, how, code = p:close()
return out, ok, how, code`)
	lines := strings.Split(got, "\n")
	real, _ := filepath.EvalSymlinks(env.info.Cwd)
	ids := strings.Fields(lines[3])
	if len(lines) < 8 || lines[0] != real || lines[1] != "s1 base" || lines[2] != "notty" || len(ids) != 2 || ids[0] != ids[1] ||
		strings.Contains(got, "err\n") || lines[len(lines)-3] != "true" || lines[len(lines)-1] != "0" {
		t.Fatalf("%q", got)
	}
	if got := run(t, a, `return os.execute("echo hidden; exit 3"), os.execute("true"), (io.popen("exit 5"):close())`); got != "3\n0" {
		t.Fatalf("%q", got)
	}
	if got := run(t, a, `local p = io.popen("cat > w.txt", "w"); p:write("via pipe"); local ok = p:close(); return ok, io.open("w.txt"):read("*a")`); got != "true\nvia pipe" {
		t.Fatalf("%q", got)
	}
}

func TestTimeoutKillsChildren(t *testing.T) {
	env := newEnv(t)
	a := NewAgent(env, nil)
	defer a.Close()
	pidFile := filepath.Join(env.info.Cwd, "pid")
	start := time.Now()
	r := a.Run(t.Context(), `local p = io.popen("echo $$ > pid; exec sleep 30"); return p:read("*a")`, 400*time.Millisecond)
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
	if got := run(t, a, `local p = io.popen("echo $$ > pid2; echo ready; exec sleep 30"); return p:read("*l")`); got != "[lua: fresh VM]\nready" {
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
