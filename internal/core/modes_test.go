package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// send_agent gives the same subagent another message: it keeps its context, and
// only a subagent this session started can be written to.
func TestSendAgentKeepsContext(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var mu sync.Mutex
	var second string
	fp := &fakeProvider{reply: func(n int, body string) string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-two"):
			second = body
			return textReply("second answer")
		case strings.Contains(body, "child-one"):
			return textReply("first answer")
		}
		return luaReply("c1", `
local r = new_agent{prompt = "child-one"}
local s = send_agent(r.session, "child-two")
local ok, e = pcall(send_agent, "nope", "x")
print(r.ok, r.answer, s.ok, s.answer, s.session == r.session)
print(e)`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	out := fp.requests()[len(fp.requests())-1]
	for _, want := range []string{`true\tfirst answer\ttrue\tsecond answer\ttrue`, "send_agent: ENOENT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(second, "child-one") || !strings.Contains(second, "first answer") {
		t.Fatalf("the subagent lost its context: %s", second)
	}
}

func TestSessionModeInLua(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var mu sync.Mutex
	var childOut string
	fp := &fakeProvider{reply: func(n int, body string) string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(body, `"c2"`):
			childOut = body
			return textReply("ok")
		case strings.Contains(body, `"c1"`):
			return textReply("done")
		case strings.Contains(body, "child-mode"):
			return luaReply("c2", `return session.mode`)
		}
		return luaReply("c1", `print(session.mode); new_agent{prompt = "child-mode"}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if out := fp.requests()[len(fp.requests())-1]; !strings.Contains(out, `"output":"main"`) {
		t.Fatalf("main: %s", out)
	}
	mu.Lock()
	if !strings.Contains(childOut, `"output":"agent"`) {
		t.Fatalf("agent: %s", childOut)
	}
	mu.Unlock()

	fp = &fakeProvider{reply: func(n int, body string) string {
		if strings.Contains(body, `"c1"`) {
			return textReply("done")
		}
		return luaReply("c1", `print(session.mode)`)
	}}
	b := newTestAgent(t, fp, Options{Mode: "headless"})
	if _, err := turn(t, b, "go"); err != nil {
		t.Fatal(err)
	}
	if out := fp.requests()[len(fp.requests())-1]; !strings.Contains(out, `"output":"headless"`) {
		t.Fatalf("headless: %s", out)
	}
}

// session_created runs once per new session: not when a session is resumed.
func TestSessionCreatedHookRunsOncePerNewSession(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("hi") }}
	var dir string
	a := newTestAgentWith(t, fp, Options{}, func(home, d string) {
		dir = d
		initLua(t, `return {hooks = {session_created = function() write(session.cwd .. "/created.log", session.id .. "\n", {append = true}) end}}`)(home, d)
	})
	lines := func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "created.log"))
		return len(strings.Fields(string(data)))
	}
	if lines() != 1 {
		t.Fatalf("new runtime: %d", lines())
	}
	first := a.SessionID()
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if lines() != 2 {
		t.Fatalf("new session: %d", lines())
	}
	if err := a.Resume(first); err != nil {
		t.Fatal(err)
	}
	if lines() != 2 {
		t.Fatalf("resume must not run the hook: %d", lines())
	}
}

func TestChooserAsksAndAnswers(t *testing.T) {
	c := &chooser{}
	if _, err := c.ask(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("no UI: choice must fail")
	}
	changed := make(chan struct{}, 8)
	c.changed = func() { changed <- struct{}{} }
	type res struct {
		i   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		i, err := c.ask(context.Background(), "q", []string{"a", "b"})
		done <- res{i, err}
	}()
	<-changed
	c.mu.Lock()
	p := *c.pending
	c.mu.Unlock()
	if p.Title != "q" || len(p.Options) != 2 {
		t.Fatalf("pending: %+v", p)
	}
	c.answer(p.ID+1, 0) // an answer to another question is ignored
	c.answer(p.ID, 5)   // so is an option that does not exist
	select {
	case r := <-done:
		t.Fatalf("answered by a wrong answer: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	c.answer(p.ID, 1)
	if r := <-done; r.err != nil || r.i != 1 {
		t.Fatalf("answer: %+v", r)
	}
	// Cancel by the user, and by the context.
	go func() {
		i, err := c.ask(context.Background(), "q", []string{"a"})
		done <- res{i, err}
	}()
	for c.pendingNow() == nil {
		time.Sleep(time.Millisecond)
	}
	c.answer(c.pendingNow().ID, -1)
	if r := <-done; r.err != nil || r.i != -1 {
		t.Fatalf("cancel: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		i, err := c.ask(ctx, "q", []string{"a"})
		done <- res{i, err}
	}()
	for c.pendingNow() == nil {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if r := <-done; r.err == nil {
		t.Fatalf("a cancelled call must fail: %+v", r)
	}
	if c.pendingNow() != nil {
		t.Fatal("the question stays after the call ended")
	}
}

func (c *chooser) pendingNow() *Choice {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
}

// choice() in Lua reaches the UI through the runtime and returns the picked option.
func TestChoiceEndToEnd(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if strings.Contains(body, `"c1"`) {
			return textReply("done")
		}
		return luaReply("c1", `local c = choice("Which color", {"Blue", "Red"}); print(c); print(choice("again", {"x"}))`)
	}}
	a := newTestAgent(t, fp, Options{})
	a.OnChoiceChanged(func() {})
	answered := make(chan string, 2)
	go func() {
		for _, index := range []int{1, -1} {
			var q *Choice
			for q = a.PendingChoice(); q == nil; q = a.PendingChoice() {
				time.Sleep(time.Millisecond)
			}
			answered <- q.Title
			a.AnswerChoice(q.ID, index)
			for a.PendingChoice() != nil && a.PendingChoice().ID == q.ID {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if first, second := <-answered, <-answered; first != "Which color" || second != "again" {
		t.Fatalf("questions: %q %q", first, second)
	}
	out := fp.requests()[len(fp.requests())-1]
	if !strings.Contains(out, `"output":"Red\nnil"`) {
		t.Fatalf("answers: %s", out)
	}
	// Without a UI (nobody registered) the call fails instead of waiting forever.
	a.OnChoiceChanged(nil)
	r := luaEnv{a.session()}
	if _, _, err := r.Choose(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("choose without UI")
	}
}
