package luart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestPromptTokenRules(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"#plan", "plan"},
		{"fix #plan, then #review.", "plan review"},
		{"a#plan", ""},
		{"##plan", ""},
		{"#", ""},
		{"(#plan)", ""},
		{"#plan-x_1 ok", "plan-x_1"},
	} {
		var names []string
		for _, tok := range PromptTokens(c.in) {
			names = append(names, tok.Name)
		}
		if got := strings.Join(names, " "); got != c.want {
			t.Errorf("PromptTokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// the cursor after "#pl" in "x #plan": the token spans the name after the cursor too
	if start, end, query, ok := PromptAt([]rune("x #plan"), 5); !ok || start != 2 || end != 7 || query != "pl" {
		t.Errorf("PromptAt = %d %d %q %v", start, end, query, ok)
	}
	if _, _, query, ok := PromptAt([]rune("#"), 1); !ok || query != "" {
		t.Errorf("a lone # must open the list with an empty query: %q %v", query, ok)
	}
	if _, _, _, ok := PromptAt([]rune("a#pl"), 4); ok {
		t.Error("a # glued to a word must not open the list")
	}
}

func TestPromptCompletionsExpandOncePerName(t *testing.T) {
	env := newEnv(t)
	g := layerFile(t, filepath.Join(env.cwd, "g"), `
require("zel").complete("plan", function() return "GLOBAL PLAN" end)
require("zel").complete("review", function() n = (n or 0) + 1; return "review " .. n end)`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `require("zel").complete("plan", function() return "PROJECT PLAN" end)`)
	cfg := LoadConfig(env, []Source{{"global", g}, {"project", p}})
	defer cfg.Close()
	if len(env.logs) == 0 || !strings.Contains(env.logs[0], "overrides") {
		t.Fatalf("the project completion must override the global one with a warning: %v", env.logs)
	}
	if got := strings.Join(cfg.PromptNames(), " "); got != "plan review" {
		t.Fatalf("names = %q", got)
	}
	got, err := cfg.Expand(context.Background(), "#plan then #review and #plan #missing #review")
	if want := "PROJECT PLAN then review 1 and PROJECT PLAN #missing review 1"; err != nil || got != want {
		t.Fatalf("Expand = %q, %v; want %q", got, err, want)
	}
}

func TestPromptCompletionErrorsKeepTheText(t *testing.T) {
	env := newEnv(t)
	p := layerFile(t, env.cwd, `
require("zel").complete("boom", function() error("bad state") end)
require("zel").complete("nothing", function() return nil end)
require("zel").complete("blank", function() return "  " end)
require("zel").complete("ok", function() return "fine" end)`)
	bad := layerFile(t, filepath.Join(env.cwd, "bad"), `require("zel").complete("has space", function() return "x" end)`)
	cfg := LoadConfig(env, []Source{{"project", p}, {"bad", bad}})
	if len(cfg.LoadErrs) != 1 || !strings.Contains(cfg.LoadErrs[0], "letters, digits") || cfg.HasPrompt("has space") {
		t.Fatalf("invalid name must skip its layer: %v", cfg.LoadErrs)
	}
	ctx := context.Background()
	for _, name := range []string{"boom", "nothing", "blank"} {
		if _, err := cfg.Expand(ctx, "#"+name); err == nil || !strings.HasPrefix(err.Error(), "#"+name+": ") {
			t.Errorf("#%s: err = %v", name, err)
		}
	}
	if _, err := cfg.Expand(ctx, "#ok and #boom"); err == nil {
		t.Error("one failing token must fail the whole expansion")
	}
	cfg.Close()
	if _, err := cfg.Expand(ctx, "#ok"); err == nil || !strings.Contains(err.Error(), "reloaded") {
		t.Errorf("a reloaded-away snapshot must not run: %v", err)
	}
}

// A call that waits for the config lock gives up with its own budget; closing the
// snapshot ends the hook that holds the lock, and a closed snapshot runs nothing.
func TestConfigLockWaitIsBoundedAndCloseEndsRunningHooks(t *testing.T) {
	env := newEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `
local zel = require("zel")
zel.on("session_start", function() state.set("holding", true); sleep(30) end)
zel.on("turn_end", function() return "never" end)`)}})
	holding := make(chan struct{})
	go func() {
		defer close(holding)
		cfg.SessionStart(context.Background())
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if v, _, _ := env.state.Get("session", "holding"); v == "true" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session_start hook never took the lock")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := cfg.TurnEnd(ctx, "x"); got != "" {
		t.Fatalf("a hook that never got the lock must not reply: %q", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("waiting for the lock took %s, past the caller's budget", took)
	}
	start = time.Now()
	cfg.Close()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not end the running hook")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("close took %s", took)
	}
	cfg.Close() // closing twice is harmless
	if got := cfg.TurnEnd(context.Background(), "y"); got != "" {
		t.Fatalf("a closed snapshot ran a hook: %q", got)
	}
}
