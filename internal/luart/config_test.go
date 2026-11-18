package luart

import (
	"context"
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

func TestConfigLoadsAndCalls(t *testing.T) {
	env := newEnv(t)
	env.tasks = &fakeTasks{}
	root := env.info.Root
	helper := fixture(t, root, "conf/helper.lua", `return {suffix="!"}`)
	init := fixture(t, root, "conf/init.lua", `
local zel = require("zel")
local helper = require("helper")
local shared = 0
assert(sh == nil and session == nil and zel.session.id == "s1")
return {
 settings = {timeout=12, env={A="a", SHARED="init"}, colors={accent="#111111",trace={lua="#111111"}}},
 systems = {b="B", a="A"},
 prompts = {review="old"},
 functions = {
  count = {description="example", fn=function(a)
   shared = shared + 1
   a.x = "copied"
   return a, shared, helper.suffix
  end},
  overwritten = {description="old", fn=function(a) return "old" end},
  bad = {description="bad", fn=function(a) return function() end end},
 },
 hooks = {{events={"message","lua_before"},fn=function(event,data)
  if event == "lua_before" then return false, data.code end
  return data.text..helper.suffix
 end}},
 loads = {"layers/extra.lua"},
}`)
	extra := fixture(t, root, "conf/layers/extra.lua", `
local zel = require("zel")
return {systems={a="A2", d=zel.sh('printf "%s" "$A"').out}, settings={env={B="b"}, colors={trace={system="#333333"}}}}`)
	local := fixture(t, root, "zel.lua", `
return {systems={c="C"}, settings={env={SHARED="local"},show_prompt=true},prompts={review="new"},
 functions={overwritten={description="new",fn=function(a) return "new" end}}}`)
	paths := config.Paths{Init: init, Local: local, Connect: filepath.Join(root, "connect.json")}
	c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d := c.Data()
	if d.Settings.Timeout != 12 || !d.Settings.ShowPrompt || d.Settings.Colors.Get("accent") != "#111111" || d.Settings.Colors.Get("trace.system") != "#333333" || !reflect.DeepEqual(d.Settings.Env, map[string]string{"A": "a", "B": "b", "SHARED": "local"}) {
		t.Fatalf("settings: %+v", d.Settings)
	}
	want := []config.System{{Name: "b", Text: "B"}, {Name: "a", Text: "A2"}, {Name: "d", Text: "a"}, {Name: "c", Text: "C"}}
	if !reflect.DeepEqual(d.Systems, want) || d.Prompts["review"] != "new" || len(d.Warnings) != 3 {
		t.Fatalf("data: %+v", d)
	}
	if !reflect.DeepEqual(d.Files, []string{paths.Connect, init, helper, extra, local}) {
		t.Fatalf("watched files: %v", d.Files)
	}
	if !strings.Contains(ToolDescription(true, 12, d.Functions), "overwritten{...}: new") {
		t.Fatal("missing function description")
	}
	a := NewAgent(env, c)
	defer a.Close()
	got := run(t, a, `
 local a = {x="original"}; a.self = a
 local r,n,suffix = count(a)
 assert(a.x == "original" and r.x == "copied" and r.self == r and suffix == "!")
 local ok, err = pcall(count,{f=function() end})
 assert(not ok and err:match("^count: EINVAL:"))
 assert(not pcall(bad,{}))
 local id = task.spawn("call",function() local _,n = count({}); return n end)
 return n, task.wait(id), overwritten{}`)
	if got != "1\n2\nnew" {
		t.Fatal(got)
	}
	h := d.Hooks[0]
	result, err := c.RunHook(t.Context(), h, "message", map[string]any{"text": "hello"})
	if err != nil || result != (HookResult{Text: "hello!", HasText: true}) {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = c.RunHook(t.Context(), h, "lua_before", map[string]any{"code": "test"})
	if err != nil || result != (HookResult{Blocked: true, Reason: "test"}) {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestConfigInvalidAndAsync(t *testing.T) {
	env := newEnv(t)
	paths := config.Paths{Init: filepath.Join(env.info.Root, "init.lua")}
	cases := []struct{ code, want string }{
		{`return {modes={}}`, "unknown key"},
		{`return {functions={sh={description="",fn=function()end}}}`, "collides"},
		{`return {settings={timeout=-2}}`, "timeout"},
		{`return {hooks={{events={"bash_before"},fn=function()end}}}`, "unknown event"},
		{`return {loads={"nested.lua"}}`, "loads are not allowed"},
	}
	fixture(t, env.info.Root, "nested.lua", `return {loads={}}`)
	for _, tt := range cases {
		fixture(t, env.info.Root, "init.lua", tt.code)
		if c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings()); c != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("%s: %v %v", tt.code, c, err)
		}
	}
	fixture(t, env.info.Root, "init.lua", `while true do end`)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if _, err := LoadConfig(ctx, env, paths, config.DefaultSettings()); err == nil {
		t.Fatal("expected deadline")
	}
	env.log = make(chan string, 1)
	fixture(t, env.info.Root, "init.lua", `
local zel = require("zel")
local write = zel.write
local session = zel.session
local data = {n=1}
local function bump() data.n = data.n + 1 end
config_global = true
return {hooks={
 {events={"idle"},async=true,fn=function(event,arg)
  assert(config_global == nil and sh == nil and session.id == "s1")
  bump()
  write("async.txt", tostring(data.n) .. arg.text)
  error("async test finished", 0)
 end},
 {events={"idle"},fn=function() return tostring(data.n) end},
 {events={"idle"},fn=function() while true do end end},
}}`)
	c, err := LoadConfig(t.Context(), env, paths, config.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hooks := c.Data().Hooks
	if r, err := c.RunHook(t.Context(), hooks[0], "idle", map[string]any{"text": "!"}); err != nil || r != (HookResult{}) {
		t.Fatalf("%+v %v", r, err)
	}
	select {
	case msg := <-env.log:
		if !strings.Contains(msg, "async test finished") {
			t.Fatal(msg)
		}
	case <-time.After(time.Second):
		t.Fatal("async hook did not finish")
	}
	if data, err := os.ReadFile(filepath.Join(env.info.Root, "async.txt")); err != nil || string(data) != "2!" {
		t.Fatalf("%q %v", data, err)
	}
	if r, err := c.RunHook(t.Context(), hooks[1], "idle", nil); err != nil || r.Text != "1" {
		t.Fatalf("%+v %v", r, err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.RunHook(ctx, hooks[2], "idle", nil); err == nil {
		t.Fatal("expected deadline")
	}
	if r, err := c.RunHook(t.Context(), hooks[1], "idle", nil); err != nil || r.Text != "1" {
		t.Fatalf("VM did not recover: %+v %v", r, err)
	}
}
