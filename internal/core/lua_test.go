package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	if _, done, _ := (luaEnv{a}).Tasks().Wait(context.Background(), "t1", 100*time.Millisecond); done || listings() != 1 {
		t.Fatalf("task finished %v, listings %d", done, listings())
	}
	open()
	if _, done, err := (luaEnv{a}).Tasks().Wait(context.Background(), "t1", 5*time.Second); !done || err != nil {
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
	a.depth = MaxAgentDepth
	if _, err := (luaEnv{a}).Agents().Spawn(luart.AgentOpts{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Fatalf("depth limit: %v", err)
	}
	a.depth = 0
	id, err := (luaEnv{a}).Agents().Spawn(luart.AgentOpts{Prompt: "child-hang"})
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
	res, ok, err := (luaEnv{a}).Tasks().Result("t1")
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
	if a.lua.get().LoadErrs != nil {
		t.Fatalf("load errors: %v", a.lua.get().LoadErrs)
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
	a := &Runtime{acts: &activities{}}
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
	endBackground := luaEnv{a}.Begin("sleeping 60 sec", true)
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
	child := &Runtime{tree: tree, acts: &activities{}, link: &agentLink{tree: tree, root: "s", id: "s/a1"}}
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
