package core

import (
	"context"
	"strings"
	"time"

	"zel/internal/config"
)

// hookOutput is what the hooks of one event produced.
type hookOutput struct {
	input   map[string]any // the input after the rewriting hooks (field chain)
	outs    []string       // non-empty stdout of each hook, in order
	blocked bool
	reason  string // stderr of the hook that blocked
}

func (s *session) hooksFor(string) []config.Hook { return nil }

func (s *session) hasHooks(string) bool { return false }

func (s *session) runHooks(_ context.Context, _ string, input map[string]any) hookOutput {
	return hookOutput{input: input}
}

func (s *session) fireHooks(string, map[string]any) {}

// hookInput lets message hooks rewrite or block what the user typed, and
// notification hooks rewrite or drop a notification. ok is false when the input
// must not reach the model.
func (s *session) hookInput(ctx context.Context, input Input, emit func(TurnEvent)) (Input, bool) {
	event := "message"
	if input.Notice {
		event = "notification"
	}
	if input.Text == "" || !s.hasHooks(event) {
		return input, true
	}
	text, wrapped := input.Text, false
	if input.Notice {
		text, wrapped = unwrapNotification(text)
	}
	res := s.runHooks(ctx, event, map[string]any{"text": text})
	if res.blocked {
		if !input.Notice {
			emit(TurnEvent{Kind: "notice", Text: "Message blocked by a hook: " + res.reason})
		}
		return input, false
	}
	out, _ := res.input["text"].(string)
	switch {
	case !wrapped:
		input.Text = out
	case out != text:
		// The hook rewrote the plain text; it may print the tags itself.
		if _, tagged := unwrapNotification(out); tagged {
			input.Text = strings.TrimSpace(out)
		} else {
			input.Text = notification(out).Text
		}
	}
	return input, true
}

// unwrapNotification returns the text inside one <notification>…</notification>
// block; ok is false when text is not exactly one such block.
func unwrapNotification(text string) (string, bool) {
	t := strings.TrimSpace(text)
	const open, end = "<notification>", "</notification>"
	if !strings.HasPrefix(t, open) || !strings.HasSuffix(t, end) || strings.Count(t, open) != 1 {
		return text, false
	}
	return strings.TrimSpace(t[len(open) : len(t)-len(end)]), true
}

// turnEnd runs the turn_end hooks; returned text becomes a notification that continues the turn.
func (s *session) turnEnd(ctx context.Context) string {
	if !s.hasHooks("turn_end") {
		return ""
	}
	running := []map[string]any{}
	for _, t := range s.ActiveTasks() {
		running = append(running, map[string]any{"id": t.ID, "name": t.Name, "ms": time.Since(t.Started).Milliseconds()})
	}
	return strings.Join(s.runHooks(ctx, "turn_end", map[string]any{"answer": s.FinalText(), "running": running}).outs, "\n")
}

// sessionCreated runs the session_created hooks once per new session.
func (s *session) sessionCreated() { s.fireHooks("session_created", map[string]any{}) }

// sessionClosed leaves the session: its tasks are stopped and then the session_closed
// hooks run with what is left of the 5 s shutdown budget.
func (s *session) sessionClosed(reason string) {
	t := s.tasks
	if t == nil {
		return
	}
	t.close(func(ctx context.Context) {
		if s.hasHooks("session_closed") {
			s.runHooks(ctx, "session_closed", map[string]any{"reason": reason})
		}
	})
}

// checkIdle fires the idle hooks once when no turn runs and nothing runs in
// the background any more.
func (s *session) checkIdle() {
	if s.ended.Load() || s.turning.Load() || s.RunningTasks() > 0 || len(s.ActiveSubagents()) > 0 || s.queue.noticeCount() > 0 {
		return
	}
	if s.idleSent.CompareAndSwap(false, true) {
		go s.fireHooks("idle", map[string]any{})
	}
}
