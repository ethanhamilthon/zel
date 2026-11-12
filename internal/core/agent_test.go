package core

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
	"syscall"
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
	if err := a.Run(ctx, "hello", func(TurnEvent) {}); err != nil {
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
	emit := func(e TurnEvent) {
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
	go func() { done <- a.Run(context.Background(), "go", func(TurnEvent) {}) }()
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

func newTestAgent(t *testing.T, fp *fakeProvider, opts Options) *Runtime {
	return newTestAgentWith(t, fp, opts, nil)
}

func newTestAgentWith(t *testing.T, fp *fakeProvider, opts Options, pre func(home, dir string)) *Runtime {
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

func turn(t *testing.T, a *Runtime, text string) ([]TurnEvent, error) {
	t.Helper()
	var events []TurnEvent
	err := a.Run(context.Background(), text, func(e TurnEvent) { events = append(events, e) })
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

func storedItems(t *testing.T, a *Runtime) []string {
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

func awaitInput(t *testing.T, a *Runtime) []Input {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := a.Queue().Take(); len(got) > 0 {
			return got
		}
	}
	t.Fatal("no input arrived")
	return nil
}

func pendingImages(a *Runtime) int {
	a.slots.mu.Lock()
	defer a.slots.mu.Unlock()
	return a.slots.n
}

func TestSessionUsageCountsEachRequestOnce(t *testing.T) {
	msg := func(role string) json.RawMessage {
		return json.RawMessage(`{"type":"message","role":"` + role + `","content":"x"}`)
	}
	a := &Runtime{histMu: &sync.RWMutex{}, History: []json.RawMessage{msg("user"), msg("assistant"), msg("assistant"), msg("assistant")}}
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

func TestTurnEmitsStartedThenExactlyOneDone(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	h.send("hello")
	events := h.settle()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "started,agent,agent,done" {
		t.Fatalf("kinds = %s", got)
	}
	if done := doneEvents(events); len(done) != 1 || done[0].Status != "ok" || done[0].Text != "answer 1" {
		t.Fatalf("done = %+v", done)
	}
}

func TestSteeringJoinsRunningTurn(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	h.send("steer me")
	close(gate)
	h.settle()
	reqs := h.model.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "steer me") {
		t.Fatalf("steering not sent to the model: %d requests", len(reqs))
	}
}

func TestNotificationKeepsOrderWithSteering(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	if _, err := RunTask(h.dir, h.a.SessionID(), "echo notice A"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); h.a.RunningTasks() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("task result did not arrive")
		}
	}
	h.send("steer B")
	close(gate)
	h.settle()
	reqs := h.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	a, b := strings.Index(reqs[1], "notice A"), strings.Index(reqs[1], "steer B")
	if a < 0 || b < a {
		t.Fatalf("notification and steering out of order: A=%d B=%d", a, b)
	}
}

func TestStopReturnsUnacceptedSteering(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	h.send("queued text")
	if got := h.ctl.Status().Queued; len(got) != 1 {
		t.Fatalf("queued = %v", got)
	}
	h.ctl.Stop()
	done := doneEvents(h.settle())
	if len(done) != 1 || done[0].Status != "cancelled" || len(done[0].Unsent) != 1 || done[0].Unsent[0].Text != "queued text" {
		t.Fatalf("done = %+v", done)
	}
}

func TestBusyRejectionsAndQuitDuringTurn(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	model := &fakeModel{}
	h := newHarness(t, model)
	model.mu.Lock()
	model.gate = gate
	model.mu.Unlock()
	h.send("slow")
	h.await("request", func([]Event) bool { return len(model.requests()) > 0 })
	if codeOf(h.ctl.NewSession()) != CodeBusy || codeOf(h.ctl.Resume("missing")) != CodeBusy {
		t.Fatal("session switch accepted during a turn")
	}
	if status := h.ctl.Status(); !status.Busy || status.Cancelling {
		t.Fatal("session switch cancelled the running turn")
	}
	if _, err := h.ctl.Info(); err != nil {
		t.Fatal(err)
	}
	h.ctl.Trace()
	if codeOf(h.ctl.Rename("x")) != "busy" {
		t.Fatal("rename accepted during a turn")
	}
	if closed, err := h.ctl.Quit(); closed || err != nil {
		t.Fatalf("quit during turn: closed %v, %v", closed, err)
	}
	select {
	case <-h.ctl.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not close after the turn was cancelled")
	}
	if codeOf(h.ctl.Input(Input{Text: "late"})) != CodeFailed {
		t.Fatal("input accepted after quit")
	}
}

func TestReloadRebuildsPromptAfterTurn(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	local := filepath.Join(h.dir, "ZEL.md")
	writeRaw := func(content string) {
		_ = os.MkdirAll(filepath.Dir(local), 0700)
		if err := os.WriteFile(local, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write := func(text string) {
		writeRaw(text)
	}
	turns := func(n int) {
		h.await(fmt.Sprintf("%d turns", n), func(events []Event) bool {
			return !h.ctl.Status().Busy && len(doneEvents(events)) == n
		})
	}
	write("v1")
	h.send("a")
	turns(1)
	if r := h.model.requests()[0]; strings.Contains(r, "v1") {
		t.Fatalf("a ZEL.md changed after startup was used without /reload: %s", r)
	}
	h.ctl.Reload()
	h.model.mu.Lock()
	h.model.gate = make(chan struct{})
	h.model.mu.Unlock()
	h.send("b")
	h.await("request b", func([]Event) bool { return len(h.model.requests()) == 2 })
	writeRaw("v2")
	home, _ := ZelHome(h.dir)
	settings := h.ctl.Settings()
	settings.ShellTimeout = 42
	if err := WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	h.ctl.Reload()
	if !h.ctl.Status().Busy {
		t.Fatal("reload interrupted the running turn")
	}
	if got := h.ctl.Settings().ShellTimeout; got == 42 {
		t.Fatalf("config change applied mid-turn: got %d", got)
	}
	info, err := h.ctl.Info()
	if err != nil {
		t.Fatalf("info failed during running turn: %v", err)
	}
	if info.Session == "" {
		t.Fatal("info session empty")
	}
	close(h.model.gate)
	turns(2)
	h.send("c")
	turns(3)
	reqs := h.model.requests()
	if !strings.Contains(reqs[1], "v1") || !strings.Contains(reqs[2], "v2") {
		t.Fatalf("prompt not rebuilt after reload:\nb: %s\nc: %s", reqs[1], reqs[2])
	}
	if events := h.log(); !slices.ContainsFunc(events, func(e Event) bool { return strings.Contains(e.Text, "after the current turn") }) {
		t.Fatalf("no deferred reload notice: %s", describe(events))
	}
}

func TestPrequeuedInputGoesFirst(t *testing.T) {
	model := &fakeModel{}
	h := newHarnessWith(t, model, func(a *Runtime) {
		a.Queue().PushNotice(Input{Text: "NOTICE-A", Notice: true})
	})
	h.await("the prequeued turn", func(events []Event) bool { return len(doneEvents(events)) == 1 })
	if err := h.ctl.Input(Input{Text: "B"}); err != nil {
		t.Fatal(err)
	}
	h.await("the second turn", func(events []Event) bool { return len(doneEvents(events)) == 2 })
	bodies := model.requests()
	if len(bodies) < 2 || !strings.Contains(bodies[0], "NOTICE-A") || strings.Contains(bodies[0], `"B"`) {
		t.Fatalf("requests %q", bodies)
	}
}

func TestConcurrentInputDuringControllerInitialization(t *testing.T) {
	for round := 0; round < 20; round++ {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			var producers sync.WaitGroup
			h := newHarnessWith(t, &fakeModel{}, func(a *Runtime) {
				for i := 0; i < 4; i++ {
					producers.Add(1)
					go func(i int) {
						defer producers.Done()
						for j := 0; j < 8; j++ {
							a.Queue().PushNotice(Input{Text: fmt.Sprintf("notice-%d-%d", i, j), Notice: true})
						}
					}(i)
				}
			})
			producers.Wait()
			h.await("queued turn", func(events []Event) bool { return len(doneEvents(events)) > 0 })
			h.ctl.Close()
			if len(h.model.requests()) == 0 {
				t.Fatal("queued input lost")
			}
		})
	}
}

type fakeModel struct {
	mu     sync.Mutex
	bodies []string
	gate   chan struct{}
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	body := string(data)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	n := len(f.bodies)
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if !strings.Contains(body, `"stream":true`) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"SUMMARY","annotations":[]}]}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer %d\"}]}]}}\n\n", n)
}

func (f *fakeModel) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

type harness struct {
	t      *testing.T
	ctl    *Controller
	a      *Runtime
	model  *fakeModel
	dir    string
	mu     sync.Mutex
	events []Event
}

func newHarness(t *testing.T, model *fakeModel) *harness {
	t.Helper()
	return newHarnessWith(t, model, nil)
}

func newHarnessWith(t *testing.T, model *fakeModel, pre func(*Runtime)) *harness {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("ZEL_HOME", home)
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL}
	if err := WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(a)
	}
	h := &harness{t: t, a: a, model: model, dir: dir}
	h.ctl = NewController(a, func(e Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, e)
	})
	t.Cleanup(func() {
		h.ctl.Close()
		a.Close()
	})
	return h
}

func (h *harness) log() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

func (h *harness) await(what string, pred func([]Event) bool) []Event {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := h.log()
		if pred(events) {
			return events
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; events: %s", what, describe(events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func describe(events []Event) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, e.Kind+"/"+e.Status+"/"+e.Text)
	}
	return strings.Join(parts, " | ")
}

func doneEvents(events []Event) []Event {
	var done []Event
	for _, e := range events {
		if e.Kind == "done" {
			done = append(done, e)
		}
	}
	return done
}

func (h *harness) settle() []Event {
	h.t.Helper()
	return h.await("idle", func(events []Event) bool {
		return !h.ctl.Status().Busy && len(doneEvents(events)) > 0
	})
}

func (h *harness) send(text string, images ...shell.Image) {
	h.t.Helper()
	if err := h.ctl.Input(Input{Text: text, Images: images}); err != nil {
		h.t.Fatalf("input %q: %v", text, err)
	}
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func (h *harness) inFlight(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.model.requests()) < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("request %d never arrived", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestModelChangeRejectedDuringTurn(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("work")
	h.inFlight(1)
	before := h.ctl.Settings()
	if err := h.ctl.SelectModel(before.Provider, "different", "high"); err == nil {
		t.Fatal("model changed during turn")
	}
	if got := h.ctl.Settings(); got.Model != before.Model || got.Effort != before.Effort {
		t.Fatal("rejected change mutated settings")
	}
	close(gate)
	h.settle()
}

func TestImageOnlyInputNormalizedByController(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	if err := h.ctl.Input(Input{Images: []shell.Image{{URL: "data:image/png;base64,aQ=="}}}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if requests := h.model.requests(); len(requests) != 1 || !strings.Contains(requests[0], "Describe this image.") || !strings.Contains(requests[0], "input_image") {
		t.Fatalf("image-only request: %v", requests)
	}
}

func TestResumeMissingSessionKeepsCurrentSession(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	id := h.ctl.Status().Session
	err := h.ctl.Resume("missing")
	if err == nil || !strings.Contains(err.Error(), "session not found in this project") || h.ctl.Status().Session != id {
		t.Fatalf("resume result: %v session=%s", err, h.ctl.Status().Session)
	}
}

const retryAttempts = 3

func TestActiveSubagentsPruning(t *testing.T) {
	home := t.TempDir()
	subDir := filepath.Join(home, "subagents")
	locksDir := filepath.Join(home, "locks")
	if err := os.MkdirAll(subDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locksDir, 0700); err != nil {
		t.Fatal(err)
	}

	parentID := "root-123"

	// 1. Dead subagent with missing lock file -> pruned
	data, _ := json.Marshal(SubagentRun{ID: "dead-1", Parent: parentID, Started: time.Now()})
	if err := os.WriteFile(filepath.Join(subDir, "dead-1.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	// 2. Dead subagent with unlocked lock file -> pruned
	data, _ = json.Marshal(SubagentRun{ID: "dead-2", Parent: parentID, Started: time.Now()})
	if err := os.WriteFile(filepath.Join(subDir, "dead-2.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "dead-2.lock"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Live subagent with exclusively locked lock file -> retained
	data, _ = json.Marshal(SubagentRun{ID: "live-1", Parent: parentID, Started: time.Now()})
	if err := os.WriteFile(filepath.Join(subDir, "live-1.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(filepath.Join(locksDir, "live-1.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	list := ActiveSubagentsForParent(home, parentID)
	if len(list) != 1 || list[0].ID != "live-1" {
		t.Fatalf("expected [live-1], got %+v", list)
	}

	// Check dead files were removed
	if _, err := os.Stat(filepath.Join(subDir, "dead-1.json")); !os.IsNotExist(err) {
		t.Fatal("dead-1.json was not pruned")
	}
	if _, err := os.Stat(filepath.Join(subDir, "dead-2.json")); !os.IsNotExist(err) {
		t.Fatal("dead-2.json was not pruned")
	}
	if _, err := os.Stat(filepath.Join(subDir, "live-1.json")); err != nil {
		t.Fatal("live-1.json was pruned unexpectedly")
	}
}
