package control

import (
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
	"zel/internal/agent"
	"zel/internal/shell"
)

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
	if _, err := agent.RunTask(h.dir, h.a.SessionID(), "echo notice A"); err != nil {
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
	if codeOf(h.ctl.Input(agent.Input{Text: "late"})) != CodeFailed {
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
	home, _ := agent.ZelHome(h.dir)
	settings := h.ctl.Settings()
	settings.ShellTimeout = 42
	if err := agent.WriteConfig(home, settings); err != nil {
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
	h := newHarnessWith(t, model, func(a *agent.Agent) {
		a.Queue().PushNotice(agent.Input{Text: "NOTICE-A", Notice: true})
	})
	h.await("the prequeued turn", func(events []Event) bool { return len(doneEvents(events)) == 1 })
	if err := h.ctl.Input(agent.Input{Text: "B"}); err != nil {
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
			h := newHarnessWith(t, &fakeModel{}, func(a *agent.Agent) {
				for i := 0; i < 4; i++ {
					producers.Add(1)
					go func(i int) {
						defer producers.Done()
						for j := 0; j < 8; j++ {
							a.Queue().PushNotice(agent.Input{Text: fmt.Sprintf("notice-%d-%d", i, j), Notice: true})
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
	a      *agent.Agent
	model  *fakeModel
	dir    string
	mu     sync.Mutex
	events []Event
}

func newHarness(t *testing.T, model *fakeModel) *harness {
	t.Helper()
	return newHarnessWith(t, model, nil)
}

func newHarnessWith(t *testing.T, model *fakeModel, pre func(*agent.Agent)) *harness {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("ZEL_HOME", home)
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	s := agent.DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = agent.Provider{BaseURL: server.URL}
	if err := agent.WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(dir, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(a)
	}
	h := &harness{t: t, a: a, model: model, dir: dir}
	h.ctl = New(a, func(e Event) {
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
	if err := h.ctl.Input(agent.Input{Text: text, Images: images}); err != nil {
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
	if err := h.ctl.Input(agent.Input{Images: []shell.Image{{URL: "data:image/png;base64,aQ=="}}}); err != nil {
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
