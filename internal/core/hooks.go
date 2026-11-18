package core

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"zel/internal/config"
	"zel/internal/luart"
)

// HookResult is what one hook returned.
type HookResult = luart.HookResult

// hookOutput is what the hooks of one event produced.
type hookOutput struct {
	input   map[string]any // the input after the rewriting hooks (field chain)
	outs    []string       // non-empty text each hook returned, in order
	blocked bool
	reason  string // what the blocking hook returned after false
}

// blockable events stop what they guard when a hook returns false.
var blockable = map[string]bool{"message": true, "notification": true, "lua_before": true}

// chained events: each hook gets the field as the previous hook rewrote it.
var chained = map[string]string{"message": "text", "notification": "text", "lua_before": "code"}

// hooksFor lists the hooks of the config for event, in config order.
func (s *session) hooksFor(event string) []config.Hook {
	cfg := s.conf.get()
	if cfg == nil {
		return nil
	}
	var list []config.Hook
	for _, h := range cfg.Hooks {
		if slices.Contains(h.Events, event) {
			list = append(list, h)
		}
	}
	return list
}

func (s *session) hasHooks(event string) bool { return len(s.hooksFor(event)) > 0 }

// runHooks calls the hooks of event one by one with input as their data. A
// returned string rewrites the chained field or is collected; false blocks a
// blockable event; an error is logged in Tracing and the hook is skipped.
// Async hooks start in their own VM and are not waited for.
func (s *session) runHooks(ctx context.Context, event string, input map[string]any) hookOutput {
	res := hookOutput{input: input}
	field := chained[event]
	lc := s.luaConfig()
	if lc == nil {
		return res
	}
	for _, h := range s.hooksFor(event) {
		if ctx.Err() != nil {
			break
		}
		started := time.Now()
		hookCtx := ctx
		if h.Async {
			// An async hook outlives the caller; the end of the session stops it.
			hookCtx = s.hookCtx
		}
		r, err := lc.RunHook(hookCtx, h, event, res.input)
		s.logHook(event, h, started, r, err)
		switch {
		case err != nil || h.Async:
		case r.Blocked && blockable[event]:
			res.blocked, res.reason = true, r.Reason
			return res
		case !r.HasText || strings.TrimSpace(r.Text) == "":
		case field != "":
			next := maps.Clone(res.input)
			next[field] = r.Text
			res.input = next
		default:
			res.outs = append(res.outs, strings.TrimRight(r.Text, "\n"))
		}
	}
	return res
}

// logHook records one hook call in Tracing: "event: source -> result, Nms",
// with the error or the returned text on the next lines.
func (s *session) logHook(event string, h config.Hook, started time.Time, r HookResult, err error) {
	result, detail := "ok", ""
	switch {
	case err != nil:
		result, detail = "error", err.Error()
	case h.Async:
		result = "async"
	case r.Blocked:
		result, detail = "blocked", r.Reason
	case r.HasText:
		detail = r.Text
	}
	line := fmt.Sprintf("%s: %s -> %s, %dms", event, h.Source, result, time.Since(started).Milliseconds())
	if detail = strings.TrimSpace(detail); detail != "" {
		line += "\n" + detail
	}
	s.LogEvent("hook", line)
}

// fireHooks runs the hooks of an event whose output is not used.
func (s *session) fireHooks(event string, input map[string]any) {
	if !s.hasHooks(event) || s.ended.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), config.HookTimeout)
	defer cancel()
	s.runHooks(ctx, event, input)
}

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
