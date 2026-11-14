package luart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func layerFile(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".zel", "init.lua")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const docExample = `
local zel = require("zel")
zel.defaults{model = "sonnet-5.5", effort = "medium"}
zel.instructions [[Read code first.]]
zel.prompt(function(ctx) return "layer=" .. ctx.layer .. " " .. sh("echo dyn").out end)
zel.fn{
  name = "zel.check", summary = "run checks",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}, {"n", "integer"}},
  returns = "{ok:boolean}",
  examples = {'zel.check("./x", 1)'},
  run = function(pkg, n) state.set("last", pkg, "project"); return {ok = n > 0, pkg = pkg} end,
}
zel.fn{name = "boom", summary = "fails", run = function() error("kaput") end}
zel.fn{name = "secret", summary = "hidden", hidden = true, run = function() return 1 end}
zel.fn{name = "web.search", summary = "search", params = {{"q", "string"}}, run = function(q) return {q = q, h = zel.check(nil, 1)} end}
`

func TestPluginFnAndPrompt(t *testing.T) {
	env := newEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, docExample)}})
	defer cfg.Close()
	if len(env.notices) != 0 {
		t.Fatal(env.notices)
	}
	if d := cfg.Defaults(); d.Model != "sonnet-5.5" {
		t.Fatalf("%+v", d)
	}
	r := run(t, env, cfg, `return zel.check(nil, 2)`)
	if r.Output != `{"ok":true,"pkg":"./..."}` {
		t.Fatalf("%q", r.Output)
	}
	if v, _, _ := env.state.Get("project", "last"); v != `"./..."` {
		t.Fatalf("state %q", v)
	}
	for code, want := range map[string]string{
		`return zel.check("x")`:                       "zel.check: EINVAL: arg #2 n: expected integer, got nil",
		`return zel.check(5, 1)`:                      "arg #1 pkg: expected string, got number",
		`return boom()`:                               "boom: EFAIL: ",
		`local ok, e = pcall(boom); return e`:         "kaput",
		`return web.search("q").h.pkg`:                "./...",
		`return web.get ~= nil and web.search ~= nil`: "true",
		`return secret()`:                             "1",
	} {
		if out := run(t, env, cfg, code).Output; !strings.Contains(out, want) {
			t.Errorf("%s: got %q, want %q", code, out, want)
		}
	}
	prompt := cfg.SystemPrompt(context.Background())
	for _, want := range []string{"Read code first.\n\nlayer=project dyn", "# Lua\n", "## built-in", "## project (", "zel.check(pkg?: string=\"./...\", n: integer) -> {ok:boolean}  -- run checks",
		"--   pkg: go package pattern", "--   e.g. zel.check(\"./x\", 1)", "web.search(q: string)  -- search"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "secret(") {
		t.Error("hidden fn rendered")
	}
}

func TestLayerMergeAndErrors(t *testing.T) {
	env := newEnv(t)
	g := layerFile(t, filepath.Join(env.cwd, "g"), `
local zel = require("zel")
zel.defaults{model="a", effort="low"}
zel.instructions "global"
zel.fn{name="f", summary="global f", run=function() return "g" end}
zel.on("session_end", function() end)`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `
local zel = require("zel")
zel.defaults{model="b"}
zel.instructions "project"
zel.fn{name="f", summary="project f", run=function() return "p" end}
zel.fn{name="web.get", summary="x", run=function() end}`)
	bad := layerFile(t, filepath.Join(env.cwd, "bad"), `error("syntax is fine, runtime is not")`)
	bad2 := layerFile(t, filepath.Join(env.cwd, "bad2"), `local zel = require("zel"); zel.on("nope", function() end)`)
	cfg := LoadConfig(env, []Source{{"global", g}, {"project", p}, {"bad", bad}, {"bad2", bad2}})
	defer cfg.Close()
	if len(env.notices) != 3 { // web.get override, bad, bad2
		t.Fatalf("notices: %q", env.notices)
	}
	if d := cfg.Defaults(); d.Model != "a" || d.Effort != "low" {
		t.Fatalf("%+v", d)
	}
	if out := run(t, env, cfg, `return f()`).Output; out != "g" {
		// project layer failed (web.get is builtin) and is skipped as a whole
		t.Fatalf("f = %q", out)
	}
	prompt := cfg.SystemPrompt(context.Background())
	if !strings.HasPrefix(prompt, "global\n\n# Lua") || strings.Contains(prompt, "project f") {
		t.Fatalf("%s", prompt)
	}
}

func TestProjectOverridesGlobalFn(t *testing.T) {
	env := newEnv(t)
	g := layerFile(t, filepath.Join(env.cwd, "g"), `require("zel").fn{name="f", summary="global f", run=function() return "g" end}`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `require("zel").fn{name="f", summary="project f", run=function() return "p" end}`)
	cfg := LoadConfig(env, []Source{{"global", g}, {"project", p}})
	defer cfg.Close()
	if out := run(t, env, cfg, `return f()`).Output; out != "p" {
		t.Fatalf("%q", out)
	}
	if !strings.Contains(env.logs[0], "overrides") {
		t.Fatalf("%v", env.logs)
	}
	if prompt := cfg.SystemPrompt(context.Background()); strings.Contains(prompt, "global f") || !strings.Contains(prompt, "project f") {
		t.Fatal(prompt)
	}
}

func TestHooks(t *testing.T) {
	env := newEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `
local zel = require("zel")
zel.on("session_start", function() state.set("started", true); return "welcome" end)
zel.on("session_end", function(ev) state.set("ended", ev.reason) end)
zel.on("message", function(ev) return ev.text:upper() end)
zel.on("turn_end", function(ev) if ev.answer == "again" then return {notify = "go on"} end end)
zel.on("message", function() error("hook bug") end)
`)}})
	defer cfg.Close()
	ctx := context.Background()
	if got := cfg.SessionStart(ctx); got != "welcome" {
		t.Fatalf("session_start: %q", got)
	}
	if v, _, _ := env.state.Get("session", "started"); v != "true" {
		t.Fatalf("session_start: %q", v)
	}
	if got := cfg.Message(ctx, "hi"); got != "HI" {
		t.Fatalf("message: %q", got)
	}
	if len(env.logs) == 0 || !strings.Contains(env.logs[len(env.logs)-1], "hook bug") {
		t.Fatalf("hook errors are logged: %v", env.logs)
	}
	if got := cfg.TurnEnd(ctx, "again"); got != "go on" {
		t.Fatalf("turn_end: %q", got)
	}
	if got := cfg.TurnEnd(ctx, "done"); got != "" {
		t.Fatalf("turn_end: %q", got)
	}
	cfg.SessionEnd(ctx, "quit")
	if v, _, _ := env.state.Get("session", "ended"); v != `"quit"` {
		t.Fatalf("session_end: %q", v)
	}
	for _, old := range []string{"before_call", "after_call", "bg_done"} {
		bad := LoadConfig(env, []Source{{"old", layerFile(t, t.TempDir(), `require("zel").on("`+old+`", function() end)`)}})
		if len(bad.LoadErrs) != 1 {
			t.Errorf("%s still accepted", old)
		}
		bad.Close()
	}
}

func TestInstructionsFileAndRequire(t *testing.T) {
	env := newEnv(t)
	os.WriteFile(filepath.Join(env.cwd, "AGENTS.md"), []byte("from file\n"), 0o644)
	os.MkdirAll(filepath.Join(env.cwd, ".zel", "lua"), 0o755)
	os.WriteFile(filepath.Join(env.cwd, ".zel", "lua", "helper.lua"), []byte("return {hi = function() return 'hi from helper' end}"), 0o644)
	path := layerFile(t, env.cwd, `
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.instructions{file = "MISSING.md"}
zel.fn{name = "helped", summary = "uses helper", run = function() return require("helper").hi() end}`)
	cfg := LoadConfig(env, []Source{{"project", path}})
	defer cfg.Close()
	if len(env.notices) != 1 || !strings.Contains(env.notices[0], "MISSING.md") {
		t.Fatalf("%v", env.notices)
	}
	if !strings.HasPrefix(cfg.SystemPrompt(context.Background()), "from file\n\n# Lua") {
		t.Fatal(cfg.SystemPrompt(context.Background()))
	}
}
