package luart

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zel/internal/config"
)

func fixture(t *testing.T, root, name, code string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(code), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMultiFileMerge(t *testing.T) {
	env := newEnv(t)
	root := env.info.Root
	home := filepath.Join(root, "home")
	fixture(t, home, "defaults/defaults.lua", `
local M = {}
M.greet = setmetatable({description = "`+"`greet(name)`"+` - says hi", fn = function(n) return "hi " .. n end},
  {__call = function(self, ...) return self.fn(...) end})
return M`)
	fixture(t, home, "helper.lua", `return {suffix = "!"}`)
	init := fixture(t, home, "init.lua", `
local d = require("defaults")
local helper = require("helper")
return {
 settings = {timeout = 12, env = {A = "a", SHARED = "init"}, keys = {menu = "ctrl+k"}},
 systems = {b = "B", a = "A"},
 prompts = {review = "old"},
 functions = {greet = d.greet, over = {description = "old", fn = function() return "old" end}},
 hooks = {{events = {"message"}, fn = function(e, data) return data.text .. helper.suffix end}},
 commands = {{name = "Commit", description = "commit", key = "alt+g", fn = function() return d.greet("x") end}},
 loads = {"../conf/layers/extra.lua"},
}`)
	fixture(t, root, "conf/layers/extra.lua", `return {systems = {a = "A2", d = "D"}, settings = {env = {B = "b"}}}`)
	// The default zel.lua of the start folder does not exist: skipped.
	local := filepath.Join(root, "zel.lua")
	extra := fixture(t, root, "b.lua", `
return {systems = {c = "C"}, settings = {timeout = 17, env = {SHARED = "b"}, show_prompt = true}, prompts = {review = "new"},
 functions = {over = {description = "new", fn = function() return "new" end}, pkgpath = {description = "pkg", fn = function() return package.path end}},
 hooks = {{events = {"message", "idle"}, async = true, fn = function() print("async ran") end}},
 commands = {{name = "Commit", description = "commit v2", fn = function() end}, {name = "Other", key = "ctrl+o", fn = function() end}}}`)
	env.log = make(chan string, 4)
	paths := config.Paths{Files: []string{init, local, extra}, Required: []string{extra}, Connect: filepath.Join(home, "connect.json")}
	c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d := c.Data()
	if d.Settings.Timeout != 17 || !d.Settings.ShowPrompt || !reflect.DeepEqual(d.Settings.Env, map[string]string{"A": "a", "B": "b", "SHARED": "b"}) {
		t.Fatalf("settings: %+v", d.Settings)
	}
	if want := []string{init, filepath.Join(root, "conf/layers/extra.lua"), extra}; !reflect.DeepEqual(d.Files, want) {
		t.Fatalf("files %v", d.Files)
	}
	sys := map[string]string{}
	for _, s := range d.Systems {
		sys[s.Name] = s.Text + "@" + filepath.Base(s.Source)
	}
	if !reflect.DeepEqual(sys, map[string]string{"a": "A2@extra.lua", "b": "B@init.lua", "c": "C@b.lua", "d": "D@extra.lua"}) {
		t.Fatalf("systems %v", sys)
	}
	if d.Prompts["review"] != "new" || filepath.Base(d.PromptSources["review"]) != "b.lua" {
		t.Fatalf("prompts %v %v", d.Prompts, d.PromptSources)
	}
	var fns []string
	for _, f := range d.Functions {
		fns = append(fns, f.Name+":"+f.Description+"@"+filepath.Base(f.Source))
	}
	if !reflect.DeepEqual(fns, []string{"greet:`greet(name)` - says hi@init.lua", "over:new@b.lua", "pkgpath:pkg@b.lua"}) {
		t.Fatalf("functions %v", fns)
	}
	if len(d.Warnings) != 6 || !strings.Contains(d.Warnings[0], "systems.a replaces a definition from") || !strings.Contains(strings.Join(d.Warnings, "\n"), "functions.over replaces a definition from "+init) ||
		!strings.Contains(strings.Join(d.Warnings, "\n"), "commands.Commit replaces") {
		t.Fatalf("warnings %q", d.Warnings)
	}
	for _, setting := range []string{"timeout", "env.SHARED"} {
		want := extra + ": settings." + setting + " replaces a definition from " + init
		if d.SettingSources[setting] != extra || !strings.Contains(strings.Join(d.Warnings, "\n"), want) {
			t.Fatalf("missing provenance for %s: %v, %q", setting, d.SettingSources, d.Warnings)
		}
	}
	if d.SettingSources["env.A"] != init {
		t.Fatalf("non-overwritten setting lost its provenance: %v", d.SettingSources)
	}
	if len(d.Commands) != 2 || d.Commands[0].Name != "Commit" || d.Commands[0].Description != "commit v2" || d.Commands[0].Key != "" || d.Commands[1].Key != "ctrl+o" {
		t.Fatalf("commands %+v", d.Commands)
	}
	// Hooks append in file order.
	if len(d.Hooks) != 2 || d.Hooks[0].Async || !d.Hooks[1].Async || !strings.HasPrefix(d.Hooks[1].Source, extra+":") {
		t.Fatalf("hooks %+v", d.Hooks)
	}
	res, err := c.RunHook(t.Context(), d.Hooks[0], "message", map[string]any{"text": "hey"})
	if err != nil || !res.HasText || res.Text != "hey!" {
		t.Fatalf("sync hook: %+v %v", res, err)
	}
	if _, err := c.RunHook(t.Context(), d.Hooks[1], "idle", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-env.log:
		if msg != "async ran" {
			t.Fatal(msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("async hook did not run")
	}
	// The private package.path of the session VM has the defaults folder first,
	// while package itself is hidden from the agent REPL.
	a := NewAgent(env, c)
	defer a.Close()
	if got := run(t, a, `return type(package), pkgpath():match("^[^;]*;[^;]*;"), greet("bob")`); !strings.HasPrefix(got, "nil\n"+filepath.Join(home, "defaults", "?.lua")+";"+filepath.Join(home, "defaults", "?", "init.lua")+";") || !strings.HasSuffix(got, "hi bob") {
		t.Fatalf("%q", got)
	}
}

func TestCommands(t *testing.T) {
	env := newEnv(t)
	init := fixture(t, env.info.Root, "init.lua", `
local n = 0
return {commands = {
  {name = "Say", fn = function() n = n + 1 return "said " .. n .. "\n" end},
  {name = "Fail", fn = function() error("boom", 0) end},
  {name = "Quiet", fn = function() return 42 end},
}}`)
	c := loadFixture(t, env, init)
	cmds := c.Data().Commands
	for i, want := range []struct{ out, err string }{{"said 1", ""}, {"", "boom"}, {"", ""}} {
		out, err := c.RunCommand(t.Context(), cmds[i])
		if out != want.out || (err == nil) != (want.err == "") || (err != nil && err.Error() != want.err) {
			t.Fatalf("%s: %q %v", cmds[i].Name, out, err)
		}
	}
	// Every run is a fresh VM with its own copy: n starts again from the original.
	if out, _ := c.RunCommand(t.Context(), cmds[0]); out != "said 1" {
		t.Fatalf("second run: %q", out)
	}
}

func TestConfigErrors(t *testing.T) {
	root := t.TempDir()
	for name, tt := range map[string]struct{ code, want string }{
		"unknown key":   {`return {tools = {}}`, `unknown key "tools"`},
		"not a table":   {`return 1`, "expected a returned table"},
		"bad function":  {`return {functions = {f = {description = "d", fn = function() end, x = 1}}}`, `unknown key "x"`},
		"go function":   {`return {functions = {f = {description = "d", fn = print}}}`, "fn must be a Lua function"},
		"builtin":       {`return {functions = {string = {description = "d", fn = function() end}}}`, "collides with a built-in"},
		"bad key":       {`return {commands = {{name = "A", key = "f5", fn = function() end}}}`, "ctrl+LETTER"},
		"key clash":     {`return {commands = {{name = "A", key = "ctrl+k", fn = function() end}}}`, "already used by keys.menu"},
		"command clash": {`return {commands = {{name = "A", key = "alt+a", fn = function() end}, {name = "B", key = "alt+a", fn = function() end}}}`, "already used by commands.A"},
		"command no fn": {`return {commands = {{name = "A"}}}`, "fn must be a Lua function"},
		"nested loads":  {`return {loads = {"nested.lua"}}`, "loads are not allowed"},
		"unknown event": {`return {hooks = {{events = {"nope"}, fn = function() end}}}`, `unknown event "nope"`},
		"runtime error": {`error("oops", 0)`, "oops"},
	} {
		file := fixture(t, root, strings.ReplaceAll(name, " ", "_")+".lua", tt.code)
		fixture(t, root, "nested.lua", `return {loads = {"x.lua"}}`)
		_, err := LoadConfig(t.Context(), newEnv(t), config.Paths{Files: []string{file}}, config.DefaultSettings())
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", name, err, tt.want)
		}
	}
	// A missing default file is skipped; a missing required one is an error.
	missing := filepath.Join(root, "missing.lua")
	if c, err := LoadConfig(t.Context(), newEnv(t), config.Paths{Files: []string{missing}}, config.DefaultSettings()); err != nil || len(c.Data().Files) != 0 {
		t.Fatalf("missing default: %v", err)
	} else {
		c.Close()
	}
	if _, err := LoadConfig(t.Context(), newEnv(t), config.Paths{Files: []string{missing}, Required: []string{missing}}, config.DefaultSettings()); err == nil {
		t.Fatal("a missing required file loaded")
	}
}

func TestHooksAndCommandsKeepFullStdlib(t *testing.T) {
	env := newEnv(t)
	env.log = make(chan string, 4)
	fixture(t, env.info.Root, "helper_mod.lua", `return {tag = "loaded-ok"}`)
	init := fixture(t, env.info.Root, "init.lua", `
local mod = require("helper_mod")
local function check_env()
  return table.concat({
    type(os), type(io), type(debug), type(package), type(require),
    type(getfenv), type(setfenv), type(loadfile), type(dofile),
    os.getenv("ZEL_SESSION_ID") or "", mod.tag,
  }, ",")
end
return {
  hooks = {
    {events = {"message"}, fn = function() return check_env() end},
    {events = {"idle"}, async = true, fn = function()
      local f = assert(io.open("async_hook.txt", "w"))
      f:write(check_env())
      f:close()
      print("async:" .. mod.tag)
    end},
  },
  commands = {
    {name = "Check", fn = function()
      local p = assert(io.popen("echo cmd-ok"))
      local out = p:read("*a"):gsub("%s+$", "")
      p:close()
      return out .. ":" .. check_env()
    end},
  },
}`)
	c := loadFixture(t, env, init)
	want := "table,table,table,table,function,function,function,function,function,s1,loaded-ok"

	res, err := c.RunHook(t.Context(), c.Data().Hooks[0], "message", map[string]any{"text": "hi"})
	if err != nil || !res.HasText || res.Text != want {
		t.Fatalf("sync hook: %+v %v", res, err)
	}
	if _, err := c.RunHook(t.Context(), c.Data().Hooks[1], "idle", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-env.log:
		if msg != "async:loaded-ok" {
			t.Fatalf("async log: %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("async hook did not log")
	}
	data, err := os.ReadFile(filepath.Join(env.info.Cwd, "async_hook.txt"))
	if err != nil || string(data) != want {
		t.Fatalf("async hook file: %q %v", data, err)
	}
	out, err := c.RunCommand(t.Context(), c.Data().Commands[0])
	if err != nil || out != "cmd-ok:"+want {
		t.Fatalf("command: %q %v", out, err)
	}
}
