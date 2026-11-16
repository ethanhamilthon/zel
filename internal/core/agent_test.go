package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"zel/internal/config"
	"zel/internal/shell"
)

func TestToolCallThenAnswerIsCommittedInOrder(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", `echo hi`)
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
	if got := strings.Join(kinds, "|"); got != `user:go|code:echo hi|output:hi|assistant:done` {
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

func TestFailedTurnClosesWithItsError(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		switch n {
		case 1:
			return bashReply("c1", `echo hi`)
		case 2:
			return sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	records := a.Records()
	closing := records[len(records)-1]
	if closing.Kind != "turn_end" || !strings.Contains(closing.Text, "boom") || closing.Meta == nil || closing.Meta.Steps != 1 || closing.Meta.Duration <= 0 {
		t.Fatalf("closing = %+v", closing)
	}
	// The closing message survives a reload from the database with its time and steps.
	stored, err := storedRecords(t, a)
	if err != nil || len(stored) == 0 || stored[len(stored)-1].Kind != "turn_end" || stored[len(stored)-1].Meta == nil || stored[len(stored)-1].Meta.Steps != 1 {
		t.Fatalf("stored %+v %v", stored, err)
	}
	// The model never sees it.
	if _, err := turn(t, a, "again"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if last := reqs[len(reqs)-1]; strings.Contains(last, "boom") {
		t.Fatalf("closing message reached the model: %s", last)
	}
}

func TestStoppedTurnClosesWithStopNote(t *testing.T) {
	fp := &fakeProvider{hold: true, reply: func(int, string) string { return "" }}
	a := newTestAgent(t, fp, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []TurnEvent
	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx, "hi", func(e TurnEvent) { events = append(events, e) })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(fp.requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("stopped turn returned no error")
	}
	records := a.Records()
	if closing := records[len(records)-1]; closing.Kind != "turn_end" || closing.Text != "stopped by user" {
		t.Fatalf("closing = %+v", closing)
	}
	if last := events[len(events)-1]; last.Kind != "turn_end" || last.Text != "stopped by user" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestFailedCommandDoesNotCloseTurn(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo boom >&2; exit 3")
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	// The failed command is tool output the model sees; the turn goes on to its answer.
	var kinds []string
	for _, r := range a.Records() {
		kinds = append(kinds, r.Kind+":"+r.Text)
	}
	if got := strings.Join(kinds, "|"); got != "user:go|code:echo boom >&2; exit 3|output:boom\n[exit 3]|assistant:ok" {
		t.Fatalf("records = %q", got)
	}
}

func TestFailedStreamCountsStepsSeenBeforeFailure(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"bash","call_id":"c1","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\":\"echo 1\"}"}`,
			`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	events, err := turn(t, a, "go")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	// Two streamed steps, one reasoning block and one bash call preview, count once each.
	if live := events[len(events)-1]; live.Kind != "turn_end" || !strings.Contains(live.Text, "boom") || live.Meta == nil || live.Meta.Steps != 2 {
		t.Fatalf("live closing = %+v", live)
	}
	stored, err := storedRecords(t, a)
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored %+v %v", stored, err)
	}
	if closing := stored[len(stored)-1]; closing.Kind != "turn_end" || closing.Meta == nil || closing.Meta.Steps != 2 {
		t.Fatalf("stored closing = %+v", closing)
	}
}

func TestStoppedStreamCountsStepsSeenBeforeCancel(t *testing.T) {
	reasoning := `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}}`
	fp := &fakeProvider{hold: true, reply: func(int, string) string { return sse(reasoning) }}
	a := newTestAgent(t, fp, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []TurnEvent
	seen := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx, "hi", func(e TurnEvent) {
			events = append(events, e)
			if e.Kind == "reasoning" {
				once.Do(func() { close(seen) })
			}
		})
	}()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("reasoning was never streamed")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("stopped turn returned no error")
	}
	if live := events[len(events)-1]; live.Kind != "turn_end" || live.Text != "stopped by user" || live.Meta == nil || live.Meta.Steps != 1 {
		t.Fatalf("live closing = %+v", live)
	}
	stored, err := storedRecords(t, a)
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored %+v %v", stored, err)
	}
	if closing := stored[len(stored)-1]; closing.Kind != "turn_end" || closing.Meta == nil || closing.Meta.Steps != 1 {
		t.Fatalf("stored closing = %+v", closing)
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
	isolate(t, home)
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
	a.session().History = append(a.session().History, raw(map[string]any{"role": "user", "content": "start"}),
		raw(map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": `{"command":"touch marker"}`}))
	if _, err := turn(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.session().dir, "marker")); err == nil {
		t.Fatal("interrupted command was rerun")
	}
	joined := ""
	for _, item := range a.session().History {
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
			return bashReply("c1", `echo hi`)
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

func TestSteeringMovesRunningCallToTaskAndSkipsRest(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			args := func(code string) string {
				data, _ := json.Marshal(map[string]any{"command": code})
				return string(data)
			}
			return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[`+
				`{"type":"function_call","call_id":"c1","name":"bash","arguments":%q},`+
				`{"type":"function_call","call_id":"c2","name":"bash","arguments":%q}]}}`,
				args("echo early\ntouch started\nsleep 1\necho late"), args("touch second")))
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- a.Run(context.Background(), "go", func(TurnEvent) {}) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(a.session().dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the call never started")
		}
	}
	a.Queue().Push(Input{Text: "STOP-AND-LISTEN"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("steering waited for the command: %s", took)
	}
	requests := fp.requests()
	if len(requests) != 2 || !strings.Contains(requests[1], "moved to task t1") || !strings.Contains(requests[1], "STOP-AND-LISTEN") || !strings.Contains(requests[1], "skipped: superseded by steering") {
		t.Fatalf("second request: %s", requests[len(requests)-1])
	}
	if _, err := os.Stat(filepath.Join(a.session().dir, "second")); !os.IsNotExist(err) {
		t.Fatal("the superseded second call ran")
	}
	job := a.session().tasks.jobs["t1"]
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background command did not finish")
	}
	log, err := os.ReadFile(job.log)
	if err != nil || !strings.Contains(string(log), "early") || !strings.Contains(string(log), "late") {
		t.Fatalf("lost task output: %q, %v", log, err)
	}
	if a.Queue().noticeCount() == 0 {
		t.Fatal("task completion did not notify the agent")
	}
}

func TestOnlyCompleteCommandsArePublished(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, _ string) string {
		if n > 1 {
			return textReply("ok")
		}
		return sse(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"bash","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":\"echo"}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":" final\"}"}`,
			`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"bash","call_id":"c1","arguments":"{\"command\":\"echo final\"}"}]}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	events, err := turn(t, a, "go")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == "code" {
			count++
			if e.Provisional || e.Text != "echo final" {
				t.Fatalf("partial command: %+v", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("command events: %d", count)
	}
}

func TestTaskStopAndSessionClose(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.Save(); err != nil { // a saved session keeps its task records
		t.Fatal(err)
	}
	if _, err := startTask(a, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	if a.RunningTasks() != 1 {
		t.Fatal("task not running")
	}
	j, err := a.session().tasks.find("t1")
	if err != nil {
		t.Fatal(err)
	}
	j.Stop()
	if got := awaitInput(t, a); got[0].Text != "<notification>t1 stopped</notification>" {
		t.Fatalf("result %+v", got)
	}
	if _, err := startTask(a, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	old, start := a.SessionID(), time.Now()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || a.RunningTasks() != 0 || len(a.Queue().Take()) != 0 {
		t.Fatal("session change did not stop the task quietly")
	}
	if list, err := a.db.Tasks(a.project, old); err != nil || len(list) != 2 || list[0].Status != "stopped" || list[1].Status != "stopped" {
		t.Fatalf("tasks %+v %v", list, err)
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
	if got := tailLines(path, 40); !strings.HasSuffix(got, "\nlast line") || len(got) > 256<<10 {
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

// bashReply is a response with one bash call.
func bashReply(callID, command string) string {
	args, _ := json.Marshal(map[string]any{"command": command})
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":%q,"name":"bash","arguments":%q}]}}`, callID, args))
}

func newTestAgent(t *testing.T, fp *fakeProvider, opts Options) *Runtime {
	return newTestAgentWith(t, fp, opts, nil)
}

func newTestAgentWith(t *testing.T, fp *fakeProvider, opts Options, pre func(home, dir string)) *Runtime {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	isolate(t, home)
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
	// Tests reload by hand; a hot reload in the background would race with them.
	a.OnConfigChanged(func() {})
	t.Cleanup(a.Close)
	return a
}

// isolate points ZEL_HOME at home and clears what a zel session around the
// test would export, so a test never becomes the child of a real session.
func isolate(t *testing.T, home string) {
	t.Helper()
	t.Setenv("ZEL_HOME", home)
	for _, name := range []string{"ZEL_MODE", "ZEL_SESSION_ID", "ZEL_SOCKET", "ZEL_TOKEN", "ZEL_DEPTH", "ZEL_CONFIG", "ZEL_PROVIDER", "ZEL_MODEL", "ZEL_EFFORT"} {
		t.Setenv(name, "")
	}
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
	s := a.session()
	_, items, err := s.db.Load(s.project, s.id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		out = append(out, string(item.Data))
	}
	return out
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
	slots := a.session().slots
	slots.mu.Lock()
	defer slots.mu.Unlock()
	return slots.n
}

func TestSessionUsageCountsEachRequestOnce(t *testing.T) {
	msg := func(role string) json.RawMessage {
		return json.RawMessage(`{"type":"message","role":"` + role + `","content":"x"}`)
	}
	a := &Runtime{cur: &session{History: []json.RawMessage{msg("user"), msg("assistant"), msg("assistant"), msg("assistant")}}}
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
	if _, err := startTask(a, "true"); err == nil || !strings.Contains(err.Error(), "unresolved task") {
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
	if _, err := startTask(a, "sleep 0.2"); err != nil {
		t.Fatal(err)
	}
	// A directory in place of tasks.jsonl makes recording the end fail.
	s := a.session()
	file := filepath.Join(s.sessionDir(), "tasks.jsonl")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	s.tasks.wg.Wait()
	inputs := a.Queue().Take()
	if len(inputs) != 1 || inputs[0].Text != "<notification>t1 finished</notification>" {
		t.Fatalf("completion: %+v", inputs)
	}
	if data, _ := os.ReadFile(s.eventsLog()); !strings.Contains(string(data), "could not be persisted") || !strings.Contains(string(data), "tasks.jsonl") {
		t.Fatalf("persistence failure not reported: %s", data)
	}
}

func TestFileBackedImagesAndLegacyHistory(t *testing.T) {
	a := newTestAgent(t, okProvider(), Options{})
	s := a.session()
	original := Input{Text: "image", Images: []shell.Image{{URL: "data:image/png;base64,aQ=="}}}
	staged, err := s.stageInput(original)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Images[0].URL == original.Images[0].URL || !strings.HasPrefix(staged.Images[0].URL, imageRefPrefix) {
		t.Fatal("not staged")
	}
	s.commitInput(original) // immutable legacy record
	legacy := append([]byte(nil), s.History[0]...)
	s.commitInput(staged)
	if strings.Contains(string(s.History[1]), "base64") {
		t.Fatal("encoded preview retained")
	}
	hydrated, err := s.hydrateImages(s.requestInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(hydrated) != 2 || !strings.Contains(string(hydrated[1]), "data:image/png;base64,aQ==") {
		t.Fatalf("hydrate: %s", hydrated)
	}
	if string(s.History[0]) != string(legacy) {
		t.Fatal("legacy was migrated")
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	id := s.id
	a.Close()
	resumed, err := New(s.dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	rs := resumed.session()
	if _, err := rs.hydrateImages(rs.requestInput()); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(staged.Images[0].URL, imageRefPrefix)
	if err := os.Remove(filepath.Join(rs.sessionDir(), "attachments", name)); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.hydrateImages(rs.requestInput()); err == nil {
		t.Fatal("missing preview silently omitted")
	}
	if _, err := rs.loadPreview("../escape.png"); err == nil {
		t.Fatal("accepted path traversal")
	}
	rs.compacted = compaction{Drop: len(rs.History)}
	if _, err := rs.hydrateImages(rs.requestInput()); err != nil {
		t.Fatalf("compacted image was read: %v", err)
	}
}

func TestFileBackedImageFailureDoesNotCommit(t *testing.T) {
	a := newTestAgent(t, okProvider(), Options{})
	s := a.session()
	if err := os.WriteFile(filepath.Join(s.sessionDir(), "attachments"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	err := a.RunInput(context.Background(), Input{Text: "image", Images: []shell.Image{{URL: "data:image/png;base64,aQ=="}}}, func(TurnEvent) {})
	if err == nil || len(s.History) != 0 {
		t.Fatalf("failed attachment committed: %v %d", err, len(s.History))
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
	b, err := New(a.session().dir, Options{})
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
	records, err := storedRecords(t, a)
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

func TestNotificationsAreBatchedInOrder(t *testing.T) {
	var q Steering
	q.PushNotice(notification("t1 finished"))
	q.PushNotice(notification("t2 failed"))
	q.Push(Input{Text: "typed"})
	q.PushNotice(notification("a1 finished"))
	var got []string
	for {
		in, ok := q.PopBatch()
		if !ok {
			break
		}
		got = append(got, in.Text)
	}
	want := []string{"<notification>t1 finished</notification>\n<notification>t2 failed</notification>", "typed", "<notification>a1 finished</notification>"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
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
	if _, err := startTask(h.a, "echo notice A"); err != nil {
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
	a, b := strings.Index(reqs[1], "t1 finished"), strings.Index(reqs[1], "steer B")
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

func TestReloadWaitsForTheTurnToEnd(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	global := filepath.Join(h.a.Home(), "zel.toml")
	turns := func(n int) {
		h.await(fmt.Sprintf("%d turns", n), func(events []Event) bool {
			return !h.ctl.Status().Busy && len(doneEvents(events)) == n
		})
	}
	h.send("a")
	turns(1)
	h.model.mu.Lock()
	h.model.gate = make(chan struct{})
	h.model.mu.Unlock()
	h.send("b")
	h.inFlight(2)
	if err := config.SetKey(global, "instructions", "INSTRUCTIONS-V2"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetKey(global, "settings.compaction", 4242); err != nil {
		t.Fatal(err)
	}
	h.ctl.Reload()
	if !h.ctl.Status().Busy {
		t.Fatal("reload interrupted the running turn")
	}
	if got := h.ctl.Settings().CompactionThreshold; got == 4242 {
		t.Fatal("config change applied mid-turn")
	}
	if info, err := h.ctl.Info(); err != nil || info.Session == "" {
		t.Fatalf("info during a turn: %+v %v", info, err)
	}
	close(h.model.gate)
	turns(2)
	if got := h.ctl.Settings().CompactionThreshold; got != 4242 {
		t.Fatalf("deferred reload not applied: %d", got)
	}
	h.send("c")
	turns(3)
	reqs := h.model.requests()
	if strings.Contains(reqs[1], "INSTRUCTIONS-V2") || !strings.Contains(reqs[2], "INSTRUCTIONS-V2") {
		t.Fatalf("prompt not rebuilt after the turn:\nb: %s\nc: %s", reqs[1], reqs[2])
	}
	if events := h.log(); !slices.ContainsFunc(events, func(e Event) bool { return e.Text == "zel.toml reloaded" }) {
		t.Fatalf("no reload notice: %s", describe(events))
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
	isolate(t, home)
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

func TestCompactionZones(t *testing.T) {
	big := func(id string) json.RawMessage {
		return callOutput(id, "first line of "+id+"\n"+strings.Repeat("x", 4000))
	}
	call := func(id string) json.RawMessage {
		return raw(map[string]any{"type": "function_call", "call_id": id, "name": "bash", "arguments": `{"command":"echo 1"}`})
	}
	reply := func(text string) json.RawMessage {
		return raw(map[string]any{"type": "message", "role": "assistant", "id": "msg_" + text, "content": []any{map[string]any{"type": "output_text", "text": text}}})
	}
	history := []json.RawMessage{
		raw(map[string]string{"type": "instructions", "text": "prompt"}),
		raw(map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "first"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}),
		raw(map[string]any{"type": "reasoning", "summary": []any{}}),
		call("c1"), big("c1"), reply("done1"),
		raw(map[string]any{"role": "user", "content": "second"}),
		call("c2"), big("c2"), reply("done2"),
		raw(map[string]any{"role": "user", "content": "third"}),
		call("c3"), big("c3"),
	}
	threshold := 4 * estimateTokens(history[12:13])
	bounds, changed := compactionBounds(history, threshold, compaction{})
	if !changed || bounds != (compaction{Prune: 12, Drop: 6}) {
		t.Fatalf("bounds = %+v changed=%v", bounds, changed)
	}
	if _, again := compactionBounds(history, threshold, bounds); again {
		t.Fatal("unchanged bounds reported as a new compaction")
	}
	s := &session{History: history, compacted: bounds}
	var sent []string
	for _, item := range s.requestInput() {
		sent = append(sent, string(item))
	}
	body := strings.Join(sent, "\n")
	for _, gone := range []string{`"c1"`, `"type":"reasoning"`, "input_image", "msg_done1", `"prompt"`} {
		if strings.Contains(body, gone) {
			t.Fatalf("%s survived compaction:\n%s", gone, body)
		}
	}
	if !strings.Contains(sent[0], "first") || !strings.Contains(sent[1], "done1") || !strings.Contains(sent[2], "context-compacted") || !strings.Contains(sent[3], "second") {
		t.Fatalf("conversation or marker out of order:\n%s", body)
	}
	if !strings.Contains(sent[5], `first line of c2\n[tool output pruned]`) || strings.Contains(sent[5], "xxx") {
		t.Fatalf("middle output not pruned: %s", sent[5])
	}
	if sent[len(sent)-1] != string(history[12]) {
		t.Fatal("fresh output changed")
	}
}

func TestAutoCompactionRunsBetweenStepsAndSurvivesResume(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 2 {
			return bashReply("c1", "seq 1 300")
		}
		return textReply("ok")
	}}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		if err := config.SetKey(filepath.Join(home, "zel.toml"), "settings.compaction", 100); err != nil {
			t.Fatal(err)
		}
	})
	for _, text := range []string{"one", "two"} {
		if _, err := turn(t, a, text); err != nil {
			t.Fatal(err)
		}
	}
	requests := fp.requests()
	if len(requests) != 3 || !strings.Contains(requests[2], "context-compacted") || strings.Contains(requests[1], "context-compacted") {
		t.Fatalf("compaction marker not sent after the tool step: %d requests", len(requests))
	}
	if !slices.ContainsFunc(a.Records(), func(r Record) bool { return r.Kind == "compaction" }) {
		t.Fatal("compaction not recorded")
	}
	s := a.session()
	want := s.compacted
	_, items, err := s.db.Load(s.project, s.id)
	resumed := &session{}
	resumed.readItems(items)
	if err != nil || resumed.compacted != want || want.Drop == 0 {
		t.Fatalf("resumed bounds %+v, want %+v (%v)", resumed.compacted, want, err)
	}
}

// storedRecords reads the session's records back from the database, with a
// connection of its own.
// storedRecords reads the session's records back from its session file, with a
// store of its own.
func storedRecords(t *testing.T, a *Runtime) ([]Record, error) {
	t.Helper()
	db, err := openDB(a.Home())
	if err != nil {
		return nil, err
	}
	_, items, err := db.Load(a.Project(), a.SessionID())
	if err != nil {
		return nil, err
	}
	s := &session{acts: &activities{}}
	s.readItems(items)
	return s.Records(), nil
}

// startTask runs code as a background task of the current session, as tasks.spawn does.
// startTask runs command as a background task of the current session, as zel task run does.
func startTask(a *Runtime, command string) (string, error) {
	j, err := a.session().tasks.spawn(command, startOpts{name: "test"})
	if err != nil {
		return "", err
	}
	return j.id, nil
}
