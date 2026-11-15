package core

import (
	"context"
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
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `return {hooks = {message = function(text) return text:upper() end}}`))
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
return {hooks = {message = function(text, notice)
 if notice then
  write("observed_notice", text)
  return "DO-NOT-REWRITE"
 end
end}}`))
	input := notification("t1 finished")
	got := a.hookInput(context.Background(), input)
	if got.Text != input.Text || !got.Notice {
		t.Fatalf("notification changed: %+v", got)
	}
	if value := readSessionFile(t, a, "observed_notice"); !strings.Contains(value, "t1 finished") {
		t.Fatalf("observed notice = %q", value)
	}
}

func TestTurnEndHookInjectsOnceAndIsBounded(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `
local n = 0
return {hooks = {turn_end = function(answer, running)
  n = n + 1
  if n == 1 and #running == 0 then return "CHECK-AGAIN" end
end}}`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "CHECK-AGAIN") {
		t.Fatalf("requests %d: %v", len(reqs), reqs)
	}

	fp2 := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	b := newTestAgentWith(t, fp2, Options{}, initLua(t, `return {hooks = {turn_end = function() return "AGAIN" end}}`))
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
		return luaReply("c1", `local m, f = models(); models()
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
	if res := a.runLua(context.Background(), `return tasks.spawn("list", function() return #models() end)`); res.IsError {
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
	go func() { answered <- a.runLua(ctx, `return #models()`) }()
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
	if res := a.runLua(context.Background(), `return #models()`); res.IsError || res.Output != "1" || listings() != 1 {
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
		res := a.runLua(context.Background(), `local m, f = models(); return #m .. " " .. tostring(f.dead ~= nil)`)
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
	a := newTestAgentWith(t, fp, Options{}, initLua(t, `return {hooks = {session_end = function(reason) write("ended", reason) end}}`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[2]; !strings.Contains(req, "kept-7") {
		t.Fatalf("globals did not survive: %s", req)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if v := readSessionFile(t, a, "ended"); v != "new" {
		t.Fatalf("session_end: %q", v)
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

func TestAgentInlineAndInATask(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // agents run in-process: no zel executable needed
	fp := agentProvider(`
local r = agent{prompt = "child-ping"}
print(r.ok, r.answer, r.session ~= "")
local id = tasks.spawn("pong", function() return agent{prompt = "child-pong"} end)
local res = tasks.wait(id, 8)
print(res.ok, res.value.answer, tasks.get(id).state, #tasks.list())`, nil)
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[len(fp.requests())-1]
	for _, want := range []string{"true\\tanswer to: child-ping\\ttrue", "true\\tanswer to: child-pong\\tdone\\t1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	// Waited jobs never queue a <agent-result> message.
	time.Sleep(100 * time.Millisecond)
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("waited results were queued too: %+v", got)
	}
	if got := a.ActiveSubagents(); len(got) != 0 {
		t.Fatalf("finished agents still active: %+v", got)
	}
}

func TestUnwaitedAgentResultArrivesAsMessage(t *testing.T) {
	fp := agentProvider(`tasks.spawn("later", function() return agent{prompt = "child-later"} end)`, nil)
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	got := awaitInput(t, a)
	if got[0].Text != "<notification>t1 finished</notification>" {
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
			return luaReply("n1", `print(select(2, pcall(agent, {prompt = "child-inner"})))`)
		case strings.Contains(body, "child-outer") && strings.Contains(body, "EDENIED"):
			return textReply("outer was denied")
		case strings.Contains(body, "child-outer"):
			return textReply("outer saw inner")
		case strings.Contains(body, "child-inner"):
			return textReply("inner answer")
		}
		return luaReply("c1", `
local nested = tasks.spawn("outer", function() return agent{prompt = "child-outer"} end)
local stuck = tasks.spawn("stuck", function() return agent{prompt = "child-stuck"} end)
local r = tasks.wait({nested}, 8)
print(r[1].value.ok, r[1].value.answer)
tasks.stop(stuck)
print(tasks.wait(stuck, 8).ok, tasks.get(stuck).state)`)
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
	// Stopping the task stops the agent it waited for.
	for deadline := time.Now().Add(5 * time.Second); len(a.ActiveSubagents()) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the stuck agent outlived its task: %+v", a.ActiveSubagents())
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
		return luaReply("c1", `tasks.spawn("hang", function() return agent{prompt = "child-hang"} end)`)
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
			return luaReply("c1", `local tag = "v1"; tasks.spawn("k", function() sh("sleep 0.2"); return tag end)`)
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
		writeFile(t, path, `return {instructions = "FIRST"}`)
	})
	writeFile(t, path, `return {instructions = "SECOND"}`)
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
		writeFile(t, path, `return {instructions = "GOOD"}`)
	})
	writeFile(t, path, `error("boom"); return {instructions = "NEW"}`)
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if req := fp.requests()[0]; !strings.Contains(req, "GOOD") || strings.Contains(req, "NEW") {
		t.Fatalf("old snapshot lost: %s", req)
	}
	writeFile(t, path, `return {instructions = "FIXED"}`)
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
	writeFile(t, filepath.Join(bin, "ddgs"), "#!/bin/sh\necho '1. T http://x'\n")
	if err := os.Chmod(filepath.Join(bin, "ddgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", `print(web.search{q = "q", n = 1})`)
		}
		return textReply("done")
	}}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		writeFile(t, filepath.Join(home, "init.lua"), `return {dofile(os.getenv("ZEL_HOME_TEST") .. "/global.lua"), require("web")}`)
		writeFile(t, filepath.Join(home, "global.lua"), read("global-init.lua"))
		writeFile(t, filepath.Join(home, "lua", "web.lua"), read("web-plugin.lua"))
		t.Setenv("ZEL_HOME_TEST", home)
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "AGENTS-TEXT")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), read("project-init.lua"))
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	req := fp.requests()[0]
	for _, want := range []string{"web.search{...}  -- DuckDuckGo", "web.fetch{...}", "zel.check{...}  -- start gofmt", "AGENTS-TEXT", "git status:", "today: "} {
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
		return luaReply("c1", "local h = tasks.spawn('bg', function() sh('sleep 30') end)\nwrite('spawned', h)\nwhile true do end")
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
	_ = (luaEnv{a.session()}).Tasks().Stop("t1")
}

// readSessionFile reads a file a hook or a call wrote in the session directory ("" when missing).
func readSessionFile(t *testing.T, a *Runtime, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(a.Shell.Dir, name))
	return string(data)
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
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `return {completions = {plan = function() return "first plan" end}}`)
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
	writeFile(t, filepath.Join(projectDir, ".zel", "init.lua"), `return {completions = {plan = function() return "second plan" end}}`)
	ctl.Reload()
	if got, err := ctl.ExpandPrompt(context.Background(), "#plan"); err != nil || got != "second plan" {
		t.Fatalf("after reload ExpandPrompt = %q, %v", got, err)
	}
}

// A running subagent is stopped at shutdown, and a slow session_end runs within
// the same budget instead of adding its own.
func TestShutdownBoundsRunningSubagentAndSessionEnd(t *testing.T) {
	gate := make(chan struct{})
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-stuck"):
			<-gate
			return textReply("late")
		}
		return luaReply("c1", `tasks.spawn("stuck", function() return agent{prompt = "child-stuck"} end)`)
	}}
	var endFile string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		endFile = filepath.Join(dir, "ended.txt")
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
return {hooks = {session_end = function(reason) write("`+endFile+`", reason); while true do end end}}`)
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
		t.Fatalf("shutdown took %s: the subagent or session_end outlived the budget", took)
	}
	if got, err := os.ReadFile(endFile); err != nil || string(got) != "quit" {
		t.Fatalf("session_end did not run: %q, %v", got, err)
	}
	if len(a.ActiveSubagents()) != 0 {
		t.Fatal("the subagent outlived the session")
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
return {completions = {slow = function() while true do end end}}`))
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

// A completion stuck in a host read ends with its caller, and closing the session does
// not wait for its VM: the VM is freed once the blocked read returns.
func TestStuckCompletionDoesNotHoldSessionClose(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	var fifo string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		fifo = namedPipe(t, filepath.Join(dir, "stuck"))
		writeFile(t, filepath.Join(dir, ".zel", "init.lua"), `
return {completions = {stuck = function() return io.open("`+fifo+`", "r"):read("a") end}}`)
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
		call3 <- a.runLua(context.Background(), "sh('sleep 0.25'); return 'ok'")
	}()
	for deadline := time.Now().Add(5 * time.Second); a.Activity() != "$ sleep 0.25"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never observed sleeping activity (got %q)", a.Activity())
		}
	}
	rollback()
	time.Sleep(50 * time.Millisecond)
	if got := a.Activity(); got != "$ sleep 0.25" {
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

	// Closing the runtime (twice) leaves calls from oldEnv safe (no panic/race).
	a.Close()
	a.Close()
	oldEnv.Log("warn", "after-close-log")
}

// The working indicator keeps the last action after it ended, until a new turn.
func TestLastActivityStaysUntilReset(t *testing.T) {
	a := &Runtime{cur: &session{acts: &activities{}}}
	if got := a.LastActivity(); got != "" {
		t.Fatalf("before any action: %q", got)
	}
	a.beginActivity("reading a.txt")()
	if a.Activity() != "" || a.LastActivity() != "reading a.txt" {
		t.Fatalf("activity %q, last %q", a.Activity(), a.LastActivity())
	}
	end := a.beginActivity("editing a.txt")
	end()
	if got := a.LastActivity(); got != "editing a.txt" {
		t.Fatalf("newest action must replace the last one: %q", got)
	}
	a.ResetActivity()
	if got := a.LastActivity(); got != "" {
		t.Fatalf("after reset: %q", got)
	}
}
