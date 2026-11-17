package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

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

// withLocal writes text as the config before the runtime starts; the test
// provider is added to it.
func withLocal(text string) func(home, dir string) {
	return func(home, dir string) {
		if err := os.WriteFile(config.File(home), []byte(text), 0600); err != nil {
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

func TestBashOutputExitCodeAndEnvironment(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("[env]\nGREETING = \"hello\"\n"))
	s, ctx := a.session(), context.Background()
	for command, want := range map[string]string{
		"echo out; echo err >&2; exit 3": "out\nerr\n[exit 3]",
		"printf no-newline; false":       "no-newline\n[exit 1]",
		"echo fine":                      "fine\n",
		"kill -9 $$":                     "[exit 137]",
	} {
		if got := s.runBash(ctx, command); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
	got := s.runBash(ctx, `printf '%s\n' "$ZEL_SESSION_ID" "$ZEL_MODE" "$ZEL_SOCKET" "$ZEL_TOKEN" "$ZEL_DEPTH" "$GREETING" "$(pwd -P)" "$PATH"`)
	lines := strings.Split(got, "\n")
	real, _ := filepath.EvalSymlinks(s.dir)
	if len(lines) < 8 || lines[0] != s.id || lines[1] != "main" || lines[2] != s.socketPath() || lines[3] != s.token ||
		lines[4] != "0" || lines[5] != "hello" || lines[6] != real {
		t.Fatalf("environment:\n%s", got)
	}
	// The folder of the zel binary comes first, so zel finds itself.
	path := filepath.SplitList(lines[7])
	if len(path) < 2 || strings.Join(path[1:], string(os.PathListSeparator)) != os.Getenv("PATH") {
		t.Fatalf("PATH = %s", lines[7])
	}
}

func TestBashOutputCut(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, nil)
	s, ctx := a.session(), context.Background()
	if !strings.Contains(s.toolDescription(), "cut to 8000 bytes") {
		t.Fatal("the prompt does not say the output limit")
	}
	// Under the limit: as is, no file.
	if got := s.runBash(ctx, "head -c 7000 /dev/zero | tr '\\0' a"); got != strings.Repeat("a", 7000) {
		t.Fatalf("short output changed: %d bytes", len(got))
	}
	if _, err := os.Stat(filepath.Join(s.sessionDir(), "outputs")); err == nil {
		t.Fatal("a file was written for an output under the limit")
	}
	// 13000 bytes: 4000 + marker + 4000, the file holds everything.
	got := s.runBash(ctx, "head -c 13000 /dev/zero | tr '\\0' a")
	path := filepath.Join(s.sessionDir(), "outputs", "b1.txt")
	want := strings.Repeat("a", 4000) + "\n\n======== [output cut: 5000 of 13000 bytes omitted]\n" +
		"======== full output: " + path + "\n" +
		"======== read it with sed -n '100,200p', rg, head or tail\n\n" + strings.Repeat("a", 4000)
	if got != want {
		t.Fatalf("cut output:\n%.200s ... %.300s", got, got[3900:])
	}
	if full, err := os.ReadFile(path); err != nil || len(full) != 13000 {
		t.Fatalf("full output file: %d bytes, %v", len(full), err)
	}
	// The ending stays visible, and the cut keeps whole lines and characters.
	got = s.runBash(ctx, "seq 1 3000; exit 3")
	if !strings.HasSuffix(got, "3000\n[exit 3]") || !strings.HasPrefix(got, "1\n2\n") || !strings.Contains(got, "b2.txt") {
		t.Fatalf("seq: %.80q ... %.80q", got, got[max(0, len(got)-80):])
	}
	got = s.runBash(ctx, "for i in $(seq 6000); do printf 'é'; done")
	for _, part := range strings.Split(got, "========") {
		if !utf8.ValidString(part) {
			t.Fatal("a cut split a character")
		}
	}
}

func TestBashOutputOffAndInvalid(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("[settings]\nbash_output = 0\n"))
	s := a.session()
	if got := s.runBash(context.Background(), "head -c 13000 /dev/zero | tr '\\0' a"); len(got) != 13000 {
		t.Fatalf("bash_output = 0 cut the output: %d bytes", len(got))
	}
	if strings.Contains(s.toolDescription(), "cut to") || !strings.Contains(s.toolDescription(), "never cut") {
		t.Fatal("the prompt does not say the output is never cut")
	}
	for _, bad := range []string{"-1", "500"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[settings]\nbash_output = "+bad+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if cfg := config.Load(context.Background(), path, nil); len(cfg.Errors) == 0 {
			t.Fatalf("bash_output = %s accepted", bad)
		}
	}
}

func TestBashTimeoutAndCancel(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("[settings]\nbash_timeout = 1\n"))
	s := a.session()
	if !strings.Contains(s.toolDescription(), "A call has 1 s") {
		t.Fatal("the prompt does not say the bash timeout")
	}
	start := time.Now()
	// The background sleep is in the call's process group, so it dies too.
	got := s.runBash(context.Background(), "echo started; sleep 10 & wait")
	if got != "started\n[timeout after 1s]" || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %q after %s", got, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := s.runBash(ctx, "sleep 10"); got != "[cancelled]" {
		t.Fatalf("cancel: %q", got)
	}
}

func TestMessageHooksRewriteAndBlock(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal(`[hook]
block = { event = "message", match = "secret", run = "echo 'no secrets' >&2; exit 2" }
ping = { event = "message", match = "PING", run = "cat >/dev/null; echo PONG-REWRITTEN" }
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

func TestBashHooks(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`[hook]
rm = { event = "bash_before", match = 'rm -rf', run = "echo 'no rm' >&2; exit 2" }
rewrite = { event = "bash_before", match = '^echo original', run = "cat >/dev/null; echo 'echo rewritten'" }
fail = { event = "bash_before", run = "cat >/dev/null; exit 7" }
after = { event = "bash_after", run = "cat >/dev/null; echo AFTER" }
code = { event = "bash_after", run = 'grep -q "\"code\":3," && echo SAW-EXIT-3 || true' }
matched = { event = "bash_after", match = '^exit 3', run = "cat >/dev/null; echo MATCHED-AFTER" }
`))
	s, ctx := a.session(), context.Background()
	for command, want := range map[string]string{
		"echo original":       "rewritten\nAFTER",
		"rm -rf /no/such/zel": "[blocked by a hook: no rm]",
		"echo other":          "other\nAFTER",
		"exit 3":              "[exit 3]\nAFTER\nSAW-EXIT-3\nMATCHED-AFTER",
	} {
		if got := s.runBash(ctx, command); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
	// A hook that fails is skipped and shows in Tracing.
	if data, _ := os.ReadFile(s.eventsLog()); !strings.Contains(string(data), "bash_before: cat >/dev/null; exit 7 -> exit 7") {
		t.Fatalf("failed hook not logged: %s", data)
	}
}

func TestTurnEndHookContinuesTheTurnAtMostThreeTimes(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal(`[hook.keep]
event = "turn_end"
run = 'grep -q "\"answer\":\"ok\"" && echo keep going'
`))
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 1+maxTurnInjections || !strings.Contains(reqs[1], `\u003cnotification\u003ekeep going\u003c/notification\u003e`) {
		t.Fatalf("%d requests; second: %s", len(reqs), reqs[min(1, len(reqs)-1)])
	}
}

func TestNotificationHookGetsPlainTextAndIsWrapped(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`[hook]
a = { event = "notification", run = 'jq -r .text | sed "s/\$/, run make test/"' }
b = { event = "notification", run = 'jq -r .text | sed "s/^/[/; s/\$/]/"' }
`))
	s := a.session()
	in := notification("t1 finished")
	got, ok := s.hookInput(context.Background(), in, func(TurnEvent) {})
	if !ok || got.Text != "<notification>[t1 finished, run make test]</notification>" || !got.Notice {
		t.Fatalf("plain hook: %+v %v", got, ok)
	}
}

func TestNotificationHookPrintingTagsIsNotWrappedAgain(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`[hook.tagged]
event = "notification"
run = 'cat >/dev/null; echo "<notification>t1 finished, run make test</notification>"'
`))
	got, ok := a.session().hookInput(context.Background(), notification("t1 finished"), func(TurnEvent) {})
	if !ok || got.Text != "<notification>t1 finished, run make test</notification>" {
		t.Fatalf("tagged hook: %+v %v", got, ok)
	}
}

func TestHooksInstructionsAndCompletionsFollowTheMode(t *testing.T) {
	conf := withLocal(`[instruction]
main = "MAIN-ONLY"
both = { text = "BOTH-MODES", modes = ["main", "headless"] }
headless = { text = "HEADLESS-ONLY", modes = ["headless"] }
plan = { text = "PLAN-ONLY", modes = ["plan"] }
[hook]
a-main = { event = "bash_after", run = "cat >/dev/null; echo MAIN" }
b-both = { event = "bash_after", run = "cat >/dev/null; echo BOTH", modes = ["main", "headless"] }
c-headless = { event = "bash_after", run = "cat >/dev/null; echo HEADLESS", modes = ["headless"] }
d-plan = { event = "bash_after", run = "cat >/dev/null; echo PLAN", modes = ["plan"] }
[completion]
hi = "say ${{ echo $((40 + 2)) }}"
bad = "${{ exit 3 }}"
planned = { text = "PLANNED", modes = ["plan"] }
[provider.q]
base_url = "http://127.0.0.1:1"
modes = ["plan"]
`)
	ctx := context.Background()
	main := newTestAgentWith(t, &fakeProvider{}, Options{}, conf)
	headless := newTestAgentWith(t, &fakeProvider{}, Options{Mode: "headless"}, conf)
	if got := main.session().runBash(ctx, "echo x"); got != "x\nMAIN\nBOTH" {
		t.Errorf("main hooks: %q", got)
	}
	if got := headless.session().runBash(ctx, "echo x"); got != "x\nBOTH\nHEADLESS" {
		t.Errorf("headless hooks: %q", got)
	}
	prompt, desc := main.session().assemblePrompt(), main.session().toolDescription()
	if !strings.Contains(prompt, "MAIN-ONLY") || !strings.Contains(prompt, "BOTH-MODES") || strings.Contains(prompt, "HEADLESS-ONLY") ||
		strings.Contains(prompt, "zel choice") || !strings.Contains(desc, `zel choice "Question"`) || !strings.Contains(desc, "Ask the user before") {
		t.Errorf("main prompt:\n%s\ntool:\n%s", prompt, desc)
	}
	prompt, desc = headless.session().assemblePrompt(), headless.session().toolDescription()
	if strings.Contains(prompt, "MAIN-ONLY") || !strings.Contains(prompt, "BOTH-MODES") || !strings.Contains(prompt, "HEADLESS-ONLY") || strings.Contains(prompt, "PLAN-ONLY") ||
		strings.Contains(desc, `zel choice "Question"`) || !strings.Contains(desc, "no user to ask") || strings.Contains(desc, "Ask the user before") ||
		strings.Contains(desc, "{") || !strings.Contains(desc, `zel -p "prompt"`) {
		t.Errorf("headless prompt:\n%s\ntool:\n%s", prompt, desc)
	}
	hi := "<completion name=\"hi\">\nsay 42\n</completion>"
	if got, err := main.ExpandPrompt(ctx, "#hi and #unknown, #hi"); err != nil || got != hi+" and #unknown, "+hi {
		t.Errorf("main expansion: %q %v", got, err)
	}
	if _, err := main.ExpandPrompt(ctx, "#bad"); err == nil || !strings.Contains(err.Error(), "#bad") {
		t.Errorf("failed completion: %v", err)
	}
	if got, err := headless.ExpandPrompt(ctx, "#hi"); err != nil || got != "#hi" {
		t.Errorf("headless expansion: %q %v", got, err)
	}
	// --mode plan: main plus plan.
	plan := newTestAgentWith(t, &fakeProvider{}, Options{Custom: "plan"}, conf)
	if got := plan.session().runBash(ctx, "echo x"); got != "x\nMAIN\nBOTH\nPLAN" {
		t.Errorf("plan hooks: %q", got)
	}
	if prompt := plan.session().assemblePrompt(); !strings.Contains(prompt, "MAIN-ONLY") || !strings.Contains(prompt, "PLAN-ONLY") || strings.Contains(prompt, "HEADLESS-ONLY") {
		t.Errorf("plan prompt:\n%s", prompt)
	}
	if got, err := plan.ExpandPrompt(ctx, "#planned"); err != nil || !strings.Contains(got, "PLANNED") {
		t.Errorf("plan completion: %q %v", got, err)
	}
	if got, _ := main.ExpandPrompt(ctx, "#planned"); got != "#planned" {
		t.Errorf("a plan completion in main: %q", got)
	}
	if _, ok := main.Settings().Providers["q"]; ok {
		t.Error("a plan provider in main")
	}
	if _, ok := plan.Settings().Providers["q"]; !ok {
		t.Error("no plan provider in plan")
	}
	if err := main.SelectSession(ctx, "q", "x", "high"); err == nil || !strings.Contains(err.Error(), "not available in mode main") {
		t.Errorf("select a plan provider in main: %v", err)
	}
}

func TestControllerReadsStayResponsiveDuringSlowChange(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal(fmt.Sprintf("[hook.slow]\nevent = \"model_changed\"\nrun = 'touch %s; while [ ! -f %s ]; do sleep 0.01; done'\n", started, release)))
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
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal("[hook.slow]\nevent = \"turn_start\"\nrun = 'touch hook-started; sleep 30; touch survived'\nasync = true\n"))
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
	a := newTestAgentWith(t, okProvider(), Options{}, withLocal("[hook.slow]\nevent = \"idle\"\nrun = 'touch idle-started; sleep 30'\n"))
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
	s.hookWG.Wait()
}

func TestAsyncHookDoesNotBlock(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal(`[hook.async]
event = "bash_after"
run = "sleep 1; echo IGNORED; touch async-done"
async = true
`))
	s := a.session()
	done := filepath.Join(s.dir, "async-done")
	if got := s.runBash(context.Background(), "echo x"); got != "x\n" {
		t.Fatalf("async hook output used: %q", got)
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the call waited for the async hook")
	}
	s.hookWG.Wait()
	if _, err := os.Stat(done); err != nil {
		t.Fatal("async hook did not run")
	}
}

func TestSocketTasks(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if r := sock(t, a, "task.run", map[string]any{"name": "greet", "cmd": "echo hello $ZEL_TASK; echo world"}); !r.OK || r.Out != "t1\n" {
		t.Fatalf("run: %+v", r)
	}
	r := sock(t, a, "task.wait", map[string]any{"ids": []string{"t1"}, "timeout": 10})
	if !r.OK || !strings.HasPrefix(r.Out, "t1 done greet ") || !strings.Contains(r.Out, "exit 0\n$ echo hello $ZEL_TASK; echo world\nhello t1\nworld") {
		t.Fatalf("wait: %+v", r)
	}
	if r := sock(t, a, "task.get", map[string]any{"id": "t1"}); !r.OK || !strings.Contains(r.Out, "t1 done greet") {
		t.Fatalf("get: %+v", r)
	}
	if r := sock(t, a, "task.tail", map[string]any{"id": "t1", "n": 1}); !r.OK || r.Out != "world\n" {
		t.Fatalf("tail: %+v", r)
	}
	if r := sock(t, a, "task.run", map[string]any{"cmd": "sleep 30"}); r.Out != "t2\n" {
		t.Fatalf("run: %+v", r)
	}
	if r := sock(t, a, "task.wait", map[string]any{"ids": []string{"t2"}, "timeout": 0.1}); r.OK || r.Code != 124 || !strings.HasPrefix(r.Error, "ETIMEOUT") {
		t.Fatalf("wait timeout: %+v", r)
	}
	r = sock(t, a, "task.list", nil)
	if lines := strings.Split(strings.TrimSpace(r.Out), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "t1 done greet") || !strings.HasPrefix(lines[1], "t2 running") {
		t.Fatalf("list: %q", r.Out)
	}
	if active := a.ActiveTasks(); len(active) != 1 || active[0].ID != "t2" {
		t.Fatalf("active: %+v", active)
	}
	if r := sock(t, a, "task.stop", map[string]any{"id": "t2"}); !r.OK {
		t.Fatalf("stop: %+v", r)
	}
	if r := sock(t, a, "task.wait", map[string]any{"ids": []string{"t2"}, "timeout": 10}); !r.OK || !strings.HasPrefix(r.Out, "t2 stopped") {
		t.Fatalf("wait stopped: %+v", r)
	}
	// Waited-for results are not notified; the others are.
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("waited tasks notified: %+v", got)
	}
	sock(t, a, "task.run", map[string]any{"cmd": "true"})
	if got := awaitInput(t, a); len(got) != 1 || got[0].Text != "<notification>t3 finished</notification>" {
		t.Fatalf("notification: %+v", got)
	}
	for _, c := range []struct {
		op   string
		args any
		code int
		msg  string
	}{
		{"task.get", map[string]any{"id": "t9"}, 3, "ENOENT: no task t9"},
		{"task.wait", map[string]any{"ids": []string{"t1", "t9"}}, 3, "ENOENT"},
		{"task.run", map[string]any{"cmd": " "}, 2, "EINVAL"},
		{"task.wait", map[string]any{}, 2, "EINVAL"},
		{"no.such", nil, 2, "EINVAL: unknown op"},
	} {
		if r := sock(t, a, c.op, c.args); r.OK || r.Code != c.code || !strings.HasPrefix(r.Error, c.msg) {
			t.Errorf("%s %v: %+v", c.op, c.args, r)
		}
	}
	if r := sockAs(t, a.session().socketPath(), "wrong", "task.list", nil); r.Code != 6 || !strings.HasPrefix(r.Error, "EDENIED") {
		t.Fatalf("wrong token: %+v", r)
	}
}

func TestChoiceDoesNotBlockAndAnswersByNotification(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	ask := func(title string, options ...string) ipc.Response {
		return sock(t, a, "choice.ask", map[string]any{"title": title, "options": options})
	}
	if r := ask("Color", "Blue", "Red"); r.Code != 6 {
		t.Fatalf("asked without a UI: %+v", r)
	}
	var changes atomic.Int32
	a.OnChoiceChanged(func() { changes.Add(1) })
	if r := ask("Color", "Blue", "Red"); !r.OK || r.Out != "c1\n" {
		t.Fatalf("ask: %+v", r)
	}
	if r := ask("Size", "S"); r.Out != "c2\n" {
		t.Fatalf("second ask: %+v", r)
	}
	if r := ask("Nothing"); r.Code != 2 {
		t.Fatalf("no options: %+v", r)
	}
	if c := a.PendingChoice(); c == nil || c.ID != 1 || c.Title != "Color" || !slices.Equal(c.Options, []string{"Blue", "Red"}) || c.Waiting != 2 {
		t.Fatalf("pending: %+v", c)
	}
	a.AnswerChoice(1, 5) // out of range: ignored
	a.AnswerChoice(1, 1)
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>c1 Color: Red</notification>" || !got[0].Notice {
		t.Fatalf("answer: %+v", got)
	}
	if r := ask("Custom", "Preset"); r.Out != "c3\n" {
		t.Fatalf("custom ask: %+v", r)
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
	if r := sock(t, headless, "choice.ask", map[string]any{"title": "Color", "options": []string{"Blue"}}); r.Code != 6 || !strings.HasPrefix(r.Error, "EDENIED") {
		t.Fatalf("headless choice: %+v", r)
	}
}

func TestSocketSessionAndView(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	s := a.session()
	r := sock(t, a, "session.get", nil)
	var info map[string]any
	if err := json.Unmarshal(r.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info["id"] != s.id || info["mode"] != "main" || info["model"] != "m" || info["provider"] != "p" || info["cwd"] != s.dir ||
		info["parent"] != "" || info["depth"] != float64(0) || !strings.Contains(r.Out, "id: "+s.id+"\n") {
		t.Fatalf("session: %s\n%s", r.Data, r.Out)
	}
	if r := sock(t, a, "session.rename", map[string]any{"name": " new   name "}); !r.OK || a.CustomTitle() != "new name" {
		t.Fatalf("rename: %+v %q", r, a.CustomTitle())
	}
	writeFile(t, filepath.Join(s.dir, "img.png"), string(tinyPNG()))
	if r := sockIn(t, a, s.dir, "view.add", map[string]any{"paths": []string{"img.png"}}); !r.OK {
		t.Fatalf("view: %+v", r)
	}
	if got := a.Queue().Take(); len(got) != 1 || got[0].Text != "<notification>view: img.png attached</notification>" || len(got[0].Images) != 1 {
		t.Fatalf("view input: %+v", got)
	}
	if r := sockIn(t, a, s.dir, "view.add", map[string]any{"paths": []string{"missing.png"}}); r.Code != 1 {
		t.Fatalf("missing image: %+v", r)
	}
	// A relative path is the caller's: a script that did cd sub, not the session folder.
	writeFile(t, filepath.Join(s.dir, "sub", "deep.png"), string(tinyPNG()))
	if r := sockIn(t, a, filepath.Join(s.dir, "sub"), "view.add", map[string]any{"paths": []string{"deep.png"}}); !r.OK {
		t.Fatalf("view from a subfolder: %+v", r)
	}
}

func TestReloadRebuildsThePromptOnlyWhenInstructionsChange(t *testing.T) {
	fp := okProvider()
	a := newTestAgentWith(t, fp, Options{}, withLocal("instruction.rule = \"RULE-ONE\"\n"))
	local := a.ConfigFile()
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	provider := "model = \"p/m\"\n[provider.p]\nbase_url = \"" + a.Settings().Providers["p"].BaseURL + "\"\n"
	writeFile(t, local, "instruction.rule = \"RULE-ONE\"\n"+provider+"[colors]\naccent = \"#aa00aa\"\n")
	if err := a.Reload(); err != nil || a.Settings().Colors.Get("accent") != "#aa00aa" || instructionItems(a) != 1 {
		t.Fatalf("reload: %v accent %q, %d prompts", err, a.Settings().Colors.Get("accent"), instructionItems(a))
	}
	writeFile(t, local, "instruction.rule = \"RULE-TWO\"\n"+provider+"[colors]\naccent = \"#aa00aa\"\n")
	if err := a.Reload(); err != nil || instructionItems(a) != 2 {
		t.Fatalf("reload: %v, %d prompts", err, instructionItems(a))
	}
	writeFile(t, local, "modle = \"typo\"\n")
	if err := a.Reload(); err == nil || !strings.Contains(err.Error(), "previous config stays") || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("broken file: %v", err)
	}
	writeFile(t, local, "[colors]\naccent = \"purple\"\n")
	if err := a.Reload(); err == nil || !strings.Contains(err.Error(), "previous config stays") || !strings.Contains(err.Error(), "colors.accent") {
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
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("[settings]\nbash_timeout = 5\n"))
	if err := a.SelectSession(context.Background(), "p", "picked", "high"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.File(a.Home()))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `model = "p/picked"`) || !strings.Contains(got, `effort = "high"`) ||
		!strings.Contains(got, "bash_timeout = 5") {
		t.Fatalf("config:\n%s", got)
	}
	if st := a.Settings(); st.Model != "picked" || st.Effort != "high" || st.BashTimeout != 5 {
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
			return bashReply("c1", "echo saved-output")
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
	file := filepath.Join(a.Home(), ".sessions", strings.ReplaceAll(filepath.Clean(a.Project()), string(filepath.Separator), "-"), id, "session.jsonl")
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
	hook := `'echo "$ZEL_HOOK $(cat)" >> hooks.log'`
	var conf strings.Builder
	conf.WriteString("[hook.log]\nrun = " + hook + "\nevent = [")
	for _, event := range []string{"session_created", "session_resumed", "session_closed", "config_changed", "turn_start", "turn_end", "idle", "model_changed", "error"} {
		conf.WriteString(`"` + event + `", `)
	}
	conf.WriteString("]\n")
	a := newTestAgentWith(t, fp, Options{}, withLocal(conf.String()))
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
	sock(t, a, "task.run", map[string]any{"cmd": "true"})
	sock(t, a, "task.wait", map[string]any{"ids": []string{"t1"}})
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(read(), "idle {}"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no idle hook:\n%s", read())
		}
	}
	got := read()
	for _, want := range []string{
		"turn_start {\"text\":\"hi\"}\n",
		"turn_end {\"answer\":\"ok\",\"running\":[]}\n",
		"error {\"kind\":\"turn\",\"message\":",
		"model_changed {\"effort\":\"low\",\"model\":\"other\",\"provider\":\"p\"}\n",
		"config_changed {\"error\":\"\",\"file\":\"",
		"session_closed {\"reason\":\"new\"}\n",
		"session_closed {\"reason\":\"resume\"}\n",
		"session_resumed {}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(got, "session_created {}\n"); n != 2 {
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
	extra := filepath.Join(t.TempDir(), "other.toml")
	writeFile(t, extra, "model = \"p/m\"\n[env]\nFROM_EXTRA = \"yes\"\n[provider.p]\nbase_url = \""+server.URL+"\"\n")
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
	if _, err := os.Stat(config.File(home)); err == nil {
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
	if child.ConfigFile() != extra || st.Env["FROM_EXTRA"] != "yes" || st.Model != "parent-model" || st.Effort != "low" {
		t.Fatalf("child: config %v, settings %+v", child.ConfigFile(), st)
	}
	if _, err := turn(t, child, "work"); err != nil {
		t.Fatal(err)
	}
	child.Close()
}

func TestConfigWatchAndBrokenFileAtStart(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("model = 1\n"))
	if st := a.Settings(); st.Model != "" {
		t.Fatalf("a broken config was applied: %+v", st)
	}
	if data, _ := os.ReadFile(a.session().eventsLog()); !strings.Contains(string(data), "config skipped") || !strings.Contains(string(data), "model: must be a string") {
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
		writeFile(t, a.ConfigFile(), fmt.Sprintf("[env]\nWATCH = \"value%d\"\n", i))
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

// The working indicator gets the command as is: no "$ ", no special labels, no comments.
func TestZelCdSetsTheFolderOfBashAndTasksAndSurvivesResume(t *testing.T) {
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
	pwd := func() string { return strings.TrimSpace(s.runBash(ctx, "pwd -P")) }

	if r := sockIn(t, a, start, "cd", map[string]any{"dir": "sub"}); !r.OK || r.Out != sub+"\n" {
		t.Fatalf("cd: %+v", r)
	}
	if pwd() != real(sub) {
		t.Fatalf("pwd = %s", pwd())
	}
	if r := sock(t, a, "task.run", map[string]any{"cmd": "pwd -P"}); !r.OK {
		t.Fatalf("task: %+v", r)
	}
	if r := sock(t, a, "task.wait", map[string]any{"ids": []string{"t1"}, "timeout": 10}); !strings.Contains(r.Out, "\n"+real(sub)) {
		t.Fatalf("task pwd: %s", r.Out)
	}
	// Failures change nothing.
	if r := sock(t, a, "cd", map[string]any{"dir": "nope"}); r.Code != 3 || !strings.HasPrefix(r.Error, "ENOENT") {
		t.Fatalf("missing: %+v", r)
	}
	if r := sock(t, a, "cd", map[string]any{"dir": filepath.Join(start, "file.txt")}); r.Code != 2 || !strings.HasPrefix(r.Error, "EINVAL") {
		t.Fatalf("file: %+v", r)
	}
	if pwd() != real(sub) || s.dir != start {
		t.Fatalf("changed after errors: %s", pwd())
	}
	if r := sock(t, a, "session.get", nil); !strings.Contains(r.Out, "bash_cwd: "+sub+"\n") {
		t.Fatalf("session: %s", r.Out)
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
	if r := sock(t, a, "cd", map[string]any{}); !r.OK || r.Out != start+"\n" || pwd() != real(start) {
		t.Fatalf("back: %+v %s", r, pwd())
	}
	if r := sock(t, a, "session.get", nil); strings.Contains(r.Out, "bash_cwd") {
		t.Fatalf("session: %s", r.Out)
	}
	// A folder that is gone falls back to the start folder.
	sockIn(t, a, start, "cd", map[string]any{"dir": "sub"})
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

func TestPromotedBashKeepsTimeoutAndCanBeStopped(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			a := newTestAgentWith(t, &fakeProvider{}, Options{}, withLocal("[settings]\nbash_timeout = 1\n"))
			s := a.session()
			done := make(chan string, 1)
			go func() { done <- s.runBash(context.Background(), "touch started; echo before; sleep 30; echo after") }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(s.dir, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bash never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			s.queue.Push(Input{Text: "change direction"})
			select {
			case out := <-done:
				if !strings.Contains(out, "moved to task t1") {
					t.Fatal(out)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handoff blocked")
			}
			if stop {
				job, err := s.tasks.find("t1")
				if err != nil {
					t.Fatal(err)
				}
				job.Stop()
			}
			s.tasks.mu.Lock()
			j := s.tasks.jobs["t1"]
			s.tasks.mu.Unlock()
			select {
			case <-j.done:
			case <-time.After(4 * time.Second):
				t.Fatal("promoted process outlived its stop/timeout")
			}
			want := "timeout"
			if stop {
				want = "stopped"
			}
			if j.status != want {
				t.Fatalf("status %s, want %s", j.status, want)
			}
		})
	}
}

// Answered questions stay in the chat at their place and come back on resume.
func TestChoiceAnswersStayInTheChat(t *testing.T) {
	a := newTestAgent(t, okProvider(), Options{})
	a.OnChoiceChanged(func() {})
	if _, err := turn(t, a, "first"); err != nil {
		t.Fatal(err)
	}
	if r := sock(t, a, "choice.ask", map[string]any{"title": "Color: which?", "options": []string{"Blue"}}); !r.OK {
		t.Fatalf("ask: %+v", r)
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

// setKey sets a dotted key in a TOML file for the tests; unlike config.SetKey it
// rewrites the whole file (comments are lost) and takes any value.
func setKey(path, key string, value any) error {
	data := map[string]any{}
	if _, err := toml.DecodeFile(path, &data); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	table, segs := data, strings.Split(key, ".")
	for _, seg := range segs[:len(segs)-1] {
		next, ok := table[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			table[seg] = next
		}
		table = next
	}
	table[segs[len(segs)-1]] = value
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(data)
}

func TestDeepestChildCannotStartSubagents(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{Mode: "headless"}, nil)
	s := a.session()
	if !strings.Contains(s.toolDescription(), `zel -p "prompt"`) {
		t.Fatal("a child below the limit is not told about subagents")
	}
	s.depth = MaxDepth
	if strings.Contains(s.toolDescription(), `zel -p "`) || strings.Contains(s.toolDescription(), "--resume") {
		t.Fatalf("the deepest child is told about subagents:\n%s", s.toolDescription())
	}
}
