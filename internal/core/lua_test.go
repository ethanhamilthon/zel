package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zel/internal/luart"
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

func TestMessageHookObservesNotificationWithoutRewriting(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
require("zel").on("message", function(ev)
 if ev.notice then
  state.set("observed_notice", ev.text)
  return "DO-NOT-REWRITE"
 end
end)`))
	input := notification("t1 finished")
	got := a.hookInput(context.Background(), input)
	if got.Text != input.Text || !got.Notice {
		t.Fatalf("notification changed: %+v", got)
	}
	value, ok, err := (kvState{s: a.session()}).Get("session", "observed_notice")
	if err != nil || !ok || !strings.Contains(value, "t1 finished") {
		t.Fatalf("observed notice = %q, %v, %v", value, ok, err)
	}
}

func TestTurnEndHookInjectsOnceAndIsBounded(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local n = 0
require("zel").on("turn_end", function(ev)
  n = n + 1
  if n == 1 then return {notify = "CHECK-AGAIN"} end
end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "CHECK-AGAIN") {
		t.Fatalf("requests %d: %v", len(reqs), reqs)
	}

	fp2 := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	b := newTestAgentWith(t, fp2, Options{}, initLua(t, `require("zel").on("turn_end", function() return "AGAIN" end)`))
	if _, err := turn(t, b, "go"); err != nil {
		t.Fatal(err)
	}
	if n := len(fp2.requests()); n != 1+maxTurnInjections {
		t.Fatalf("requests = %d, want %d", n, 1+maxTurnInjections)
	}
}

func TestSessionModelsListsOnceAndKeepsFailures(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case body == "": // GET /models
			return `{"data":[{"id":"m-a","context_length":8000}]}`
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		}
		return luaReply("c1", `local m, f = session.models(); session.models()
print(#m .. " " .. m[1].provider .. "/" .. m[1].model .. " " .. m[1].context_window .. " " .. tostring(f.dead ~= nil))`)
	}}
	a := newTestAgent(t, fp, Options{})
	// A provider that refuses connections: its failure must not fail the listing.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	home, err := ZelHome(a.Shell.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Settings()
	s.Providers["dead"] = Provider{BaseURL: deadURL}
	if err := WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	listings, answered := 0, false
	for _, body := range fp.requests() {
		if body == "" {
			listings++
		}
		answered = answered || strings.Contains(body, "1 p/m-a 8000 true")
	}
	if listings != 1 || !answered {
		t.Fatalf("listings %d, answered %v: %v", listings, answered, fp.requests())
	}
}

func TestSessionModelsCancelledCallDoesNotWaitForListing(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		if body == "" { // GET /models: held until the test opens the gate
			<-gate
			return `{"data":[{"id":"m-a"}]}`
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	t.Cleanup(open) // runs before the server closes, so a held request can end
	listings := func() (n int) {
		for _, body := range fp.requests() {
			if body == "" {
				n++
			}
		}
		return n
	}
	// A background task starts the listing and the provider holds it.
	if res := a.runLua(context.Background(), `return tasks.spawn(function() return #session.models() end, {name = "list"})`); res.IsError {
		t.Fatal(res.Output)
	}
	for deadline := time.Now().Add(5 * time.Second); listings() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the listing never started")
		}
	}
	// A main call with a short deadline gives up without waiting for the task's listing.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	answered := make(chan luart.Result, 1)
	go func() { answered <- a.runLua(ctx, `return #session.models()`) }()
	select {
	case res := <-answered:
		if !res.IsError || !strings.Contains(res.Output, "ECANCELED") {
			t.Fatalf("cancelled call: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call waited for the other listing")
	}
	// The cancelled caller neither released the held request nor started a listing of its own.
	if _, done, _ := (luaEnv{a.session()}).Tasks().Wait(context.Background(), "t1", 100*time.Millisecond); done || listings() != 1 {
		t.Fatalf("task finished %v, listings %d", done, listings())
	}
	open()
	if _, done, err := (luaEnv{a.session()}).Tasks().Wait(context.Background(), "t1", 5*time.Second); !done || err != nil {
		t.Fatalf("task after release: done %v, err %v", done, err)
	}
	if res := a.runLua(context.Background(), `return #session.models()`); res.IsError || res.Output != "1" || listings() != 1 {
		t.Fatalf("answer after release %+v, listings %d", res, listings())
	}
}

func TestSessionModelsRefreshesOnProviderChangeAndNewSession(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if body == "" {
			return `{"data":[{"id":"m-a"}]}`
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	listings := func() (n int) {
		for _, body := range fp.requests() {
			if body == "" {
				n++
			}
		}
		return n
	}
	answer := func() string {
		res := a.runLua(context.Background(), `local m, f = session.models(); return #m .. " " .. tostring(f.dead ~= nil)`)
		if res.IsError {
			t.Fatal(res.Output)
		}
		return res.Output
	}
	if got := answer(); got != "1 false" || listings() != 1 {
		t.Fatalf("first answer %q, listings %d", got, listings())
	}
	if got := answer(); got != "1 false" || listings() != 1 {
		t.Fatalf("cached answer %q, listings %d", got, listings())
	}
	// A new provider changes the providers: the list is made again, with its failure.
	home, err := ZelHome(a.Shell.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Settings()
	s.Providers["dead"] = Provider{BaseURL: deadURL}
	if err := WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if got := answer(); got != "1 true" || listings() != 2 {
		t.Fatalf("after provider change %q, listings %d", got, listings())
	}
	// A new session starts with a new list.
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if got := answer(); got != "1 true" || listings() != 3 {
		t.Fatalf("after new session %q, listings %d", got, listings())
	}
}

func TestMainVMPersistsAcrossCalls(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch n {
		case 1:
			return luaReply("c1", "x = 7")
		case 2:
			return luaReply("c2", `return "kept-" .. x`)
		}
		return textReply("done")
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `require("zel").on("session_end", function(ev) state.set("ended", ev.reason, "project") end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[2]; !strings.Contains(req, "kept-7") {
		t.Fatalf("globals did not survive: %s", req)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := a.db.KVGet("project:"+a.projectRoot(), "ended"); !ok || v != `"new"` {
		t.Fatalf("session_end: %q %v", v, ok)
	}
}

var childMarker = regexp.MustCompile(`child-\w+`)

// agentProvider plays both the parent (a lua call, then "done") and its
// children (an answer naming their prompt): children never see the call id c1.
func agentProvider(parentCode string, onChild func(prompt string)) *fakeProvider {
	return &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case childMarker.MatchString(body):
			prompt := childMarker.FindString(body)
			if onChild != nil {
				onChild(prompt)
			}
			return textReply("answer to: " + prompt)
		}
		return luaReply("c1", parentCode)
	}}
}

func TestAgentsRunAndSpawn(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // agents run in-process: no zel executable needed
	fp := agentProvider(`
local r = agents.run{prompt = "child-ping"}
print(r.ok, r.answer, r.session ~= "")
local id = agents.spawn{prompt = "child-pong"}
print(type(id), #agents.list(), agents.get(id).prompt)
local res = agents.wait(id, 8)
print(res.ok, res.answer, agents.status(id), agents.result(id).answer)
print(agents.tail(id, 5))`, nil)
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[len(fp.requests())-1]
	for _, want := range []string{"true\\tanswer to: child-ping\\ttrue", "string\\t2\\tchild-pong", "true\\tanswer to: child-pong\\tdone\\tanswer to: child-pong"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	// Waited jobs never queue a <agent-result> message.
	time.Sleep(100 * time.Millisecond)
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("waited results were queued too: %+v", got)
	}
	// The children ran in this process, as sessions of their own.
	if list, err := a.Tasks(); err != nil || len(list) != 2 || !strings.HasPrefix(list[0].Command, "agent: child-ping") {
		t.Fatalf("tasks %+v %v", list, err)
	}
	if got := a.ActiveSubagents(); len(got) != 0 {
		t.Fatalf("finished agents still active: %+v", got)
	}
}

func TestUnwaitedAgentResultArrivesAsMessage(t *testing.T) {
	fp := agentProvider(`agents.spawn{prompt = "child-later"}`, nil)
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	got := awaitInput(t, a)
	if got[0].Text != "<notification>a1 finished</notification>" {
		t.Fatalf("message %q", got[0].Text)
	}
}

func TestAgentsConcurrentNestedAndStopped(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		case strings.Contains(body, "child-outer") && !strings.Contains(body, `"n1"`):
			return luaReply("n1", `print(select(2, pcall(agents.run, {prompt = "child-inner"})))`)
		case strings.Contains(body, "child-outer") && strings.Contains(body, "EDENIED"):
			return textReply("outer was denied")
		case strings.Contains(body, "child-outer"):
			return textReply("outer saw inner")
		case strings.Contains(body, "child-inner"):
			return textReply("inner answer")
		}
		return luaReply("c1", `
local nested = agents.spawn{prompt = "child-outer"}
local stuck = agents.spawn{prompt = "child-stuck"}
local r = agents.wait_all({nested}, 8)
print(r[1].ok, r[1].answer)
local x = agents.stop(stuck)
print(agents.wait(stuck, 8).ok, agents.status(stuck))`)
	}}
	a := newTestAgent(t, fp, Options{})
	t.Cleanup(func() { close(gate) }) // runs before the server closes
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[len(fp.requests())-1]
	for _, want := range []string{"true\\touter was denied", "false\\tstopped"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}

func TestAgentDepthLimitAndParentShutdown(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		if strings.Contains(body, "child-hang") {
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-hang"}`)
	}}
	a := newTestAgent(t, fp, Options{})
	t.Cleanup(func() { close(gate) })
	a.session().depth = MaxAgentDepth
	if _, err := (luaEnv{a.session()}).Agents().Spawn(luart.AgentOpts{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Fatalf("depth limit: %v", err)
	}
	a.session().depth = 0
	id, err := (luaEnv{a.session()}).Agents().Spawn(luart.AgentOpts{Prompt: "child-hang"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(a.ActiveSubagents()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("agent never became active")
		}
	}
	// Closing the parent cancels the child and waits for it.
	start := time.Now()
	a.release()
	if time.Since(start) > 5*time.Second || len(a.ActiveSubagents()) != 0 {
		t.Fatalf("shutdown left agent %s running after %s", id, time.Since(start))
	}
}

func TestTaskFunctionNotifiesOnce(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `local tag = "v1"; tasks.spawn(function() sleep(0.2); return tag end, {name = "k"})`)
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	got := awaitInput(t, a)
	if len(got) != 1 || got[0].Text != "<notification>t1 finished</notification>" {
		t.Fatalf("messages %+v", got)
	}
	res, ok, err := (luaEnv{a.session()}).Tasks().Result("t1")
	if err != nil || !ok || res["value"] != "v1" || res["ok"] != true {
		t.Fatalf("result %+v %v %v", res, ok, err)
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
	if a.session().lua.get().LoadErrs != nil {
		t.Fatalf("load errors: %v", a.session().lua.get().LoadErrs)
	}
}

func TestCancelEndsRunningCallAndKeepsBackground(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		return luaReply("c1", "local h = tasks.spawn(function() sh('sleep 30') end, {name='bg'})\nfs.write('spawned', h)\nsleep(20)")
	}}
	a := newTestAgent(t, fp, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- a.Run(ctx, "go", func(TurnEvent) {}) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(a.Shell.Dir, "spawned")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the call never started")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the call")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("too slow")
	}
	if a.RunningTasks() != 1 {
		t.Fatalf("the spawned task did not survive: %d running", a.RunningTasks())
	}
	_ = a.StopTask("t1")
}

// storedString reads a string kept by state.set in the project scope.
func storedString(t *testing.T, a *Runtime, key string) string {
	t.Helper()
	raw, ok, err := a.db.KVGet("project:"+a.projectRoot(), key)
	if err != nil || !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// awaitStored waits until a stored string reaches want.
func awaitStored(t *testing.T, a *Runtime, key, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); storedString(t, a, key) != want; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s = %q, want %q", key, storedString(t, a, key), want)
		}
	}
}

func TestSubagentLifecycleHooksRunOncePerSubagent(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		case childMarker.MatchString(body):
			return textReply("answer to: " + childMarker.FindString(body))
		}
		return luaReply("c1", `
agents.run{prompt = "child-ping"}
local stuck = agents.spawn{prompt = "child-stuck"}
agents.stop(stuck)
print(agents.wait(stuck, 8).ok)
print(agents.run{prompt = "child-bad", provider = "missing"}.ok)`)
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local zel = require("zel")
local function record(key, value)
	state.set(key, (state.get(key, "project") or "") .. value .. ";", "project")
end
zel.on("subagent_start", function(ev) record("start." .. ev.id, ev.model) end)
zel.on("subagent_end", function(ev) record("end." .. ev.id, ev.status) end)`))
	t.Cleanup(func() { close(gate) }) // runs before the server closes
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	// The start hooks run beside the subagents and the end hooks just after
	// their results are published, so wait for every expected record.
	wants := map[string]string{"start.a1": "m;", "start.a2": "m;", "start.a3": "m;", "end.a1": "done;", "end.a2": "stopped;", "end.a3": "failed;"}
	for key, want := range wants {
		awaitStored(t, a, key, want)
	}
	time.Sleep(50 * time.Millisecond) // a duplicate hook would have landed by now
	for key, want := range wants {
		if got := storedString(t, a, key); got != want {
			t.Errorf("%s = %q, want %q: hooks ran more than once or out of order", key, got, want)
		}
	}
}

// A plugin function holds the config lock while it waits for a subagent; the
// subagent's lifecycle hooks need that lock, so they must not block the result.
func TestSubagentHooksDoNotBlockPluginFunctionWaiting(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case childMarker.MatchString(body):
			return textReply("answer to: " + childMarker.FindString(body))
		}
		return luaReply("c1", `print(review())`)
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local zel = require("zel")
zel.fn{name = "review", summary = "runs a subagent", run = function()
	return agents.run{prompt = "child-ping"}.answer
end}
zel.on("subagent_start", function(ev) sh("true") end)
zel.on("subagent_end", function(ev) sh("true") end)`))
	start := time.Now()
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("plugin call waited %s for its subagent", elapsed)
	}
	if out := fp.requests(); !strings.Contains(out[len(out)-1], "answer to: child-ping") {
		t.Fatalf("plugin result missing: %s", out)
	}
}

// Overlapping foreground operations keep their own entries: a call ends only
// its own, the indicator shows the newest one still running, and a background
// operation never takes over the foreground.
func TestActivityOverlapAndBackground(t *testing.T) {
	a := &Runtime{cur: &session{acts: &activities{}}}
	endOuter := a.beginActivity("running review")
	endInner := a.beginActivity("reading ~/.zel/init.lua")
	if got := a.Activity(); got != "reading ~/.zel/init.lua" {
		t.Fatalf("activity = %q, want the newest operation", got)
	}
	endInner()
	endInner() // ending twice is harmless
	if got := a.Activity(); got != "running review" {
		t.Fatalf("after the inner end activity = %q", got)
	}
	endBackground := luaEnv{a.session()}.Begin("sleeping 60 sec", true)
	if got := a.Activity(); got != "running review" {
		t.Fatalf("background work replaced the foreground: %q", got)
	}
	endBackground()
	endOuter()
	if got := a.Activity(); got != "" {
		t.Fatalf("activity %q left after every operation ended", got)
	}
}

// A subagent publishes its activity on its own row; unchanged labels do not
// notify again, and a finished subagent leaves no row to update.
func TestSubagentRowTracksItsOwnActivity(t *testing.T) {
	tree := newAgentTree()
	changes := 0
	tree.changed = func() { changes++ }
	tree.add(SubagentRun{ID: "s/a1", Parent: "s"})
	child := &Runtime{tree: tree, cur: &session{tree: tree, acts: &activities{}, link: &agentLink{tree: tree, root: "s", id: "s/a1"}}}
	endFirst := child.beginActivity("reading a.txt")
	endSecond := child.beginActivity("reading a.txt")
	rows := tree.active("s")
	if len(rows) != 1 || rows[0].Activity != "reading a.txt" {
		t.Fatalf("row = %+v", rows)
	}
	before := changes
	endSecond() // the shown label is still the first one: nothing changed
	if changes != before {
		t.Fatalf("an unchanged activity notified again")
	}
	endFirst()
	if got := tree.active("s")[0].Activity; got != "" {
		t.Fatalf("row kept activity %q after the operations ended", got)
	}
	if changes != before+1 {
		t.Fatalf("changes = %d, want one notification for the cleared activity", changes)
	}
	tree.remove("s/a1")
	child.beginActivity("late") // a finished subagent updates nothing
	if rows := tree.active("s"); len(rows) != 0 {
		t.Fatalf("finished subagent came back: %+v", rows)
	}
}

// Registered completions expand through the controller, outside its lock, and
// follow a reload: the new init.lua is the snapshot for the next expansion.
func TestPromptCompletionsExpandAndFollowReload(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	var projectDir string
	setup := func(home, dir string) {
		projectDir = dir
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `require("zel").complete("plan", function() return "first plan" end)`)
	}
	a := newTestAgentWith(t, fp, Options{}, setup)
	ctl := NewController(a, func(Event) {})
	if got := strings.Join(ctl.PromptNames(), " "); got != "plan" || !ctl.HasPromptTokens("do #plan") {
		t.Fatalf("names = %q", got)
	}
	if got, err := ctl.ExpandPrompt(context.Background(), "do #plan and #unknown"); err != nil || got != "do first plan and #unknown" {
		t.Fatalf("ExpandPrompt = %q, %v", got, err)
	}
	if ctl.HasPromptTokens("plain #unknown text") {
		t.Fatal("an unregistered name must not count as a completion")
	}
	writeFile(t, filepath.Join(projectDir, ".zel", "init.lua"), `require("zel").complete("plan", function() return "second plan" end)`)
	ctl.Reload()
	if got, err := ctl.ExpandPrompt(context.Background(), "#plan"); err != nil || got != "second plan" {
		t.Fatalf("after reload ExpandPrompt = %q, %v", got, err)
	}
}

// A subagent's end hook belongs to the snapshot that ran its start hook. A reload
// in between neither drops that end hook nor hands it to the new snapshot.
func TestSubagentEndStaysWithItsSnapshotAcrossReload(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-stuck"}`)
	}}
	first := `
local zel = require("zel")
zel.on("subagent_start", function(ev) state.set("start." .. ev.id, "v1", "project") end)
zel.on("subagent_end", function(ev) state.set("end." .. ev.id, "v1:" .. ev.status, "project") end)`
	second := `
local zel = require("zel")
zel.on("subagent_end", function(ev) state.set("end2." .. ev.id, "v2:" .. ev.status, "project") end)`
	var path string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		path = filepath.Join(dir, ".zel", "init.lua")
		writeFile(t, path, first)
	})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release) // runs before the session closes
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	awaitStored(t, a, "start.a1", "v1")
	writeFile(t, path, second)
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	release()
	awaitStored(t, a, "end.a1", "v1:done")
	time.Sleep(50 * time.Millisecond) // a misrouted hook would have landed by now
	if got := storedString(t, a, "end2.a1"); got != "" {
		t.Fatalf("the reloaded snapshot received an end hook it never matched a start for: %q", got)
	}
}

// Starting a new session stops the old session's subagents. Each one still gets
// its end hook exactly once, from the snapshot that started it.
func TestSubagentEndRunsOnceWhenSessionSwitches(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-stuck"}`)
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local zel = require("zel")
zel.on("subagent_start", function(ev) state.set("start." .. ev.id, "ok", "project") end)
zel.on("subagent_end", function(ev)
	state.set("end." .. ev.id, (state.get("end." .. ev.id, "project") or "") .. ev.status .. ";", "project")
end)`))
	t.Cleanup(func() { close(gate) })
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	awaitStored(t, a, "start.a1", "ok")
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	awaitStored(t, a, "end.a1", "stopped;")
	time.Sleep(50 * time.Millisecond) // a duplicate end hook would have landed by now
	if got := storedString(t, a, "end.a1"); got != "stopped;" {
		t.Fatalf("end hook ran more than once: %q", got)
	}
}

// Shutdown has one budget. A start hook that outlives it does not hold Close,
// even though the subagent it belongs to finished before shutdown began.
func TestShutdownBoundsSlowSubagentHooks(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case childMarker.MatchString(body):
			return textReply("answer to: " + childMarker.FindString(body))
		}
		return luaReply("c1", `agents.spawn{prompt = "child-ping"}`)
	}}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local zel = require("zel")
zel.on("subagent_start", function(ev) state.set("started", "1", "project"); sleep(30) end)
zel.on("subagent_end", function(ev) sleep(30) end)`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	awaitStored(t, a, "started", "1")
	start := time.Now()
	a.Close()
	if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
		t.Fatalf("shutdown took %s: a subagent hook outlived the session budget", took)
	}
}

// A running subagent is stopped at shutdown, and its end hook, which is slow too,
// runs within the same budget instead of adding its own.
func TestShutdownBoundsRunningSubagentAndItsEndHook(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-stuck"}`)
	}}
	var endFile string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		endFile = filepath.Join(dir, "ended.txt")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
local zel = require("zel")
zel.on("subagent_end", function(ev) fs.write("`+endFile+`", ev.status); sleep(30) end)`)
	})
	t.Cleanup(func() { close(gate) })
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(a.ActiveSubagents()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the subagent never started")
		}
	}
	start := time.Now()
	a.Close()
	if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
		t.Fatalf("shutdown took %s: the subagent or its end hook outlived the budget", took)
	}
	if got, err := os.ReadFile(endFile); err != nil || string(got) != "stopped" {
		t.Fatalf("the end hook did not run for the stopped subagent: %q, %v", got, err)
	}
}

// A job that ignores cancellation is abandoned when the shutdown budget ends,
// so a stuck job cannot hold the session open.
func TestShutdownAbandonsJobThatIgnoresCancel(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	_, err := a.session().tasks.start("stuck", startOpts{}, func(io.Writer, *job) (*proc, error) {
		return &proc{cancel: func() {}, term: func() {}, wait: func() error { <-never; return nil }, exit: func() int { return 0 }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a.Close()
	if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
		t.Fatalf("shutdown took %s waiting for a job that ignores cancellation", took)
	}
}

// Expanding a prompt ends with its caller: a cancelled context stops a slow
// completion and returns no text, and a session closed meanwhile ends it too.
func TestPromptExpansionStopsWithItsCaller(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
require("zel").complete("slow", function() sleep(30); return "never" end)`))
	ctl := NewController(a, func(Event) {})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if out, err := a.ExpandPrompt(ctx, "do #slow"); err == nil || out != "" || time.Since(start) > 5*time.Second {
		t.Fatalf("headless expansion: %q, %v after %s", out, err, time.Since(start))
	}
	if out, err := ctl.ExpandPrompt(ctx, "do #slow"); err == nil || out != "" {
		t.Fatalf("controller expansion after cancel: %q, %v", out, err)
	}
	// Closing the session while a completion runs is race-free and ends the call.
	running, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := ctl.ExpandPrompt(running, "#slow")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	a.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an expansion of a closed session returned text")
		}
	case <-time.After(luart.SessionEndTimeout + 3*time.Second):
		t.Fatal("the expansion outlived the session")
	}
}

// A start hook that raises an error does not change the lifecycle: the child still
// ends, its timeout is reported to the end hook, and the end hook runs exactly once.
func TestSubagentTimeoutWithFailingStartHookEndsOnce(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-stuck", timeout = 0.2}`)
	}}
	var endFile string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		endFile = filepath.Join(dir, "ends.txt")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
local zel = require("zel")
zel.on("subagent_start", function(ev) error("start hook failed on purpose") end)
zel.on("subagent_end", function(ev) fs.write("`+endFile+`", ev.status .. ";", {append = true}) end)`)
	})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if got, err := os.ReadFile(endFile); err == nil && len(got) > 0 {
			if string(got) != "timeout;" {
				t.Fatalf("end hook ran with %q, want one timeout", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the end hook never ran for the timed-out subagent")
		}
	}
	release()
	time.Sleep(50 * time.Millisecond) // a duplicate end hook would have landed by now
	if got, _ := os.ReadFile(endFile); string(got) != "timeout;" {
		t.Fatalf("end hook ran more than once: %q", got)
	}
}

// namedPipe makes a FIFO. Reading it from Lua is a host call that blocks until a
// writer opens it, which is how a stuck host operation looks from the session.
func namedPipe(t *testing.T, path string) string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// releasePipe gives the reader blocked on the FIFO its writer, so its call returns.
// It waits for the reader to arrive, so it runs on a goroutine of its own.
func releasePipe(path string) {
	if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		w.Close()
	}
}

// awaitSubagentRunning waits until a subagent of a runs.
func awaitSubagentRunning(t *testing.T, a *Runtime) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(a.ActiveSubagents()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the subagent never started")
		}
	}
}

// stuckChildProvider runs the parent's spawn of a child that only ends when it is
// cancelled, as a cooperative subagent does.
func stuckChildProvider(gate chan struct{}) *fakeProvider {
	return &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `agents.spawn{prompt = "child-stuck"}`)
	}}
}

// A slow session_end must not cost a cooperative subagent its end hook. The subagent
// is cancelled first and its end hook runs while the budget lasts; session_end runs
// after it with what is left. Quitting and switching sessions order them alike.
func TestSlowSessionEndDoesNotDropFastSubagentEnd(t *testing.T) {
	for _, leave := range []struct {
		name string
		do   func(*Runtime) error
	}{
		{"quit", func(a *Runtime) error { a.Close(); return nil }},
		{"new session", func(a *Runtime) error { return a.NewSession() }},
	} {
		t.Run(leave.name, func(t *testing.T) {
			gate := make(chan struct{})
			var endFile, seenFile string
			a := newTestAgentWith(t, stuckChildProvider(gate), Options{}, func(home, dir string) {
				endFile = filepath.Join(dir, "ended.txt")
				seenFile = filepath.Join(dir, "seen.txt")
				writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
local zel = require("zel")
zel.on("subagent_end", function(ev) fs.write("`+endFile+`", ev.status) end)
zel.on("session_end", function()
	if session.depth ~= 0 then return end
	fs.write("`+seenFile+`", tostring(fs.exists("`+endFile+`")))
	sleep(30)
end)`)
			})
			t.Cleanup(func() { close(gate) }) // after the agent: its server closes last
			if _, err := turn(t, a, "go"); err != nil {
				t.Fatal(err)
			}
			awaitSubagentRunning(t, a)
			start := time.Now()
			if err := leave.do(a); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
				t.Fatalf("leaving took %s: session_end outlived the budget", took)
			}
			if got, err := os.ReadFile(endFile); err != nil || string(got) != "stopped" {
				t.Fatalf("the end hook of the cancelled subagent was lost behind session_end: %q, %v", got, err)
			}
			if got, err := os.ReadFile(seenFile); err != nil || string(got) != "true" {
				t.Fatalf("session_end ran before the end hook of the subagent: %q, %v", got, err)
			}
		})
	}
}

// A subagent_start hook still running when the shutdown begins is cancelled with the
// subagent, so it cannot hold up the subagent's end hook.
func TestShutdownCancelsPendingSubagentStart(t *testing.T) {
	gate := make(chan struct{})
	var endFile string
	a := newTestAgentWith(t, stuckChildProvider(gate), Options{}, func(home, dir string) {
		endFile = filepath.Join(dir, "ended.txt")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
local zel = require("zel")
zel.on("subagent_start", function() state.set("started", "1", "project"); sleep(30) end)
zel.on("subagent_end", function(ev) fs.write("`+endFile+`", ev.status) end)`)
	})
	t.Cleanup(func() { close(gate) }) // after the agent: its server closes last
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	awaitStored(t, a, "started", "1")
	start := time.Now()
	a.Close()
	if took := time.Since(start); took > luart.SessionEndTimeout/2 {
		t.Fatalf("shutdown took %s: the pending start hook held the end hook", took)
	}
	if got, err := os.ReadFile(endFile); err != nil || string(got) != "stopped" {
		t.Fatalf("the end hook did not run: %q, %v", got, err)
	}
}

// A subagent_end hook stuck in a host read that never returns does not hold shutdown:
// Close returns at the budget, and the hook that did not finish is logged.
func TestShutdownStuckSubagentEndHookIsBounded(t *testing.T) {
	gate := make(chan struct{})
	var fifo string
	a := newTestAgentWith(t, stuckChildProvider(gate), Options{}, func(home, dir string) {
		fifo = namedPipe(t, filepath.Join(dir, "stuck"))
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
require("zel").on("subagent_end", function() io.open("`+fifo+`", "r"):read("a") end)`)
	})
	t.Cleanup(func() { close(gate) }) // after the agent: its server closes last
	t.Cleanup(func() { go releasePipe(fifo) })
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	awaitSubagentRunning(t, a)
	if !closeWithin(a, luart.SessionEndTimeout+3*time.Second) {
		t.Fatal("shutdown waited past its budget for a stuck subagent end hook")
	}
	// The hook gives up at the same moment the shutdown does, so its entry follows Close.
	var events []byte
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		events, _ = os.ReadFile(EventsLog(a.Home(), a.SessionID()))
		if strings.Contains(string(events), "stopped early") || time.Now().After(deadline) {
			break
		}
	}
	if !strings.Contains(string(events), "subagent_end") || !strings.Contains(string(events), "stopped early") {
		t.Fatalf("the skipped end hook is not logged: %q", events)
	}
}

// A completion stuck in a host read ends with its caller, and closing the session does
// not wait for its VM: the VM is freed once the blocked read returns.
func TestStuckCompletionDoesNotHoldSessionClose(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	var fifo string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		fifo = namedPipe(t, filepath.Join(dir, "stuck"))
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
require("zel").complete("stuck", function() return io.open("`+fifo+`", "r"):read("a") end)`)
	})
	t.Cleanup(func() { go releasePipe(fifo) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if out, err := a.ExpandPrompt(ctx, "do #stuck"); err == nil || out != "" || time.Since(start) > 5*time.Second {
		t.Fatalf("expansion of a stuck completion: %q, %v after %s", out, err, time.Since(start))
	}
	if !closeWithin(a, 2*time.Second) {
		t.Fatal("closing the session waited for a stuck completion")
	}
}

// closeWithin closes a and reports whether that returned within d.
func closeWithin(a *Runtime, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		a.Close()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// A deadline-expired subagent_end hook must never race with session replacement
// (NewSession or Resume) or route warnings into the new session's log. Both a
// single cooperative handler and a 200-handler burst must isolate warnings to
// the old session and clean up the abandoned work cleanly.
func TestDeadlineExpiredSubagentEndHookAcrossSessionSwitch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
		copies int
	}{
		{"new/single", false, 1},
		{"new/200", false, 200},
		{"resume/single", true, 1},
		{"resume/200", true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{})
			initCode := fmt.Sprintf(`
for i = 1, %d do
	require("zel").on("subagent_end", function() sleep(30) end)
end`, tc.copies)
			a := newTestAgentWith(t, stuckChildProvider(gate), Options{}, initLua(t, initCode))
			t.Cleanup(func() { close(gate) })

			var targetID string
			if tc.resume {
				if err := a.Save(); err != nil {
					t.Fatal(err)
				}
				targetID = a.SessionID()
				if err := a.NewSession(); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := turn(t, a, "go"); err != nil {
				t.Fatal(err)
			}
			awaitSubagentRunning(t, a)

			oldSess := a.session()
			oldID := a.SessionID()
			oldCfg := oldSess.lua.get()
			if oldCfg == nil {
				t.Fatal("missing old config snapshot")
			}

			start := time.Now()
			var err error
			if tc.resume {
				err = a.Resume(targetID)
			} else {
				err = a.NewSession()
			}
			if err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
				t.Fatalf("session switch took %s past shutdown budget", took)
			}

			newID := a.SessionID()
			if newID == oldID {
				t.Fatalf("session ID did not change: %s", newID)
			}

			// Eventual cleanup of the abandoned job goroutine and pinned config snapshot.
			drained := make(chan struct{})
			go func() {
				oldSess.tasks.wg.Wait()
				close(drained)
			}()
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("abandoned subagent_end / job did not finish cleaning up")
			}
			oldSess.lua.mu.Lock()
			pins := len(oldSess.lua.pins)
			oldSess.lua.mu.Unlock()
			if pins != 0 {
				t.Fatalf("old config snapshot retained %d pins after drain", pins)
			}

			a.LogEvent("notice", "in-new")

			var oldEvents []byte
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				oldEvents, _ = os.ReadFile(EventsLog(a.Home(), oldID))
				if strings.Contains(string(oldEvents), "stopped early") || time.Now().After(deadline) {
					break
				}
			}
			if !strings.Contains(string(oldEvents), "subagent_end") || !strings.Contains(string(oldEvents), "stopped early") {
				t.Fatalf("old session %s missing subagent_end warning: %q", oldID, oldEvents)
			}
			if strings.Contains(string(oldEvents), "in-new") {
				t.Fatalf("old session %s received new session notice: %q", oldID, oldEvents)
			}
			newEvents, _ := os.ReadFile(EventsLog(a.Home(), newID))
			if strings.Contains(string(newEvents), "subagent_end") || strings.Contains(string(newEvents), "stopped early") {
				t.Fatalf("new session %s received old subagent_end warning: %q", newID, newEvents)
			}
			if !strings.Contains(string(newEvents), "in-new") {
				t.Fatalf("new session %s missing its own notice: %q", newID, newEvents)
			}
		})
	}
}

// An abandoned non-cooperative subagent_end hook survives session switch while
// pinned to its old session, routes later warnings strictly to the old session,
// and releases its task waiter and config snapshot once unblocked.
func TestAbandonedSubagentEndHookCleansUpAfterReleaseAcrossSessionSwitch(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "new"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			gate := make(chan struct{})
			var fifo string
			a := newTestAgentWith(t, stuckChildProvider(gate), Options{}, func(home, dir string) {
				fifo = namedPipe(t, filepath.Join(dir, "stuck-end"))
				writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
local zel = require("zel")
zel.on("subagent_end", function() io.open("`+fifo+`", "r"):read("a") end)
for i = 1, 20 do
	zel.on("subagent_end", function() sleep(30) end)
end`)
			})
			t.Cleanup(func() { close(gate) })
			release := sync.OnceFunc(func() { go releasePipe(fifo) })
			t.Cleanup(release)

			var targetID string
			if resume {
				if err := a.Save(); err != nil {
					t.Fatal(err)
				}
				targetID = a.SessionID()
				if err := a.NewSession(); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := turn(t, a, "go"); err != nil {
				t.Fatal(err)
			}
			awaitSubagentRunning(t, a)

			oldSess := a.session()
			oldID := a.SessionID()
			oldCfg := oldSess.lua.get()

			start := time.Now()
			var err error
			if resume {
				err = a.Resume(targetID)
			} else {
				err = a.NewSession()
			}
			if err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > luart.SessionEndTimeout+3*time.Second {
				t.Fatalf("session switch took %s past shutdown budget", took)
			}
			newID := a.SessionID()
			if newID == oldID {
				t.Fatalf("session ID did not change")
			}

			// Still pinned until the blocked hook is released.
			oldSess.lua.mu.Lock()
			pinsBefore := oldSess.lua.pins[oldCfg]
			oldSess.lua.mu.Unlock()
			if pinsBefore != 1 {
				t.Fatalf("pins before release = %d, want 1", pinsBefore)
			}

			release()
			drained := make(chan struct{})
			go func() {
				oldSess.tasks.wg.Wait()
				close(drained)
			}()
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("abandoned subagent_end did not drain after release")
			}
			oldSess.lua.mu.Lock()
			pinsAfter := len(oldSess.lua.pins)
			oldSess.lua.mu.Unlock()
			if pinsAfter != 0 {
				t.Fatalf("pins after release = %d, want 0", pinsAfter)
			}

			oldEvents, _ := os.ReadFile(EventsLog(a.Home(), oldID))
			if !strings.Contains(string(oldEvents), "subagent_end") || !strings.Contains(string(oldEvents), "stopped early") {
				t.Fatalf("old session %s missing subagent_end warnings: %q", oldID, oldEvents)
			}
			newEvents, _ := os.ReadFile(EventsLog(a.Home(), newID))
			if strings.Contains(string(newEvents), "subagent_end") || strings.Contains(string(newEvents), "stopped early") {
				t.Fatalf("new session %s leaked old subagent_end warnings: %q", newID, newEvents)
			}
		})
	}
}

// Cancelling a session.rename call blocked on SQLite's write lock must clear the
// core Runtime's foreground Activity immediately when runLua returns, keep it clear
// across a subsequent call, and keep later activities intact when the lock releases.
func TestAbandonedHostCallClearsCoreActivity(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	locker, err := openDB(a.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rolledBack := false
	rollback := func() {
		if !rolledBack {
			rolledBack = true
			_ = tx.Rollback()
		}
	}
	defer rollback()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan luart.Result, 1)
	go func() {
		done <- a.runLua(ctx, `session.rename("locked")`)
	}()
	for deadline := time.Now().Add(5 * time.Second); a.Activity() != "renaming session"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never observed renaming session activity (got %q)", a.Activity())
		}
	}
	cancel()

	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Output, luart.ECANCELED) {
			t.Fatalf("abandoned runLua = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runLua did not return promptly after cancel")
	}
	if got := a.Activity(); got != "" {
		t.Fatalf("Activity after abandoned call = %q, want empty", got)
	}

	next := a.runLua(context.Background(), "return 42")
	if next.IsError || !strings.HasSuffix(next.Output, "\n42") {
		t.Fatalf("next call = %+v", next)
	}
	if got := a.Activity(); got != "" {
		t.Fatalf("Activity after next call = %q, want empty", got)
	}

	// Start a third call holding a live activity, then release the SQLite write lock:
	// the late return of session.rename must not remove the third call's activity.
	call3 := make(chan luart.Result, 1)
	go func() {
		call3 <- a.runLua(context.Background(), "sleep(0.25); return 'ok'")
	}()
	for deadline := time.Now().Add(5 * time.Second); a.Activity() != "sleeping 0.25 sec"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never observed sleeping activity (got %q)", a.Activity())
		}
	}
	rollback()
	time.Sleep(50 * time.Millisecond)
	if got := a.Activity(); got != "sleeping 0.25 sec" {
		t.Fatalf("late host return removed later activity: %q", got)
	}
	select {
	case res := <-call3:
		if res.IsError || res.Output != "ok" {
			t.Fatalf("call 3 = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call 3 did not finish")
	}
	if got := a.Activity(); got != "" {
		t.Fatalf("Activity after call 3 = %q, want empty", got)
	}
}

// Surviving environment references (from abandoned hooks, tasks, or VMs) keep
// their own session identity across NewSession and Close, never mutate or log
// to the replacement session, and fail cleanly when closed.
func TestSurvivingEnvironmentWorkIsolatedAcrossSessionSwitch(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	var notices []string
	var noticeMu sync.Mutex
	a.SetNoticeHandler(func(msg string) {
		noticeMu.Lock()
		notices = append(notices, msg)
		noticeMu.Unlock()
	})

	oldEnv := luaEnv{a.session()}
	oldID := a.SessionID()

	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	newID := a.SessionID()
	if newID == oldID {
		t.Fatalf("session did not switch: %s", newID)
	}

	// Old environment retains old identity while Runtime points to the new session.
	if got := oldEnv.Info().ID; got != oldID {
		t.Fatalf("oldEnv.Info().ID = %q, want %q", got, oldID)
	}
	if got := oldEnv.ShellEnv()["ZEL_SESSION_ID"]; got != oldID {
		t.Fatalf("oldEnv.ShellEnv = %q, want %q", got, oldID)
	}
	if got := a.Shell.Env()["ZEL_SESSION_ID"]; got != newID {
		t.Fatalf("a.Shell.Env = %q, want %q", got, newID)
	}

	// Session-scoped KV stays isolated.
	if err := oldEnv.State().Set("session", "from_old", "1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := (luaEnv{a.session()}).State().Get("session", "from_old"); err != nil || ok {
		t.Fatalf("new session saw old session KV: ok=%v err=%v", ok, err)
	}
	if got, ok, err := oldEnv.State().Get("session", "from_old"); err != nil || !ok || got != "1" {
		t.Fatalf("old session KV = %q, %v, %v", got, ok, err)
	}

	// Closed old environment cannot spawn tasks/agents, attach views, or rename.
	if _, err := oldEnv.Tasks().Spawn(func(context.Context, io.Writer) (any, error) { return nil, nil }, luart.TaskOpts{Name: "late"}); err == nil {
		t.Fatal("oldEnv.Tasks().Spawn succeeded after session switch")
	}
	if _, err := oldEnv.Agents().Spawn(luart.AgentOpts{Prompt: "late"}); err == nil {
		t.Fatal("oldEnv.Agents().Spawn succeeded after session switch")
	}
	if err := oldEnv.View(filepath.Join(a.Home(), "missing.png")); err == nil || !strings.Contains(err.Error(), "session has ended") {
		t.Fatalf("oldEnv.View = %v, want session has ended", err)
	}
	if err := oldEnv.Rename("hijack"); err == nil || !strings.Contains(err.Error(), "session has ended") {
		t.Fatalf("oldEnv.Rename = %v, want session has ended", err)
	}
	if a.RunningTasks() != 0 || len(a.ActiveSubagents()) != 0 || len(a.Queue().Take()) != 0 || a.CustomTitle() != "" {
		t.Fatal("old environment mutated new session state")
	}

	// Logs and notices route to the old session's log; UI notice handler is detached.
	oldEnv.Log("warn", "late-old-log")
	oldEnv.Notice("late-old-notice")
	noticeMu.Lock()
	gotNotices := len(notices)
	noticeMu.Unlock()
	if gotNotices != 0 {
		t.Fatalf("old notice fired UI handler after session switch: %v", notices)
	}
	oldEvents, _ := os.ReadFile(EventsLog(a.Home(), oldID))
	if !strings.Contains(string(oldEvents), "late-old-log") || !strings.Contains(string(oldEvents), "late-old-notice") {
		t.Fatalf("old events log missing entries: %q", oldEvents)
	}
	newEvents, _ := os.ReadFile(EventsLog(a.Home(), newID))
	if strings.Contains(string(newEvents), "late-old-log") || strings.Contains(string(newEvents), "late-old-notice") {
		t.Fatalf("new events log leaked old entries: %q", newEvents)
	}

	// Old activity does not affect the new session's activity indicator.
	endOldAct := oldEnv.Begin("old-work", false)
	if got := a.Activity(); got != "" {
		t.Fatalf("new session Activity polluted by oldEnv: %q", got)
	}
	endOldAct()

	// Closing the runtime (twice) leaves DB calls from oldEnv safe (error, no panic/race).
	a.Close()
	a.Close()
	if err := oldEnv.State().Set("session", "after_close", "1"); err == nil {
		t.Fatal("expected DB closed error after Runtime.Close")
	}
	oldEnv.Log("warn", "after-close-log")
}
