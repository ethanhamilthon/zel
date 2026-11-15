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
local check = {
  desc = "run checks\nargs: pkg? (default ./...), n\nreturns {ok, pkg}",
  fn = function(a) write("last", a.pkg or "./..."); return {ok = a.n > 0, pkg = a.pkg or "./..."} end,
}
return {
  {instructions = {"Read code first.", function() return "dyn " .. sh("echo x").out end}},
  {tools = {
    ["zel.check"] = check,
    boom = {desc = "fails", fn = function() error("kaput") end},
    ["web.search"] = {desc = "search", fn = function(a) return {q = a.q, h = zel.check{n = 1}} end},
  }},
  true, -- what require() gives for a module that returns nothing
}
`

func TestToolsAndPrompt(t *testing.T) {
	env := newEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, docExample)}})
	defer cfg.Close()
	if len(env.notices) != 0 {
		t.Fatal(env.notices)
	}
	r := run(t, env, cfg, `return zel.check{n = 2}`)
	if r.Output != `{"ok":true,"pkg":"./..."}` {
		t.Fatalf("%q", r.Output)
	}
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "last")); string(data) != "./..." {
		t.Fatalf("the tool runs with the host API: %q", data)
	}
	for code, want := range map[string]string{
		`return zel.check("x")`:               "zel.check: EINVAL: takes one table of arguments: zel.check{...}, got string",
		`return zel.check({n = 1}, 2)`:        "zel.check: EINVAL: takes one table of arguments",
		`return boom()`:                       "boom: EFAIL: ",
		`local ok, e = pcall(boom); return e`: "kaput",
		`return web.search{q = "q"}.h.pkg`:    "./...",
	} {
		if out := run(t, env, cfg, code).Output; !strings.Contains(out, want) {
			t.Errorf("%s: got %q, want %q", code, out, want)
		}
	}
	prompt := cfg.SystemPrompt(context.Background())
	for _, want := range []string{"Read code first.\n\ndyn x", "# Lua\n", "## built-in", "## tools (", "zel.check{...}  -- run checks\n--   args: pkg? (default ./...), n\n--   returns {ok, pkg}", "web.search{...}  -- search", "boom{...}  -- fails"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestLayerMergeAndErrors(t *testing.T) {
	env := newEnv(t)
	g := layerFile(t, filepath.Join(env.cwd, "g"), `return {
  instructions = "global",
  tools = {f = {desc = "global f", fn = function() return "g" end}},
  hooks = {session_end = function() end},
}`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `return {
  instructions = "project",
  tools = {f = {desc = "project f", fn = function() return "p" end}, read = {desc = "x", fn = function() end}},
}`)
	cases := map[string]string{
		"runtime":  `error("syntax is fine, runtime is not")`,
		"hook":     `return {hooks = {nope = function() end}}`,
		"subagent": `return {hooks = {subagent_end = function() end}}`,
		"key":      `return {instruction = "typo"}`,
		"defaults": `return {defaults = {model = "m"}}`,
		"zel":      `local zel = require("zel")`,
		"desc":     `return {tools = {t = {fn = function() end}}}`,
		"fn":       `return {tools = {t = {desc = "x"}}}`,
		"number":   `return 5`,
	}
	sources := []Source{{"global", g}, {"project", p}}
	for name, body := range cases {
		sources = append(sources, Source{name, layerFile(t, filepath.Join(env.cwd, name), body)})
	}
	cfg := LoadConfig(env, sources)
	defer cfg.Close()
	if len(cfg.LoadErrs) != len(cases)+1 { // + the project layer: read is built in
		t.Fatalf("load errors: %q", cfg.LoadErrs)
	}
	for _, want := range []string{"read is built in", `unknown hook "nope"`, `unknown hook "subagent_end"`, `unknown key "instruction"`, "defaults was removed", `require("zel") was removed`, "desc is required", "fn is required", "must return a table"} {
		if !strings.Contains(strings.Join(cfg.LoadErrs, "\n"), want) {
			t.Errorf("no load error says %q: %q", want, cfg.LoadErrs)
		}
	}
	if out := run(t, env, cfg, `return f()`).Output; out != "g" {
		t.Fatalf("the failed project layer must be skipped as a whole: f = %q", out)
	}
	prompt := cfg.SystemPrompt(context.Background())
	if !strings.HasPrefix(prompt, "global\n\n# Lua") || strings.Contains(prompt, "project f") {
		t.Fatalf("%s", prompt)
	}
}

func TestProjectOverridesGlobalTool(t *testing.T) {
	env := newEnv(t)
	g := layerFile(t, filepath.Join(env.cwd, "g"), `return {tools = {f = {desc = "global f", fn = function() return "g" end}}}`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `return {tools = {f = {desc = "project f", fn = function() return "p" end}}}`)
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
local seen = {}
return {
  {hooks = {
    session_start = function() return "welcome" end,
    session_end = function(reason) write("ended", reason) end,
    message = function(text, notice) if notice then write("notice", text) return "ignored" end return text:upper() end,
    turn_end = function(answer, running)
      if #running > 0 then return running[1].name .. " " .. running[1].id .. " " .. running[1].ms end
      if answer == "again" then return "go on" end
    end,
  }},
  {hooks = {message = function(text) return text .. "!" end}},
  {hooks = {message = function() error("hook bug") end}},
}`)}})
	defer cfg.Close()
	ctx := context.Background()
	if got := cfg.SessionStart(ctx); got != "welcome" {
		t.Fatalf("session_start: %q", got)
	}
	if got := cfg.Message(ctx, "hi"); got != "HI!" {
		t.Fatalf("message hooks chain: %q", got)
	}
	if len(env.logs) == 0 || !strings.Contains(env.logs[len(env.logs)-1], "hook bug") {
		t.Fatalf("hook errors are logged: %v", env.logs)
	}
	cfg.Notice(ctx, "t1 finished")
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "notice")); string(data) != "t1 finished" {
		t.Fatalf("notice: %q", data)
	}
	if got := cfg.TurnEnd(ctx, "again", nil); got != "go on" {
		t.Fatalf("turn_end: %q", got)
	}
	if got := cfg.TurnEnd(ctx, "done", nil); got != "" {
		t.Fatalf("turn_end: %q", got)
	}
	if got := cfg.TurnEnd(ctx, "done", []RunningTask{{ID: "t2", Name: "build", Ms: 1500}}); got != "build t2 1500" {
		t.Fatalf("turn_end with a running task: %q", got)
	}
	cfg.SessionEnd(ctx, "quit")
	if data, _ := os.ReadFile(filepath.Join(env.cwd, "ended")); string(data) != "quit" {
		t.Fatalf("session_end: %q", data)
	}
}

func TestInstructionsFileAndRequire(t *testing.T) {
	env := newEnv(t)
	os.WriteFile(filepath.Join(env.cwd, "AGENTS.md"), []byte("from file\n"), 0o644)
	os.MkdirAll(filepath.Join(env.cwd, ".zel", "lua"), 0o755)
	os.WriteFile(filepath.Join(env.cwd, ".zel", "lua", "helper.lua"), []byte("return {tools = {helped = {desc = 'uses helper', fn = function() return 'hi from helper' end}}}"), 0o644)
	path := layerFile(t, env.cwd, `return {{instructions = read("AGENTS.md")}, require("helper")}`)
	cfg := LoadConfig(env, []Source{{"project", path}})
	defer cfg.Close()
	if len(env.notices) != 0 {
		t.Fatalf("%v", env.notices)
	}
	if !strings.HasPrefix(cfg.SystemPrompt(context.Background()), "from file\n\n# Lua") {
		t.Fatal(cfg.SystemPrompt(context.Background()))
	}
	if out := run(t, env, cfg, `return helped()`).Output; out != "hi from helper" {
		t.Fatalf("%q", out)
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
	g := layerFile(t, filepath.Join(env.cwd, "g"), `return {completions = {
  plan = function() return "GLOBAL PLAN" end,
  review = function() n = (n or 0) + 1; return "review " .. n end,
}}`)
	p := layerFile(t, filepath.Join(env.cwd, "p"), `return {completions = {plan = function() return "PROJECT PLAN" end}}`)
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
	p := layerFile(t, env.cwd, `return {completions = {
  boom = function() error("bad state") end,
  nothing = function() return nil end,
  blank = function() return "  " end,
  ok = function() return "fine" end,
}}`)
	bad := layerFile(t, filepath.Join(env.cwd, "bad"), `return {completions = {["has space"] = function() return "x" end}}`)
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
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `return {hooks = {
  session_start = function() write("holding", "1"); while true do end end,
  turn_end = function() return "never" end,
}}`)}})
	holding := make(chan struct{})
	go func() {
		defer close(holding)
		cfg.SessionStart(context.Background())
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(env.cwd, "holding")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session_start hook never took the lock")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := cfg.TurnEnd(ctx, "x", nil); got != "" {
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
	if got := cfg.TurnEnd(context.Background(), "y", nil); got != "" {
		t.Fatalf("a closed snapshot ran a hook: %q", got)
	}
}

// A hook stuck in a host operation that never returns holds neither its caller past
// the caller's deadline nor Close: Close returns at once, and the VM is freed when
// the blocked Lua returns.
func TestStuckHookDoesNotHoldCallerOrClose(t *testing.T) {
	env := newEnv(t)
	fifo := mkfifo(t, filepath.Join(env.cwd, "stuck"))
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `return {hooks = {turn_end = function() return io.open("`+fifo+`", "r"):read("a") end}}`)}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := cfg.TurnEnd(ctx, "x", nil); got != "" {
		t.Fatalf("a stuck hook must not reply: %q", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the caller waited %s behind a stuck hook", took)
	}
	start = time.Now()
	cfg.Close()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("close waited %s for a stuck hook", took)
	}
	cfg.Close()
	if got := cfg.TurnEnd(context.Background(), "y", nil); got != "" {
		t.Fatalf("a closed snapshot ran a hook: %q", got)
	}
	go releasePipe(fifo)
	awaitFreed(t, cfg.v)
}

// Cancelling a config-side completion while a host call is blocked ends its
// foreground activity immediately when Expand returns, and releasing the blocked
// call afterward neither double-closes nor disturbs a later completion's activity.
func TestAbandonedConfigCallClearsForegroundActivity(t *testing.T) {
	env := newGatedHostEnv(t)
	cfg := LoadConfig(env, []Source{{"project", layerFile(t, env.cwd, `return {completions = {
  stuck = function() session.rename("stuck"); return "x" end,
  hold = function() view("later.png"); return "ok" end,
  fast = function() return "done" end,
}}`)}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cfg.Expand(ctx, "#stuck")
		done <- err
	}()
	select {
	case <-env.renameEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("session.rename was never entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Expand returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Expand did not return after cancel")
	}
	if got := env.foreground(); got != "" {
		t.Fatalf("foreground activity still live after cancelled Expand: %q", got)
	}

	// Expand acquires the config lock; release the blocked call so a later completion
	// can run, while testing Close and end-count invariants.
	close(env.renameRelease)
	got, err := cfg.Expand(context.Background(), "#fast")
	if err != nil || got != "done" {
		t.Fatalf("Expand(#fast) = %q, %v", got, err)
	}
	if fg := env.foreground(); fg != "" {
		t.Fatalf("foreground after #fast = %q", fg)
	}

	// Also verify cancelling while holding #hold leaves no activity and allows Close.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan error, 1)
	go func() {
		_, err := cfg.Expand(ctx2, "#hold")
		done2 <- err
	}()
	select {
	case <-env.viewEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("view was never entered")
	}
	cancel2()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("cancelled #hold returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Expand(#hold) did not return after cancel")
	}
	if fg := env.foreground(); fg != "" {
		t.Fatalf("foreground after #hold cancel = %q", fg)
	}
	cfg.Close()
	close(env.viewRelease)
	awaitFreed(t, cfg.v)

	env.stackMu.Lock()
	defer env.stackMu.Unlock()
	for id, count := range env.endCalls {
		if count != 1 {
			t.Errorf("activity %d ended %d times, want 1", id, count)
		}
	}
}
