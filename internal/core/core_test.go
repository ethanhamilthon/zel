package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"zel/internal/config"
	"zel/internal/ipc"
)

func TestContextProjectionInvalidation(t *testing.T) {
	s := &session{History: []json.RawMessage{raw(map[string]any{"role": "user", "content": "first"})}}
	initial := s.ContextTokens()
	if again := s.ContextTokens(); again != initial {
		t.Fatal("unstable cached context")
	}
	s.History[0] = raw(map[string]any{"role": "user", "content": strings.Repeat("longer ", 100)})
	if got := s.ContextTokens(); got <= initial {
		t.Fatal("replacement used stale cache")
	}
	s.History = append(s.History, callOutput("c", strings.Repeat("output\n", 100)))
	s.compacted = compaction{Prune: 2, Drop: 1}
	if got, want := s.ContextTokens(), estimateTokens(s.requestInput()); got != want {
		t.Fatalf("compaction %d != %d", got, want)
	}
	s.History = s.History[:1]
	if got, want := s.ContextTokens(), estimateTokens(s.requestInput()); got != want {
		t.Fatalf("truncate %d != %d", got, want)
	}
	n := int64(42)
	s.lastRequest.ContextInput = &n
	s.requestedLen = 1
	s.History = append(s.History, callOutput("c", "new output"))
	if got, want := s.ContextTokens(), n+estimateTokens(s.History[1:]); got != want {
		t.Fatalf("usage %d != %d", got, want)
	}
}

// The trace pairs a bash call with its output, numbers the turns and the model
// requests, reads failures and puts logged events in the turn they happened in.
func TestTraceEntries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	call := func(id, cmd string) json.RawMessage {
		return raw(map[string]any{"type": "function_call", "name": "lua", "call_id": id, "arguments": `{"code":"` + cmd + `"}`})
	}
	usage := &Usage{Input: 1000, Output: 50, Cached: 800}
	s := &session{History: []json.RawMessage{
		raw(map[string]any{"type": "instructions", "text": "system"}),
		raw(map[string]any{"role": "user", "content": "first"}),
		raw(map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "thinking"}}}),
		call("a", "ls()"), call("b", "false"),
		callOutput("b", "error: boom"), callOutput("a", "files\nhook line"),
		raw(map[string]any{"role": "assistant", "content": "done"}),
		raw(map[string]any{"role": "user", "content": "second"}),
	}, itemMeta: map[int]*Metadata{
		1: {At: at(1)},
		2: {At: at(2), RequestSeq: 3, TurnSeq: 1, Duration: 2 * time.Second, Usage: usage},
		3: {At: at(2), RequestSeq: 3, TurnSeq: 1, Duration: 2 * time.Second, Usage: usage},
		4: {At: at(2), RequestSeq: 3, TurnSeq: 1, Duration: 2 * time.Second, Usage: usage},
		5: {At: at(3), Duration: 300 * time.Millisecond},
		6: {At: at(3), Duration: 100 * time.Millisecond},
		7: {At: at(5), RequestSeq: 8, TurnSeq: 1, Duration: time.Second},
		8: {At: at(6)},
	}}
	events := []TraceEntry{
		{Kind: "hook", Text: "turn_end: init.lua:3 -> error, 5ms\nboom", At: at(5).Add(time.Millisecond), Turn: -1},
		{Kind: "notice", Text: "later", At: at(9), Turn: -1},
	}
	for i := range events {
		events[i].Status, events[i].Duration = hookResult(events[i].Text)
	}
	got := mergeTrace(s.historyTrace(), events)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, fmt.Sprintf("%s/%d", e.Kind, e.Turn))
	}
	if want := "system/0 user/1 thinking/1 lua/1 lua/1 agent/1 hook/1 user/2 notice/2"; strings.Join(kinds, " ") != want {
		t.Fatalf("entries %s, want %s", strings.Join(kinds, " "), want)
	}
	ls, f := got[3], got[4]
	if ls.Text != "ls()" || ls.Output != "files\nhook line" || ls.Status != "" || ls.Duration != 100*time.Millisecond {
		t.Fatalf("ls: %+v", ls)
	}
	if f.Text != "false" || f.Output != "error: boom" || f.Status != "error" || f.Duration != 300*time.Millisecond {
		t.Fatalf("false: %+v", f)
	}
	think, answer := got[2], got[5]
	if !think.First || ls.First || think.Request != ls.Request || think.Request.N != 1 || think.Request.Usage != usage || think.Request.Duration != 2*time.Second {
		t.Fatalf("the records of a response share its request, the first is marked: %+v %+v", think, ls)
	}
	if !answer.First || answer.Request.N != 2 {
		t.Fatalf("the answer is request 2: %+v", answer)
	}
	if hook := got[6]; hook.Status != "error" || hook.Duration != 5*time.Millisecond {
		t.Fatalf("hook: %+v", hook)
	}
	if st, _ := hookResult("message: init.lua:2 -> blocked, 1ms\nno"); st != "" {
		t.Fatal("a blocking hook blocks on purpose, it is no failure")
	}
	for out, want := range map[string]string{"ok": "", "error: lua: EINVAL: x": "error", "x\n[timeout after 30s; VM reset]": "timeout",
		"error: lua: ECANCELED: stop": "cancelled", "[blocked by a hook: no]": "blocked"} {
		if got := callStatus(out); got != want {
			t.Fatalf("callStatus(%q) = %q, want %q", out, got, want)
		}
	}
}

func BenchmarkUnchangedSave(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.Setenv("ZEL_HOME", b.TempDir())
			a, err := New(b.TempDir(), Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			s := a.session()
			for i := 0; i < n; i++ {
				s.message("user", "small message")
				s.setItemMeta(i, Metadata{At: time.Now()})
			}
			if err := s.Save(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Save(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkImageContext(b *testing.B) {
	s := &session{}
	for i := 0; i < 16; i++ {
		s.History = append(s.History, raw(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "describe image"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("A", 1<<20)},
		}}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.ContextTokens()
	}
}

// withLocal writes text as init.lua before the runtime starts; the test
// provider is added to connect.json.
func withLocal(text string) func(home, dir string) {
	return func(home, dir string) {
		if err := os.WriteFile(config.DefaultPaths(home, dir, "").Init, []byte(text), 0600); err != nil {
			panic(err)
		}
	}
}

// sock sends one request to the session socket, as the zel subcommands do.
func sock(t *testing.T, a *Runtime, op string, args any) ipc.Response {
	t.Helper()
	s := a.session()
	return sockAs(t, s.socketPath(), s.token, op, args)
}

// sockIn is sock from the folder cwd, like a zel command run by a bash call there.
func sockIn(t *testing.T, a *Runtime, cwd, op string, args any) ipc.Response {
	t.Helper()
	s := a.session()
	return sockFrom(t, s.socketPath(), s.token, cwd, op, args)
}

func sockAs(t *testing.T, path, token, op string, args any) ipc.Response {
	t.Helper()
	return sockFrom(t, path, token, "", op, args)
}

func sockFrom(t *testing.T, path, token, cwd, op string, args any) ipc.Response {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := ipc.Call(ctx, path, ipc.Request{Token: token, Op: op, Cwd: cwd, Args: data})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return r
}

func okProvider() *fakeProvider {
	return &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
}

func instructionItems(a *Runtime) int {
	s := a.session()
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	n := 0
	for _, item := range s.History {
		if _, ok := instructionSnapshot(item); ok {
			n++
		}
	}
	return n
}

func TestLuaOutputErrorAndEnvironment(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("return {settings = {env = {GREETING = 'hello'}}}\n"))
	s, ctx := a.session(), context.Background()
	for code, want := range map[string]string{
		"print('a'); return 'b'":     "a\nb",
		"return 1, {x = 2}":          "1\n{\"x\":2}",
		"error('boom', 0)":           "error: boom",
		"return sh('exit 3').code":   "3",
		"return sh('echo fine').out": "fine\n",
	} {
		got := s.runLua(ctx, code)
		if !strings.HasPrefix(got, want) && !strings.Contains(got, want) {
			t.Errorf("%q: got %q, want %q", code, got, want)
		}
	}
	got := s.runLua(ctx, `return sh([[printf '%s\n' "$ZEL_SESSION_ID" "$ZEL_SOCKET" "$ZEL_TOKEN" "$GREETING" "$(pwd -P)" "$PATH"]]).out`)
	lines := strings.Split(got, "\n")
	real, _ := filepath.EvalSymlinks(s.dir)
	if len(lines) < 6 || lines[0] != s.id || lines[1] != s.socketPath() || lines[2] != s.token || lines[3] != "hello" || lines[4] != real {
		t.Fatalf("environment:\n%s", got)
	}
	// The folder of the zel binary comes first, so zel finds itself.
	path := filepath.SplitList(lines[5])
	if len(path) < 2 || strings.Join(path[1:], string(os.PathListSeparator)) != os.Getenv("PATH") {
		t.Fatalf("PATH = %s", lines[5])
	}
}

// runSh runs a shell command through sh() and returns its output as the call's output.
func runSh(s *session, cmd string) string {
	return s.runLua(context.Background(), "return sh("+strconv.Quote(cmd)+").out")
}

func TestLuaOutputCut(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, nil)
	s := a.session()
	// Under the limit: as is, no file.
	if got := runSh(s, "head -c 7000 /dev/zero | tr '\\0' a"); got != strings.Repeat("a", 7000) {
		t.Fatalf("short output changed: %d bytes", len(got))
	}
	if _, err := os.Stat(filepath.Join(s.sessionDir(), "outputs")); err == nil {
		t.Fatal("a file was written for an output under the limit")
	}
	// 13000 bytes: 4000 + marker + 4000, the file holds everything.
	got := runSh(s, "head -c 13000 /dev/zero | tr '\\0' a")
	path := filepath.Join(s.sessionDir(), "outputs", "b1.txt")
	want := strings.Repeat("a", 4000) + "\n\n======== [output cut: 5000 of 13000 bytes omitted]\n" +
		"======== full output: " + path + "\n" +
		"======== read it with slice(read(path), from, count) or grep()\n\n" + strings.Repeat("a", 4000)
	if got != want {
		t.Fatalf("cut output:\n%.200s ... %.300s", got, got[3900:])
	}
	if full, err := os.ReadFile(path); err != nil || len(full) != 13000 {
		t.Fatalf("full output file: %d bytes, %v", len(full), err)
	}
	// The ending stays visible, and the cut keeps whole lines and characters.
	got = runSh(s, "seq 1 3000")
	if !strings.HasSuffix(got, "3000\n") || !strings.HasPrefix(got, "1\n2\n") || !strings.Contains(got, "b2.txt") {
		t.Fatalf("seq: %.80q ... %.80q", got, got[max(0, len(got)-80):])
	}
	got = runSh(s, "for i in $(seq 6000); do printf 'é'; done")
	for _, part := range strings.Split(got, "========") {
		if !utf8.ValidString(part) {
			t.Fatal("a cut split a character")
		}
	}
}

func TestLuaOutputOff(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("return {settings = {output = 0}}\n"))
	if got := runSh(a.session(), "head -c 13000 /dev/zero | tr '\\0' a"); len(got) < 13000 {
		t.Fatalf("output = 0 cut the output: %d bytes", len(got))
	}
	for _, bad := range []float64{-1, 500} {
		if _, err := config.ParseSettings(map[string]any{"output": bad}, config.DefaultSettings()); err == nil {
			t.Fatalf("output = %v accepted", bad)
		}
	}
}

func TestLuaTimeoutAndCancel(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("return {settings = {timeout = 1}}\n"))
	s := a.session()
	if !strings.Contains(s.toolDescription(), "1 s") {
		t.Fatal("the tool description does not say the timeout")
	}
	start := time.Now()
	got := s.runLua(context.Background(), "print('started'); while true do end")
	if !strings.HasPrefix(got, "started\n") || !strings.Contains(got, "timeout after 1s") || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %q after %s", got, time.Since(start))
	}
	if got := s.runLua(context.Background(), "return 'fresh'"); !strings.Contains(got, "[lua: fresh VM]") {
		t.Fatalf("no fresh VM note: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	s.runLua(ctx, "sh('sleep 10')")
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the call")
	}
}

func TestMessageHooksRewriteAndBlock(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal(`return {hooks = {
  { events = {"message"}, fn = function(_, d)
      if d.text:find("secret") then return false, "no secrets" end
      if d.text:find("PING") then return "PONG-REWRITTEN" end
    end },
}}
`))
	if _, err := turn(t, a, "PING please"); err != nil {
		t.Fatal(err)
	}
	if r := fp.requests()[0]; !strings.Contains(r, "PONG-REWRITTEN") || strings.Contains(r, "PING please") {
		t.Fatalf("message not rewritten: %s", r)
	}
	events, err := turn(t, a, "my secret")
	if err != nil || len(fp.requests()) != 1 {
		t.Fatalf("blocked message reached the model: %v, %d requests", err, len(fp.requests()))
	}
	if !slices.ContainsFunc(events, func(e TurnEvent) bool { return e.Kind == "notice" && e.Text == "Message blocked by a hook: no secrets" }) {
		t.Fatalf("no block notice: %+v", events)
	}
	if _, err := turn(t, a, "plain hello"); err != nil {
		t.Fatal(err)
	}
	if r := fp.requests()[1]; !strings.Contains(r, "plain hello") || strings.Contains(r, "my secret") {
		t.Fatalf("unmatched message changed: %s", r)
	}
}

func TestLuaHooks(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`return {hooks = {
  { events = {"lua_before"}, fn = function(_, d)
      if d.code:find("rm %-rf") then return false, "no rm" end
      if d.code:find("^return 'original'") then return "return 'rewritten'" end
      if d.code:find("^error%('broken hook'") then error("bad hook") end
    end },
  { events = {"lua_after"}, fn = function(_, d)
      if d.ok then return "AFTER" end
      return "FAILED:" .. tostring(d.timed_out)
    end },
}}
`))
	s, ctx := a.session(), context.Background()
	for code, want := range map[string]string{
		"return 'original'":      "rewritten\nAFTER",
		"-- rm -rf /no/such/zel": "[blocked by a hook: no rm]",
		"return 'other'":         "other\nAFTER",
		"error('x', 0)":          "error: x\nFAILED:false",
	} {
		if got := s.runLua(ctx, code); got != want {
			t.Errorf("%q: got %q, want %q", code, got, want)
		}
	}
	// A hook that raises is skipped and shows in Tracing.
	if got := s.runLua(ctx, "error('broken hook', 0)"); !strings.Contains(got, "error: broken hook") {
		t.Errorf("call after a failing hook: %q", got)
	}
	if data, _ := os.ReadFile(s.eventsLog()); !strings.Contains(string(data), "lua_before: ") || !strings.Contains(string(data), "bad hook") {
		t.Fatalf("failed hook not logged: %s", data)
	}
}

func TestTurnEndHookContinuesTheTurnAtMostThreeTimes(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal(`return {hooks = {
  { events = {"turn_end"}, fn = function(_, d) if d.answer == "ok" then return "keep going" end end },
}}
`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 1+maxTurnInjections || !strings.Contains(reqs[1], `\u003cnotification\u003ekeep going\u003c/notification\u003e`) {
		t.Fatalf("%d requests; second: %s", len(reqs), reqs[min(1, len(reqs)-1)])
	}
}

func TestNotificationHooksGetPlainTextAndAreWrapped(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`return {hooks = {
  { events = {"notification"}, fn = function(_, d)
      if d.text == "t1 finished" then return d.text .. ", run make test" end
      if d.text == "tagged" then return "<notification>t2 finished</notification>" end
    end },
  { events = {"notification"}, fn = function(_, d) if d.text:sub(1, 1) ~= "<" then return "[" .. d.text .. "]" end end },
}}
`))
	s := a.session()
	got, ok := s.hookInput(context.Background(), notification("t1 finished"), func(TurnEvent) {})
	if !ok || got.Text != "<notification>[t1 finished, run make test]</notification>" || !got.Notice {
		t.Fatalf("plain hook: %+v %v", got, ok)
	}
	// A hook that prints the tags itself is not wrapped again.
	got, ok = s.hookInput(context.Background(), notification("tagged"), func(TurnEvent) {})
	if !ok || got.Text != "<notification>t2 finished</notification>" {
		t.Fatalf("tagged hook: %+v %v", got, ok)
	}
}

func TestConfigFollowsTheMode(t *testing.T) {
	conf := withLocal(`local zel = require("zel")
local plan = false
for _, m in ipairs(zel.session.modes) do if m == "plan" then plan = true end end
return {
  systems = {
    all = "SYSTEM-" .. zel.session.mode,
    plan = plan and "PLAN-ONLY" or "",
  },
  prompts = { hi = "say hi", planned = plan and "PLANNED" or "plain" },
  functions = { ping = { description = "PING-DESC", fn = function() return "pong" end } },
  hooks = {
    { events = {"lua_after"}, fn = function() return zel.session.mode end },
  },
}
`)
	ctx := context.Background()
	main := newTestAgentWith(t, &fakeProvider{}, Options{}, conf)
	headless := newTestAgentWith(t, &fakeProvider{}, Options{Mode: "headless"}, conf)
	plan := newTestAgentWith(t, &fakeProvider{}, Options{Custom: "plan"}, conf)
	if got := main.session().runLua(ctx, "return 'x'"); got != "x\nmain" {
		t.Errorf("main hook: %q", got)
	}
	if got := headless.session().runLua(ctx, "return ping{}"); got != "pong\nheadless" {
		t.Errorf("headless call: %q", got)
	}
	prompt, desc := main.session().assemblePrompt(), main.session().toolDescription()
	if !strings.Contains(prompt, "SYSTEM-main") || strings.Contains(prompt, "PLAN-ONLY") || !strings.Contains(desc, "PING-DESC") {
		t.Errorf("main prompt:\n%s\ntool:\n%s", prompt, desc)
	}
	if prompt := headless.session().assemblePrompt(); !strings.Contains(prompt, "SYSTEM-headless") {
		t.Errorf("headless prompt:\n%s", prompt)
	}
	if prompt := plan.session().assemblePrompt(); !strings.Contains(prompt, "PLAN-ONLY") {
		t.Errorf("plan prompt:\n%s", prompt)
	}
	hi := "<prompt name=\"hi\">\nsay hi\n</prompt>"
	if got, err := main.ExpandPrompt(ctx, "#hi and #unknown, #hi"); err != nil || got != hi+" and #unknown, "+hi {
		t.Errorf("expansion: %q %v", got, err)
	}
	if got, _ := plan.ExpandPrompt(ctx, "#planned"); !strings.Contains(got, "PLANNED") {
		t.Errorf("plan prompt text: %q", got)
	}
}

func TestControllerReadsStayResponsiveDuringSlowChange(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal(fmt.Sprintf(`local zel = require("zel")
return {hooks = {
  { events = {"model_changed"}, fn = function()
      zel.sh("touch %s; while [ ! -f %s ]; do sleep 0.01; done")
    end },
}}
`, started, release)))
	ctl := NewController(a, func(Event) {})
	defer ctl.Close()
	done := make(chan error, 1)
	go func() { done <- ctl.SelectModel("p", "new-model", "high") }()
	defer os.WriteFile(release, nil, 0600)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.WriteFile(release, nil, 0600)
			t.Fatal("hook did not start")
		}
	}
	read := make(chan struct{})
	go func() { _ = ctl.Status(); _ = ctl.Settings(); _, _ = ctl.Appearance(); _, _ = ctl.Info(); close(read) }()
	select {
	case <-read:
	case <-time.After(500 * time.Millisecond):
		os.WriteFile(release, nil, 0600)
		t.Fatal("controller reads blocked on hook")
	}
	if err := ctl.NewSession(); codeOf(err) != CodeBusy {
		t.Fatalf("overlapping change: %v", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("change did not finish")
	}
}

func TestAsyncHooksCancelledBeforeSessionCleanup(t *testing.T) {
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal(`local zel = require("zel")
return {hooks = {
  { events = {"turn_start"}, async = true, fn = function()
      zel.sh("touch hook-started; sleep 30; touch survived")
    end },
}}
`))
	s := a.session()
	s.fireHooks("turn_start", map[string]any{})
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.dir, "hook-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async hook did not start")
		}
	}
	start := time.Now()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("async hook blocked shutdown")
	}
	if _, err := os.Stat(filepath.Join(s.dir, "survived")); !os.IsNotExist(err) {
		t.Fatal("cancelled hook survived")
	}
	if _, err := os.Stat(s.sessionDir()); !os.IsNotExist(err) {
		t.Fatal("unsaved session directory leaked")
	}
}

func TestIdleHookCancelledAndJoinedOnShutdown(t *testing.T) {
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal(`local zel = require("zel")
return {hooks = {
  { events = {"idle"}, fn = function() zel.sh("touch idle-started; sleep 30") end },
}}
`))
	s := a.session()
	s.checkIdle()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.dir, "idle-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle hook never started")
		}
	}
	start := time.Now()
	a.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatal("idle hook outlived shutdown budget")
	}
}

func TestAsyncHookDoesNotBlock(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`local zel = require("zel")
return {hooks = {
  { events = {"lua_after"}, async = true, fn = function()
      zel.sh("sleep 1; touch async-done")
      return "IGNORED"
    end },
}}
`))
	s := a.session()
	done := filepath.Join(s.dir, "async-done")
	if got := s.runLua(context.Background(), "return 'x'"); got != "x" {
		t.Fatalf("async hook output used: %q", got)
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the call waited for the async hook")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async hook did not run")
		}
	}
}

func TestTaskFunctions(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	s, ctx := a.session(), context.Background()
	if got := s.runLua(ctx, `return task.spawn("greet", function() print("hello"); print("world"); return "done" end)`); got != "t1" {
		t.Fatalf("spawn: %q", got)
	}
	got := s.runLua(ctx, `local r = task.wait("t1", 10); return r, task.get("t1").state, task.tail("t1", 1)`)
	if got != "done\ndone\nworld\n" && got != "done\ndone\nworld" {
		t.Fatalf("wait: %q", got)
	}
	if got := s.runLua(ctx, `return task.spawn("slow", function() sh("sleep 30") end)`); got != "t2" {
		t.Fatalf("spawn: %q", got)
	}
	if got := s.runLua(ctx, `return task.wait("t2", 0.1)`); got != "" && got != "nil" {
		t.Fatalf("wait timeout: %q", got)
	}
	if got := s.runLua(ctx, `return #task.list()`); got != "2" {
		t.Fatalf("list: %q", got)
	}
	if active := a.ActiveTasks(); len(active) != 1 || active[0].ID != "t2" {
		t.Fatalf("active: %+v", active)
	}
	s.runLua(ctx, `task.stop("t2")`)
	if got := s.runLua(ctx, `task.wait("t2", 10); return task.get("t2").state`); got != "stopped" {
		t.Fatalf("stopped: %q", got)
	}
	// Waited-for results are not notified; the others are.
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("waited tasks notified: %+v", got)
	}
	s.runLua(ctx, `task.spawn("quick", function() end)`)
	if got := awaitInput(t, a); len(got) != 1 || !strings.HasPrefix(got[0].Text, "<notification>t3 finished") {
		t.Fatalf("notification: %+v", got)
	}
	if got := s.runLua(ctx, `return task.get("t9")`); got != "" && got != "nil" {
		t.Fatalf("unknown task: %q", got)
	}
	if got := s.runLua(ctx, `task.wait()`); !strings.Contains(got, "EINVAL") {
		t.Fatalf("wait without ids: %q", got)
	}
}

func TestChoiceDoesNotBlockAndAnswersByNotification(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	s, ctx := a.session(), context.Background()
	ask := func(title string, options ...string) string {
		list := "{"
		for _, o := range options {
			list += strconv.Quote(o) + ","
		}
		return s.runLua(ctx, "return choice("+strconv.Quote(title)+", "+list+"})")
	}
	if got := ask("Color", "Blue", "Red"); !strings.Contains(got, "EDENIED") {
		t.Fatalf("asked without a UI: %q", got)
	}
	var changes atomic.Int32
	a.OnChoiceChanged(func() { changes.Add(1) })
	if got := ask("Color", "Blue", "Red"); got != "c1" {
		t.Fatalf("ask: %q", got)
	}
	if got := ask("Size", "S"); got != "c2" {
		t.Fatalf("second ask: %q", got)
	}
	if got := ask("Nothing"); !strings.Contains(got, "EINVAL") {
		t.Fatalf("no options: %q", got)
	}
	if c := a.PendingChoice(); c == nil || c.ID != 1 || c.Title != "Color" || !slices.Equal(c.Options, []string{"Blue", "Red"}) || c.Waiting != 2 {
		t.Fatalf("pending: %+v", c)
	}
	a.AnswerChoice(1, 5) // out of range: ignored
	a.AnswerChoice(1, 1)
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>c1 Color: Red</notification>" || !got[0].Notice {
		t.Fatalf("answer: %+v", got)
	}
	if got := ask("Custom", "Preset"); got != "c3" {
		t.Fatalf("custom ask: %q", got)
	}
	a.AnswerChoiceText(3, "  ")
	if len(a.ask.queue) != 2 {
		t.Fatal("blank answer consumed question")
	}
	a.AnswerChoiceText(3, "  Свой ответ 123  ")
	a.AnswerChoiceText(3, "duplicate")
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>c3 Custom: Свой ответ 123</notification>" {
		t.Fatalf("custom: %+v", got)
	}
	a.AnswerChoice(2, -1)
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>c2 Size: cancelled</notification>" {
		t.Fatalf("cancel: %+v", got)
	}
	if c := a.PendingChoice(); c != nil || changes.Load() < 4 {
		t.Fatalf("pending %+v after %d changes", c, changes.Load())
	}

	headless := newTestAgent(t, &fakeProvider{}, Options{Mode: "headless"})
	headless.OnChoiceChanged(func() {})
	if got := headless.session().runLua(ctx, `return choice("Color", {"Blue"})`); !strings.Contains(got, "EDENIED") {
		t.Fatalf("headless choice: %q", got)
	}
}

func TestLuaSessionRenameAndImages(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	s, ctx := a.session(), context.Background()
	if got := s.runLua(ctx, `return session.id, session.mode, session.model, session.provider, session.cwd`); got != strings.Join([]string{s.id, "main", "m", "p", s.dir}, "\n") {
		t.Fatalf("session: %q", got)
	}
	s.runLua(ctx, `session.rename(" new   name ")`)
	if a.CustomTitle() != "new name" {
		t.Fatalf("rename: %q", a.CustomTitle())
	}
	writeFile(t, filepath.Join(s.dir, "img.png"), string(tinyPNG()))
	s.runLua(ctx, `read("img.png")`)
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>view: img.png attached</notification>" || len(got[0].Images) != 1 {
		t.Fatalf("view input: %+v", got)
	}
	if got := s.runLua(ctx, `read("missing.png")`); !strings.Contains(got, "ENOENT") {
		t.Fatalf("missing image: %q", got)
	}
	// cd() moves the next calls into a folder.
	writeFile(t, filepath.Join(s.dir, "sub", "deep.png"), string(tinyPNG()))
	s.runLua(ctx, `cd("sub"); read("deep.png")`)
	if got := a.Queue().Take(); len(got) != 1 || len(got[0].Images) != 1 {
		t.Fatalf("image from a subfolder: %+v", got)
	}
}

func TestReloadRebuildsThePromptOnlyWhenInstructionsChange(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal("return {systems = {rule = 'RULE-ONE'}}\n"))
	local := a.ConfigFile()
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "return {systems = {rule = 'RULE-ONE'}, settings = {colors = {accent = '#aa00aa'}}}\n")
	if err := a.Reload(); err != nil || a.Settings().Colors.Get("accent") != "#aa00aa" || instructionItems(a) != 1 {
		t.Fatalf("reload: %v accent %q, %d prompts", err, a.Settings().Colors.Get("accent"), instructionItems(a))
	}
	writeFile(t, local, "return {systems = {rule = 'RULE-TWO'}, settings = {colors = {accent = '#aa00aa'}}}\n")
	if err := a.Reload(); err != nil || instructionItems(a) != 2 {
		t.Fatalf("reload: %v, %d prompts", err, instructionItems(a))
	}
	writeFile(t, local, "return {modle = 'typo'}\n")
	if err := a.Reload(); err == nil || !strings.Contains(err.Error(), "previous config stays") || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("broken file: %v", err)
	}
	writeFile(t, local, "return {settings = {colors = {accent = 'purple'}}}\n")
	if err := a.Reload(); err == nil || !strings.Contains(err.Error(), "previous config stays") || !strings.Contains(err.Error(), "accent") {
		t.Fatalf("bad color: %v", err)
	}
	if a.Settings().Colors.Get("accent") != "#aa00aa" || instructionItems(a) != 2 {
		t.Fatal("a broken layer changed the snapshot")
	}
	if _, err := turn(t, a, "again"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if !strings.Contains(reqs[0], "RULE-ONE") || !strings.Contains(reqs[1], "RULE-TWO") || strings.Contains(reqs[1], "RULE-ONE") {
		t.Fatalf("prompts:\n%s\n%s", reqs[0], reqs[1])
	}
}

func TestOnlyTheModelIsWrittenToTheConfig(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, nil)
	if err := a.SelectSession(context.Background(), "p", "picked", "high"); err != nil {
		t.Fatal(err)
	}
	conn, err := config.LoadConnect(config.DefaultPaths(a.Home(), "", "").Connect)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Provider != "p" || conn.Model != "picked" || conn.Effort != "high" {
		t.Fatalf("connect: %+v", conn)
	}
	if st := a.Settings(); st.Model != "picked" || st.Effort != "high" {
		t.Fatalf("selection: %+v", st)
	}
}

func TestChildrenAreTrackedHiddenAndResumedOnlyByTheirParent(t *testing.T) {
	parent := newTestAgent(t, okProvider(), Options{})
	ps := parent.session()
	if r := sock(t, parent, "child.start", map[string]any{"session": "kid", "model": "m", "effort": "low", "prompt": "check tests"}); !r.OK {
		t.Fatalf("child.start: %+v", r)
	}
	if list := parent.ActiveSubagents(); len(list) != 1 || list[0].ID != "kid" || list[0].Parent != ps.id || list[0].Command != "check tests" || list[0].Effort != "low" {
		t.Fatalf("children: %+v", list)
	}
	sock(t, parent, "child.end", map[string]any{"session": "kid"})
	if list := parent.ActiveSubagents(); len(list) != 0 {
		t.Fatalf("child did not end: %+v", list)
	}
	if _, err := turn(t, parent, "hi"); err != nil {
		t.Fatal(err)
	}

	// A zel -p run by the parent's bash finds it through the environment.
	t.Setenv("ZEL_SOCKET", ps.socketPath())
	t.Setenv("ZEL_TOKEN", ps.token)
	t.Setenv("ZEL_SESSION_ID", ps.id)
	t.Setenv("ZEL_DEPTH", "0")
	child, err := New(ps.dir, Options{Mode: "headless", Prompt: "review this"})
	if err != nil {
		t.Fatal(err)
	}
	cs := child.session()
	if list := parent.ActiveSubagents(); len(list) != 1 || list[0].ID != cs.id || list[0].Command != "review this" {
		t.Fatalf("child not announced: %+v", list)
	}
	if env := cs.baseEnv(); env["ZEL_PARENT"] != ps.id || env["ZEL_DEPTH"] != "1" || env["ZEL_MODE"] != "headless" {
		t.Fatalf("child env: %v", env)
	}
	if _, err := turn(t, child, "work"); err != nil {
		t.Fatal(err)
	}
	// After each response the child reports its model, context and spend for the parent's Info.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		list := parent.ActiveSubagents()
		if len(list) == 1 && list[0].Provider == "p" && list[0].Context > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage report: %+v", list)
		}
	}
	childID := child.SessionID()
	child.Close()
	if list := parent.ActiveSubagents(); len(list) != 0 {
		t.Fatalf("child end not announced: %+v", list)
	}
	if all := parent.Subagents(); len(all) != 2 || all[0].ID != "kid" || all[0].Ended.IsZero() || all[1].ID != childID || all[1].Ended.IsZero() || all[1].Context <= 0 {
		t.Fatalf("finished subagents must stay for Info: %+v", all)
	}
	list, err := parent.Sessions()
	if err != nil || len(list) != 1 || list[0].ID != ps.id {
		t.Fatalf("sessions: %+v %v", list, err)
	}

	again, err := New(ps.dir, Options{Mode: "headless", Resume: childID})
	if err != nil {
		t.Fatalf("parent could not resume its child: %v", err)
	}
	again.Close()
	t.Setenv("ZEL_SESSION_ID", "another-session")
	if _, err := New(ps.dir, Options{Mode: "headless", Resume: childID}); err == nil || codeOf(err) != CodeNotFound || !strings.HasPrefix(err.Error(), "ENOENT") {
		t.Fatalf("a stranger resumed the child: %v", err)
	}
	t.Setenv("ZEL_DEPTH", "3")
	if _, err := New(ps.dir, Options{Mode: "headless"}); codeOf(err) != CodeDenied {
		t.Fatalf("too deep: %v", err)
	}
}

func TestSessionRoundTripThroughResume(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return luaReply("c1", "print(\"saved-output\")")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "remember me"); err != nil {
		t.Fatal(err)
	}
	if err := a.Rename(" My   title "); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureSession(context.Background(), "p", "pinned", "high"); err != nil {
		t.Fatal(err)
	}
	texts := func() (out []string) {
		for _, r := range a.Records() {
			out = append(out, r.Kind+":"+r.Text)
		}
		return out
	}
	before, id := texts(), a.SessionID()
	file := filepath.Join(a.StateHome(), "sessions", strings.ReplaceAll(filepath.Clean(a.Project()), string(filepath.Separator), "-"), id, "session.jsonl")
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if a.SessionID() == id || len(a.Records()) != 0 || a.Settings().Model != "m" {
		t.Fatalf("new session: %s %v %s", a.SessionID(), texts(), a.Settings().Model)
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if after := texts(); !slices.Equal(after, before) || len(after) != 4 {
		t.Fatalf("records:\n%q\n%q", before, after)
	}
	if st := a.Settings(); a.CustomTitle() != "My title" || st.Model != "pinned" || st.Effort != "high" {
		t.Fatalf("title %q, model %s/%s", a.CustomTitle(), st.Model, st.Effort)
	}
	if list, err := a.Sessions(); err != nil || len(list) != 1 || list[0].ID != id || list[0].Title != "My title" {
		t.Fatalf("sessions: %+v %v", list, err)
	}
	// The resumed session goes on with its history.
	if _, err := turn(t, a, "more"); err != nil {
		t.Fatal(err)
	}
	if last := fp.requests()[len(fp.requests())-1]; !strings.Contains(last, "saved-output") || !strings.Contains(last, `"model":"pinned"`) {
		t.Fatalf("resumed request: %s", last)
	}
}

func TestLifecycleHooks(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") },
		status: func(n int) int { return map[bool]int{true: 400}[n == 2] }}
	conf := `local zel = require("zel")
local events = {"session_created", "session_resumed", "session_closed", "config_changed", "turn_start", "turn_end", "idle", "model_changed", "error"}
return {hooks = {
  { events = events, fn = function(event, d)
      local detail = d.text or d.answer or d.reason or d.model or d.kind or d.file or ""
      local f = io.open(zel.session.root .. "/hooks.log", "a")
      f:write(event .. " " .. tostring(detail) .. "\n")
      f:close()
    end },
}}
`
	a := newTestAgentWith(t, fp, Options{}, withLocal(conf))
	logFile := filepath.Join(a.session().dir, "hooks.log")
	read := func() string {
		data, _ := os.ReadFile(logFile)
		return string(data)
	}
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "fails"); err == nil {
		t.Fatal("the second request should fail")
	}
	if err := a.ConfigureSession(context.Background(), "p", "other", "low"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	first := a.SessionID()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(first); err != nil {
		t.Fatal(err)
	}
	// Idle fires once nothing runs any more, here after a task that was waited for.
	s := a.session()
	s.runLua(context.Background(), `task.wait(task.spawn("t", function() end), 10)`)
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(read(), "idle \n"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no idle hook:\n%s", read())
		}
	}
	got := read()
	for _, want := range []string{
		"turn_start hi\n",
		"turn_end ok\n",
		"error turn\n",
		"model_changed other\n",
		"config_changed ",
		"session_closed new\n",
		"session_closed resume\n",
		"session_resumed \n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(got, "session_created \n"); n != 2 {
		t.Errorf("session_created %d times", n)
	}
	if t.Failed() {
		t.Log(got)
	}
}

func TestChildUsesTheConfigAndModelOfItsParent(t *testing.T) {
	server := httptest.NewServer(okProvider())
	t.Cleanup(server.Close)
	home := t.TempDir()
	isolate(t, home)
	extra := filepath.Join(t.TempDir(), "other.lua")
	writeFile(t, extra, "return {}\n")
	writeFile(t, config.DefaultPaths(home, "", "").Connect, fmt.Sprintf(`{"provider":"p","model":"m","providers":{"p":{"base_url":%q}}}`, server.URL))
	parent, err := New(t.TempDir(), Options{Config: extra})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.ConfigureSession(context.Background(), "p", "parent-model", "low"); err != nil {
		t.Fatal(err)
	}
	if parent.ConfigFile() != extra {
		t.Fatalf("parent config: %v", parent.ConfigFile())
	}
	if _, err := os.Stat(config.DefaultPaths(home, "", "").Init); err == nil {
		t.Fatal("--config must not create the default config")
	}
	for k, v := range parent.session().baseEnv() {
		if k != "PATH" {
			t.Setenv(k, v)
		}
	}
	elsewhere := t.TempDir()
	child, err := New(elsewhere, Options{Mode: "headless"})
	if err != nil {
		t.Fatal(err)
	}
	st := child.Settings()
	if child.ConfigFile() != extra || st.Model != "parent-model" || st.Effort != "low" {
		t.Fatalf("child: config %v, settings %+v", child.ConfigFile(), st)
	}
	if _, err := turn(t, child, "work"); err != nil {
		t.Fatal(err)
	}
	child.Close()
}

func TestConfigWatchAndBrokenFileAtStart(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("return {settings = {timeout = 'x'}}\n"))
	if st := a.Settings(); st.Timeout != config.DefaultSettings().Timeout {
		t.Fatalf("a broken config was applied: %+v", st)
	}
	if data, _ := os.ReadFile(a.session().eventsLog()); !strings.Contains(string(data), "config skipped") || !strings.Contains(string(data), "timeout") {
		t.Fatalf("no notice for the broken file: %s", data)
	}
	changed := make(chan struct{}, 1)
	a.OnConfigChanged(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	// The watcher takes its first look in the background, so a change made at
	// once may be part of it: change the file again until it is noticed.
	for i := 0; ; i++ {
		writeFile(t, a.ConfigFile(), fmt.Sprintf("return {settings = {env = {WATCH = 'value%d'}}}\n", i))
		select {
		case <-changed:
			return
		case <-time.After(1500 * time.Millisecond):
		}
		if i == 3 {
			t.Fatal("the config change was not noticed")
		}
	}
}

func TestPromptTokens(t *testing.T) {
	got := PromptTokens("привет #hi x#no #c-d, # #é")
	if want := []PromptToken{{7, 10, "hi"}, {16, 20, "c-d"}}; !slices.Equal(got, want) {
		t.Fatalf("tokens %+v", got)
	}
	for _, c := range []struct {
		text       string
		cursor     int
		start, end int
		query      string
		ok         bool
	}{
		{"say #he", 7, 4, 7, "he", true},
		{"say #hello", 6, 4, 10, "h", true},
		{"say #", 5, 4, 5, "", true},
		{"x#he", 4, 0, 0, "", false},
		{"plain", 3, 0, 0, "", false},
	} {
		start, end, query, ok := PromptAt([]rune(c.text), c.cursor)
		if start != c.start || end != c.end || query != c.query || ok != c.ok {
			t.Errorf("PromptAt(%q, %d) = %d %d %q %v", c.text, c.cursor, start, end, query, ok)
		}
	}
}

func TestLuaCdSetsTheFolderAndSurvivesResume(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	s, ctx := a.session(), context.Background()
	start := s.dir
	sub := filepath.Join(start, "sub")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(start, "file.txt"), "x")
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	pwd := func() string { return strings.TrimSpace(runSh(s, "pwd -P")) }

	s.runLua(ctx, `cd("sub")`)
	if pwd() != real(sub) {
		t.Fatalf("pwd = %s", pwd())
	}
	// Tasks start in the folder of the next calls too.
	got := s.runLua(ctx, `return task.wait(task.spawn("pwd", function() return sh("pwd -P").out end), 10)`)
	if !strings.Contains(got, real(sub)) {
		t.Fatalf("task pwd: %s", got)
	}
	// Failures change nothing.
	if got := s.runLua(ctx, `cd("nope")`); !strings.Contains(got, "ENOENT") {
		t.Fatalf("missing: %q", got)
	}
	if got := s.runLua(ctx, `cd("../file.txt")`); !strings.Contains(got, "EINVAL") {
		t.Fatalf("file: %q", got)
	}
	if pwd() != real(sub) || s.dir != start {
		t.Fatalf("changed after errors: %s", pwd())
	}
	// Saved and restored by a resume.
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	id := a.SessionID()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	s = a.session()
	if pwd() != real(start) {
		t.Fatalf("new session pwd = %s", pwd())
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	s = a.session()
	if pwd() != real(sub) {
		t.Fatalf("resumed pwd = %s", pwd())
	}
	// No argument goes back to the start folder.
	s.runLua(ctx, `cd()`)
	if pwd() != real(start) {
		t.Fatalf("back: %s", pwd())
	}
	// A folder that is gone falls back to the start folder.
	s.runLua(ctx, `cd("sub")`)
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(sub)
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	s = a.session()
	if pwd() != real(start) {
		t.Fatalf("gone folder pwd = %s", pwd())
	}
}

// Answered questions stay in the chat at their place and come back on resume.
func TestChoiceAnswersStayInTheChat(t *testing.T) {
	a := newTestAgent(t, okProvider(), Options{})
	a.OnChoiceChanged(func() {})
	if _, err := turn(t, a, "first"); err != nil {
		t.Fatal(err)
	}
	if got := a.session().runLua(context.Background(), `return choice("Color: which?", {"Blue"})`); got != "c1" {
		t.Fatalf("ask: %q", got)
	}
	if rec, ok := a.AnswerChoice(1, 0); !ok || rec.Kind != "choice" || rec.Text != "Color: which?\x1fBlue" {
		t.Fatalf("record: %+v %v", rec, ok)
	}
	if _, ok := a.AnswerChoice(1, 0); ok {
		t.Fatal("answered twice")
	}
	a.Queue().Take()
	if _, err := turn(t, a, "second"); err != nil {
		t.Fatal(err)
	}
	kinds := func(records []Record) string {
		var out []string
		for _, r := range records {
			if r.Kind == "user" || r.Kind == "choice" {
				out = append(out, r.Kind+":"+r.Text)
			}
		}
		return strings.Join(out, "|")
	}
	want := "user:first|choice:Color: which?\x1fBlue|user:second"
	if got := kinds(a.Records()); got != want {
		t.Fatalf("records %q", got)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	id, dir := a.SessionID(), a.session().dir
	a.Close()
	again, err := New(dir, Options{Mode: "headless", Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := kinds(again.Records()); got != want {
		t.Fatalf("resumed records %q", got)
	}
}

func TestDeepestChildCannotStartSubagents(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{Mode: "headless"}, nil)
	s := a.session()
	if !strings.Contains(s.toolDescription(), "zel -p 'prompt'") {
		t.Fatal("a child below the limit is not told about subagents")
	}
	s.depth = MaxDepth
	if strings.Contains(s.toolDescription(), "zel -p '") || strings.Contains(s.toolDescription(), "--resume") {
		t.Fatalf("the deepest child is told about subagents:\n%s", s.toolDescription())
	}
}
