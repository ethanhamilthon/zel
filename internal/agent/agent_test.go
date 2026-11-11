package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"zel/internal/shell"
)

func TestToolCallThenAnswerIsCommittedInOrder(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range a.Records() {
		kinds = append(kinds, r.Kind+":"+strings.TrimSpace(r.Text))
	}
	if got := strings.Join(kinds, "|"); got != "user:go|code:echo hi|output:hi|assistant:done" {
		t.Fatalf("records = %s", got)
	}
	if final := a.FinalText(); final != "done" {
		t.Fatalf("FinalText = %q", final)
	}
	if n := len(fp.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

func TestStreamFailureSurfacesAsError(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompletedResponseWithoutStreamClose(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{hold: true, reply: func(int, string) string { return textReply("hi") }}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx, "hello", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := a.FinalText(); got != "hi" {
		t.Fatalf("final %q", got)
	}
}

func TestModelsStopOnRepeatedPage(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return `{"data":[{"id":"a"},{"id":"b"}],"has_more":true}`
	}}
	a := newTestAgent(t, fp, Options{})
	choices, errs := AllModels(context.Background(), a.Settings().Providers)
	if len(errs) != 0 || len(choices) != 2 || len(fp.requests()) != 2 {
		t.Fatalf("choices %v errs %v requests %d", choices, errs, len(fp.requests()))
	}
}

func TestStreamBudgets(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		repeat int
		want   string
	}{
		{"line", "data: " + strings.Repeat("x", maxStreamEvent) + "\n", 1, "token too long"},
		{"multiline event", "data: " + strings.Repeat("x", maxStreamEvent/2) + "\n", 3, "event byte limit"},
		{"response", ":" + strings.Repeat("x", (1<<20)-2) + "\n", 65, "response byte limit"},
		{"event count", "\n", maxStreamEvents + 1, "event count limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < tc.repeat; i++ {
					if _, err := fmt.Fprint(w, tc.line); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			c := &client{base: server.URL}
			err := c.stream(context.Background(), map[string]any{}, func(streamEvent) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v want %s", err, tc.want)
			}
		})
	}
}

func TestProviderFailuresNeverShowTheKey(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"bad key sk-secret-123"}}}`)
	}}
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL, APIKey: "sk-secret-123"}
	if err := WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	events, err := turn(t, a, "hi")
	if err == nil || strings.Contains(err.Error(), "sk-secret-123") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error = %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Text, "sk-secret-123") {
			t.Fatalf("key in event %+v", e)
		}
	}
}

func TestInterruptedCallIsClosedWithoutRerunning(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	a.History = append(a.History, raw(map[string]any{"role": "user", "content": "start"}),
		raw(map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": `{"command":"touch marker"}`}))
	if _, err := turn(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "marker")); err == nil {
		t.Fatal("interrupted command was rerun")
	}
	joined := ""
	for _, item := range a.History {
		joined += string(item)
	}
	if !strings.Contains(joined, "interrupted") || !strings.Contains(joined, "c1") {
		t.Fatalf("no interrupted result in history: %s", joined)
	}
}

func TestFailedResponseLeavesNoPartialItem(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"partial answer"}]}}`,
			`{"type":"response.failed","response":{"error":{"code":"server_error","message":"cut off"}}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil {
		t.Fatal("failed response reported as success")
	}
	items := strings.Join(storedItems(t, a), "\n")
	if strings.Contains(items, "partial answer") || !strings.Contains(items, "hi") {
		t.Fatalf("saved session %q", items)
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	fp := &fakeProvider{
		reply:  func(int, string) string { return textReply("ok") },
		status: func(n int) int { return map[bool]int{true: http.StatusInternalServerError}[n == 1] },
	}
	a := newTestAgent(t, fp, Options{})
	events, err := turn(t, a, "hi")
	if err != nil || len(fp.requests()) != 2 {
		t.Fatalf("err %v after %d requests", err, len(fp.requests()))
	}
	var notice bool
	for _, e := range events {
		notice = notice || (e.Kind == "notice" && strings.HasPrefix(e.Text, "Retrying in"))
	}
	if !notice {
		t.Fatal("retry not shown")
	}
}

func TestRetryStopsWhereItShould(t *testing.T) {
	client := &fakeProvider{reply: func(int, string) string { return textReply("ok") },
		status: func(int) int { return http.StatusBadRequest }}
	if _, err := turn(t, newTestAgent(t, client, Options{}), "hi"); err == nil || len(client.requests()) != 1 {
		t.Fatalf("client error retried: %v after %d", err, len(client.requests()))
	}
	server := &fakeProvider{reply: func(int, string) string { return textReply("ok") },
		status: func(int) int { return http.StatusInternalServerError }}
	if _, err := turn(t, newTestAgent(t, server, Options{}), "hi"); err == nil || len(server.requests()) != retryAttempts {
		t.Fatalf("server errors: %v after %d requests", err, len(server.requests()))
	}
}

func TestRetryDoesNotDuplicateStreamedOutput(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil || len(fp.requests()) != 1 {
		t.Fatalf("shown output retried: %v after %d requests", err, len(fp.requests()))
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	if got := delay(0, nil); got != time.Second {
		t.Fatalf("first pause %s", got)
	}
	if got := delay(10, nil); got != retryMax {
		t.Fatalf("capped pause %s", got)
	}
	long := &apiError{Status: 429, RetryAfter: "3600"}
	if got := delay(0, long); got != retryMax {
		t.Fatalf("Retry-After above the cap: %s", got)
	}
	short := &apiError{Status: 429, RetryAfter: "3"}
	if got := delay(0, short); got != 3*time.Second {
		t.Fatalf("Retry-After 3: %s", got)
	}
}

func TestSteeringIsAcceptedAfterToolOutputs(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	queue := a.Queue()
	pushed, shown := false, false
	emit := func(e Event) {
		if e.Kind == "code" && e.Final && !e.Provisional && !pushed {
			pushed = true
			queue.Push(Input{Text: "also check Y"})
		}
		if e.Kind == "user" && e.Text == "also check Y" {
			shown = true
		}
	}
	if err := a.RunInput(context.Background(), Input{Text: "start"}, emit); err != nil {
		t.Fatal(err)
	}
	second := fp.requests()[1]
	call, output, steer := strings.Index(second, `"function_call"`), strings.Index(second, `"function_call_output"`), strings.Index(second, "also check Y")
	if !(0 <= call && call < output && output < steer) {
		t.Fatalf("order call=%d output=%d steer=%d", call, output, steer)
	}
	if !shown {
		t.Fatal("accepted steering was not emitted as a user event")
	}
}

func TestTypedInputMovesRunningCallToBackground(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			args := func(command string) string {
				data, _ := json.Marshal(map[string]any{"command": command, "timeout": nil})
				return string(data)
			}
			return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[`+
				`{"type":"function_call","call_id":"c1","name":"bash","arguments":%q},`+
				`{"type":"function_call","call_id":"c2","name":"bash","arguments":%q}]}}`,
				args("touch started; echo early; sleep 2; echo late"), args("touch second")))
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- a.Run(context.Background(), "go", func(Event) {}) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(a.Shell.Dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
	}
	a.Queue().Push(Input{Text: "STOP-AND-LISTEN"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1800*time.Millisecond {
		t.Fatalf("the turn waited for the command: %s", took)
	}
	requests := fp.requests()
	if len(requests) != 2 || !strings.Contains(requests[1], "STOP-AND-LISTEN") || !strings.Contains(requests[1], "background as task t1") ||
		!strings.Contains(requests[1], "early") || !strings.Contains(requests[1], "Skipped: the user sent a new message") {
		t.Fatalf("second request: %s", requests[len(requests)-1])
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "second")); err == nil {
		t.Fatal("a call after the message ran")
	}
	got := awaitInput(t, a)
	if !strings.Contains(got[0].Text, `id="t1" status="done"`) || !strings.Contains(got[0].Text, "early") || !strings.Contains(got[0].Text, "late") {
		t.Fatalf("task result %q", got[0].Text)
	}
}

func TestPartialCodeShowsCompletePrefix(t *testing.T) {
	cases := map[string]string{
		`{"command":"echo hi`:         "echo hi",
		`{"command":"printf \"a\`:     `printf "a`,
		`{"command":"x", "timeout":1`: "x",
		`{"command":"`:                "",
		`{"other":1`:                  "",
	}
	for arguments, want := range cases {
		if got := partialCode(arguments); got != want {
			t.Errorf("partialCode(%q) = %q, want %q", arguments, got, want)
		}
	}
}

func TestSocketValidates(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	for _, m := range []Message{
		{},
		{Task: &TaskRequest{Op: "run", Command: " "}},
		{Task: &TaskRequest{Op: "stop", ID: "t9"}},
		{Image: &shell.Image{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
	} {
		if _, err := Send(a.sock.path, m); err == nil {
			t.Fatalf("accepted %+v", m)
		}
	}
	if got := a.Queue().Take(); len(got) != 0 {
		t.Fatalf("queued %+v", got)
	}
}

func TestTaskStopAndSessionClose(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "run", Command: "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	if a.RunningTasks() != 1 {
		t.Fatal("task not running")
	}
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "stop", ID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	if got := awaitInput(t, a); !strings.HasPrefix(got[0].Text, `<task-result id="t1" status="stopped"`) {
		t.Fatalf("result %+v", got)
	}
	if _, err := Send(a.sock.path, Message{Task: &TaskRequest{Op: "run", Command: "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	old, start := a.SessionID(), time.Now()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || a.RunningTasks() != 0 || len(a.Queue().Take()) != 0 {
		t.Fatal("session change did not stop the task quietly")
	}
	if list, err := SessionTasks(a.home, old); err != nil || len(list) != 2 || list[1].Status != "stopped" {
		t.Fatalf("tasks %+v %v", list, err)
	}
}

func TestConnectionAcceptedBeforeNewSessionIsRetired(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	payload, err := json.Marshal(Message{Image: &shell.Image{URL: pngURL()}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", a.sock.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload[:len(payload)/2]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		a.sock.mu.Lock()
		accepted := a.sock.current != nil
		a.sock.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection not accepted")
		}
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write(payload[len(payload)/2:])
	var status string
	if err := json.NewDecoder(conn).Decode(&status); err == nil && status == "" {
		t.Fatal("retired connection was acknowledged")
	}
	if pendingImages(a) != 0 || len(a.Queue().Take()) != 0 {
		t.Fatal("image from the retired connection reached the new session")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	path := a.sock.path
	a.sock.retire()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := a.listenNotifications(); err != nil {
		t.Fatalf("stale socket blocked listening: %v", err)
	}
}

func TestTailOfHugeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	file.WriteAt([]byte("\nlast line\n"), 1<<30)
	file.Close()
	if got := tail(path); !strings.HasPrefix(got, "... earlier output") || !strings.HasSuffix(got, "\nlast line") || len(got) > taskResultBytes+40 {
		t.Fatalf("tail %d bytes: %q", len(got), got[max(0, len(got)-60):])
	}
}

type fakeProvider struct {
	mu     sync.Mutex
	bodies []string
	reply  func(n int, body string) string
	status func(n int) int
	hold   bool
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(data))
	n := len(f.bodies)
	f.mu.Unlock()
	if f.status != nil && f.status(n) != 0 {
		w.WriteHeader(f.status(n))
		fmt.Fprint(w, `{"error":{"message":"transient"}}`)
		return
	}
	out := f.reply(n, string(data))
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(string(data), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	fmt.Fprint(w, out)
	if f.hold {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

func (f *fakeProvider) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", e)
	}
	return b.String()
}

func textReply(text string) string {
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}}`, text))
}

func bashReply(callID, command string) string {
	args, _ := json.Marshal(map[string]any{"command": command, "timeout": nil})
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":%q,"name":"bash","arguments":%q}]}}`, callID, args))
}

func newTestAgent(t *testing.T, fp *fakeProvider, opts Options) *Agent {
	return newTestAgentWith(t, fp, opts, nil)
}

func newTestAgentWith(t *testing.T, fp *fakeProvider, opts Options, pre func(home, dir string)) *Agent {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("ZEL_HOME", home)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL}
	if err := WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(home, dir)
	}
	a, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func turn(t *testing.T, a *Agent, text string) ([]Event, error) {
	t.Helper()
	var events []Event
	err := a.Run(context.Background(), text, func(e Event) { events = append(events, e) })
	return events, err
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func tinyPNG() []byte {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	return data
}

func storedItems(t *testing.T, a *Agent) []string {
	t.Helper()
	rows, err := a.db.Query(`SELECT data FROM items WHERE session = ? ORDER BY seq`, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func pngURL() string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG())
}

func awaitInput(t *testing.T, a *Agent) []Input {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := a.Queue().Take(); len(got) > 0 {
			return got
		}
	}
	t.Fatal("no input arrived")
	return nil
}

func pendingImages(a *Agent) int {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	return a.slots.n
}

func TestSessionUsageCountsEachRequestOnce(t *testing.T) {
	msg := func(role string) json.RawMessage {
		return json.RawMessage(`{"type":"message","role":"` + role + `","content":"x"}`)
	}
	a := &Agent{histMu: &sync.RWMutex{}, History: []json.RawMessage{msg("user"), msg("assistant"), msg("assistant"), msg("assistant")}}
	a.setItemMeta(1, Metadata{RequestSeq: 2, Usage: &Usage{Input: 100, Output: 10, Cached: 40}})
	a.setItemMeta(2, Metadata{RequestSeq: 2, Usage: &Usage{Input: 100, Output: 10, Cached: 40}}) // same request
	a.setItemMeta(3, Metadata{RequestSeq: 4, Usage: &Usage{Input: 200, Output: 20, Cached: 160}})
	total, requests := a.SessionUsage()
	if requests != 2 || total != (Usage{Input: 300, Output: 30, Cached: 200}) || a.UserMessages() != 1 {
		t.Fatalf("usage %+v over %d requests", total, requests)
	}
	a.setItemMeta(3, Metadata{RequestSeq: 4, Usage: &Usage{Input: 200, Output: 20, Cached: -1}})
	if total, _ = a.SessionUsage(); total.Cached != -1 {
		t.Fatalf("unknown cache kept: %+v", total)
	}
}

func TestWithdrawIdentifiedSteering(t *testing.T) {
	q := &Steering{}
	q.PushNotice(Input{Text: "notice", Notice: true})
	q.Push(Input{Text: "same text", QueueID: 1})
	q.Push(Input{Text: "same text", QueueID: 2})
	if !q.Withdraw(1) || q.Withdraw(1) || q.Withdraw(0) {
		t.Fatal("withdraw did not match identity")
	}
	pending := q.Take()
	if len(pending) != 2 || !pending[0].Notice || pending[1].QueueID != 2 {
		t.Fatalf("withdraw affected other messages: %v", pending)
	}
	if q.Withdraw(2) {
		t.Fatal("accepted input must not be withdrawable")
	}
}

func TestChatStreamCountsNonzeroUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"hello"}}]}`+"\n\ndata: "+`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"prompt_tokens_details":{"cached_tokens":67}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	client := newClient(Provider{BaseURL: server.URL, API: APIChat})
	var input, output, cached int64
	err := client.streamChat(context.Background(), map[string]any{"model": "m"}, func(e streamEvent) error {
		if e.Response.Usage != nil {
			input, output = e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens
			cached = *e.Response.Usage.InputTokensDetails.CachedTokens
		}
		return nil
	})
	if err != nil || input != 123 || output != 45 || cached != 67 {
		t.Fatalf("usage=%d/%d/%d err=%v", input, output, cached, err)
	}
}

func TestTaskLogIsBoundedAndDrained(t *testing.T) {
	var data bytes.Buffer
	w := &taskLog{file: &data}
	chunk := bytes.Repeat([]byte("x"), 65536)
	for i := 0; i < 256; i++ {
		if n, err := w.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("write=%d err=%v", n, err)
		}
	}
	if data.Len() != taskLogLimit+len(taskLogTruncated) || !strings.HasSuffix(data.String(), taskLogTruncated) {
		t.Fatalf("log size=%d", data.Len())
	}
}

func TestTaskAdmissionReservesCompletionCapacity(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	for i := 0; i < maxQueued; i++ {
		a.Queue().PushNotice(Input{Text: "completion", Notice: true})
	}
	if _, err := a.tasks.run("echo should-not-run"); err == nil || !strings.Contains(err.Error(), "unresolved task") {
		t.Fatalf("admission: %v", err)
	}
	for i := 0; i < maxQueued; i++ {
		if !a.Queue().Push(Input{Text: "composer"}) {
			t.Fatalf("composer slot %d rejected", i)
		}
	}
	if a.Queue().Push(Input{Text: "overflow"}) {
		t.Fatal("composer limit not enforced")
	}
}

func TestTaskCompletionPersistenceFailureIsReported(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if _, err := a.db.Exec(`CREATE TRIGGER fail_task_finish BEFORE UPDATE ON tasks BEGIN SELECT RAISE(FAIL, 'completion blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.run("echo finished"); err != nil {
		t.Fatal(err)
	}
	a.tasks.wg.Wait()
	inputs := a.Queue().Take()
	if len(inputs) != 1 || !strings.Contains(inputs[0].Text, "could not be persisted") || !strings.Contains(inputs[0].Text, "completion blocked") {
		t.Fatalf("completion: %+v", inputs)
	}
}

func TestSavedImageOriginalsRetainedAndMissingReported(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	path := filepath.Join(a.home, "attachments", "original.png")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	a.commitInput(Input{Text: "image", Images: []shell.Image{{Path: path, URL: "data:image/png;base64,aQ=="}}})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.Shell.Dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("saved source removed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	records, err := ReadSession(a.Shell.Dir, a.SessionID())
	if err != nil || !slices.ContainsFunc(records, func(r Record) bool { return strings.Contains(r.Text, "Original image unavailable") }) {
		t.Fatalf("missing-source notice: %v err=%v", records, err)
	}
}
