package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"zel/internal/config"
	"zel/internal/shell"
)

// hookOutput is what the hooks of one event produced.
type hookOutput struct {
	input   map[string]any // the input after the rewriting hooks (field chain)
	outs    []string       // non-empty stdout of each hook, in order
	blocked bool
	reason  string // stderr of the hook that blocked (exit 2)
}

// blockable events stop what they guard when a hook exits with 2.
var blockable = map[string]bool{"message": true, "notification": true, "bash_before": true}

// chained events: each hook gets the field as the previous hook rewrote it.
var chained = map[string]string{"message": "text", "notification": "text", "bash_before": "command"}

// hooksFor lists the hooks of event that apply to this session's mode.
func (s *session) hooksFor(event string) []config.Hook {
	cfg := s.conf.get()
	if cfg == nil {
		return nil
	}
	var list []config.Hook
	for _, h := range cfg.Hooks[event] {
		if (s.mode() == "main" && h.Main) || (s.mode() == "headless" && h.Headless) {
			list = append(list, h)
		}
	}
	return list
}

func (s *session) hasHooks(event string) bool { return len(s.hooksFor(event)) > 0 }

// runHooks runs the hooks of event one by one with input as JSON on stdin.
// Exit 0: stdout counts (rewrites the chained field, or is collected); exit 2:
// blocks a blockable event; anything else is logged in Tracing and skipped.
func (s *session) runHooks(ctx context.Context, event string, input map[string]any) hookOutput {
	res := hookOutput{input: input}
	field := chained[event]
	for _, h := range s.hooksFor(event) {
		if ctx.Err() != nil {
			break
		}
		if h.Match != "" {
			subject, _ := res.input[config.MatchField[event]].(string)
			if re, err := regexp.Compile(h.Match); err != nil || !re.MatchString(subject) {
				continue
			}
		}
		data, _ := json.Marshal(res.input)
		if h.Async {
			s.hookMu.Lock()
			if s.ended.Load() {
				s.hookMu.Unlock()
				continue
			}
			s.hookWG.Add(1)
			life := s.hookCtx
			if life == nil {
				life = context.Background()
			}
			go func() { defer s.hookWG.Done(); s.execHook(life, event, h, data) }()
			s.hookMu.Unlock()
			continue
		}
		out, errText, code := s.execHook(ctx, event, h, data)
		out = strings.TrimRight(out, "\n")
		switch {
		case code == 2 && blockable[event]:
			res.blocked, res.reason = true, strings.TrimSpace(errText)
			return res
		case code != 0:
			continue
		case out == "":
		case field != "":
			next := map[string]any{}
			for k, v := range res.input {
				next[k] = v
			}
			next[field] = out
			res.input = next
		default:
			res.outs = append(res.outs, out)
		}
	}
	return res
}

// fireHooks runs the hooks of an event whose output is not used.
func (s *session) fireHooks(event string, input map[string]any) {
	if !s.hasHooks(event) {
		return
	}
	s.hookMu.Lock()
	if s.ended.Load() {
		s.hookMu.Unlock()
		return
	}
	s.hookWG.Add(1)
	life := s.hookCtx
	if life == nil {
		life = context.Background()
	}
	s.hookMu.Unlock()
	defer s.hookWG.Done()
	ctx, cancel := context.WithTimeout(life, config.HookTimeout)
	defer cancel()
	s.runHooks(ctx, event, input)
}

const hookOutputLimit = 1 << 20

// execHook runs one hook and logs it in Tracing.
func (s *session) execHook(ctx context.Context, event string, h config.Hook, stdin []byte) (out, errText string, code int) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = config.HookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env := s.shellEnv()
	env["ZEL_HOOK"] = event
	cmd := shell.Command(ctx, s.dir, env, h.Run)
	stdout, stderr := shell.NewOutputBuffer(hookOutputLimit), shell.NewOutputBuffer(64<<10)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	started := time.Now()
	code = 0
	if err := cmd.Start(); err != nil {
		code = 127
		errText = err.Error()
	} else if err := shell.Reap(cmd); err != nil {
		code = exitCode(err)
		if ctx.Err() != nil {
			code, errText = 124, "timed out after "+timeout.String()
		}
	}
	if errText == "" {
		errText = stderr.Text()
	}
	line := fmt.Sprintf("%s: %s -> exit %d, %dms", event, FirstLine(h.Run), code, time.Since(started).Milliseconds())
	if code != 0 && code != 2 && strings.TrimSpace(errText) != "" {
		line += "\n" + strings.TrimSpace(errText)
	}
	s.LogEvent("hook", line)
	return stdout.Text(), errText, code
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
