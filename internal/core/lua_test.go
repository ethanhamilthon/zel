package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initLua(t *testing.T, code string) func(home, dir string) {
	return func(home, dir string) { writeFile(t, filepath.Join(dir, ".zel", "init.lua"), code) }
}

func TestMessageHookRewritesUserText(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `require("zel").on("message", function(ev) return ev.text:upper() end)`))
	if _, err := turn(t, a, "hello there"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[0]; !strings.Contains(req, "HELLO THERE") || strings.Contains(req, "hello there") {
		t.Fatalf("request: %s", req)
	}
}

func TestTurnEndHookInjectsOnceAndIsBounded(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local n = 0
require("zel").on("turn_end", function(ev)
  n = n + 1
  if n == 1 then return {inject = "CHECK-AGAIN"} end
end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "CHECK-AGAIN") {
		t.Fatalf("requests %d: %v", len(reqs), reqs)
	}

	fp2 := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	b := newTestAgentWith(t, fp2, Options{}, initLua(t, `require("zel").on("turn_end", function() return {inject = "AGAIN"} end)`))
	if _, err := turn(t, b, "go"); err != nil {
		t.Fatal(err)
	}
	if n := len(fp2.requests()); n != 1+maxTurnInjections {
		t.Fatalf("requests = %d, want %d", n, 1+maxTurnInjections)
	}
}

func TestBeforeCallHookDeniesAndRewrites(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", "local ok, err = pcall(sh, 'touch denied-marker')\nprint(tostring(ok), err)\nprint(sh('echo plain').out)")
		}
		return textReply("done")
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
require("zel").on("before_call", function(ev)
  if ev.fn == "sh" and ev.args[1]:find("denied") then return {deny = "no touching"} end
  if ev.fn == "sh" and ev.args[1] == "echo plain" then return {args = {"echo rewritten"}} end
end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[1]
	if !strings.Contains(out, "EDENIED") || !strings.Contains(out, "no touching") || !strings.Contains(out, "rewritten") {
		t.Fatalf("tool output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "denied-marker")); err == nil {
		t.Fatal("a denied call ran")
	}
}

// A fake zel binary stands in for the child process of agents.spawn.
func fakeZelOnPath(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "zel"), "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(filepath.Join(bin, "zel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAgentsRunAndSpawn(t *testing.T) {
	fakeZelOnPath(t, `printf '{"ok":true,"answer":"answer to: %s","session":"s1"}' "$2" > "$ZEL_REPORT"; echo "$2"`)
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `
local r = agents.run{prompt = "ping", model = "haiku-5.5"}
print(r.ok, r.answer, r.session)
local h = agents.spawn{prompt = "pong"}
print(h.kind, #agents.list())
local res = h:wait(30)
print(res.ok, res.answer, h:status())`)
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[1]
	for _, want := range []string{"answer to: ping", "answer to: pong", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	// Waited jobs never queue a <agent-result> message.
	time.Sleep(100 * time.Millisecond)
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("waited results were queued too: %+v", got)
	}
}

func TestUnwaitedAgentResultArrivesAsMessage(t *testing.T) {
	fakeZelOnPath(t, `sleep 1; printf '{"ok":true,"answer":"bg answer","session":"s2"}' > "$ZEL_REPORT"`)
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `agents.spawn{prompt = "later"}`)
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	got := awaitInput(t, a)
	if !strings.Contains(got[0].Text, "<agent-result") || !strings.Contains(got[0].Text, "bg answer") {
		t.Fatalf("message %q", got[0].Text)
	}
}

func TestBgDoneHookDropsAndRewrites(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `tasks.spawn("sleep 1; echo dropme", {name = "d"}); tasks.spawn("sleep 1; echo keepme", {name = "k"})`)
		}
		return textReply("done")
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
require("zel").on("bg_done", function(ev)
  if ev.result.out:find("dropme") then return {drop = true} end
  return {text = "REWRITTEN " .. ev.id}
end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	got := awaitInput(t, a)
	if len(got) != 1 || !strings.HasPrefix(got[0].Text, "REWRITTEN") {
		t.Fatalf("messages %+v", got)
	}
}

func TestInitLuaSnapshotAndReload(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	var path string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		path = filepath.Join(dir, ".zel", "init.lua")
		writeFile(t, path, `require("zel").instructions("FIRST")`)
	})
	writeFile(t, path, `require("zel").instructions("SECOND")`)
	if _, err := turn(t, a, "one"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[0]; !strings.Contains(req, "FIRST") || strings.Contains(req, "SECOND") {
		t.Fatalf("snapshot not kept: %s", req)
	}
}

func TestBrokenInitLuaDoesNotStopTheSession(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `this is not lua`))
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.requests()[0], "built-in") {
		t.Fatal("the built-in section is missing")
	}
}

func TestReloadKeepsOldSnapshotOnBrokenInitLua(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	var path string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		path = filepath.Join(dir, ".zel", "init.lua")
		writeFile(t, path, `require("zel").instructions("GOOD")`)
	})
	writeFile(t, path, `require("zel").instructions("NEW"); error("boom")`)
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[0]; !strings.Contains(req, "GOOD") || strings.Contains(req, "NEW") {
		t.Fatalf("old snapshot lost: %s", req)
	}
	writeFile(t, path, `require("zel").instructions("FIXED")`)
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "again"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[1]; !strings.Contains(req, "FIXED") {
		t.Fatalf("reload did not apply: %s", req)
	}
}

func TestDocumentedExamplesLoad(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "docs", "examples", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "ddgs"), "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && out=$2; shift; done\necho '[{\"title\":\"T\",\"href\":\"http://x\",\"body\":\"B\"}]' > \"$out\"\n")
	if err := os.Chmod(filepath.Join(bin, "ddgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `for _, h in ipairs(web.search("q", 1)) do print(h.url, h.title) end`)
		}
		return textReply("done")
	}}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		writeFile(t, filepath.Join(home, "init.lua"), read("global-init.lua")+"\n"+read("web-plugin.lua"))
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "AGENTS-TEXT")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), read("project-init.lua"))
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	req := fp.requests()[0]
	for _, want := range []string{"web.search(q: string", "web.fetch(url: string", "zel.check(pkg?: string", "AGENTS-TEXT", "## project", "## global"} {
		if !strings.Contains(req, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if out := fp.requests()[1]; !strings.Contains(out, "http://x") {
		t.Fatalf("web.search plugin did not run: %s", out)
	}
	if a.lua.get().LoadErrs != nil {
		t.Fatalf("load errors: %v", a.lua.get().LoadErrs)
	}
}
