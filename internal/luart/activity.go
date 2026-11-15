package luart

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// Activity labels say what a host operation does for the working indicator, such
// as "reading ~/.zel/init.lua". They never show secret values.

const maxLabel = 160 // runes; the UI truncates again to the terminal width

// simpleVerbs are host functions labeled by a verb and their first argument.
var simpleVerbs = map[string]string{
	"view": "viewing", "read": "reading", "edit": "editing", "patch": "editing",
	"glob": "finding files", "exists": "checking",
}

// backgroundKey marks the context of a background task (tasks.spawn); its host
// operations never take over the foreground indicator.
type backgroundKey struct{}

// WithBackground returns ctx marked as background work for host operations.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func isBackground(ctx context.Context) bool {
	return ctx != nil && ctx.Value(backgroundKey{}) != nil
}

// activityTrackerKey attaches an activityTracker to a foreground call context
// so abandoning a stuck call ends every activity it started.
type activityTrackerKey struct{}

// activityTracker tracks the foreground activities started by one call so the
// caller can close them immediately when it stops waiting, without racing with
// or double-closing a late host return.
type activityTracker struct {
	mu     sync.Mutex
	closed bool
	ends   []func()
}

func withActivityTracker(ctx context.Context, t *activityTracker) context.Context {
	return context.WithValue(ctx, activityTrackerKey{}, t)
}

func activityTrackerFrom(ctx context.Context) *activityTracker {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(activityTrackerKey{}).(*activityTracker)
	return t
}

// begin calls start and registers its end function unless the call was already
// abandoned. The returned cleanup is idempotent and safe alongside closeAll.
func (t *activityTracker) begin(start func() func()) func() {
	if t == nil {
		return start()
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return func() {}
	}
	end := start()
	idx := len(t.ends)
	t.ends = append(t.ends, end)
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		var fn func()
		if idx < len(t.ends) {
			fn = t.ends[idx]
			t.ends[idx] = nil
		}
		t.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
}

// closeAll ends every still-open foreground activity owned by this call and blocks
// any further activity registration from it.
func (t *activityTracker) closeAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	ends := t.ends
	t.ends = nil
	t.mu.Unlock()
	for i := len(ends) - 1; i >= 0; i-- {
		if ends[i] != nil {
			ends[i]()
		}
	}
}

// hostFn wraps a host function so its activity shows while it runs; the activity
// ends on every exit, errors, cancellation and call abandonment included.
func (v *vm) hostFn(name string, fn lua.LGFunction) *lua.LFunction {
	return v.L.NewFunction(func(L *lua.LState) int {
		text := scrubLabel(v.describe(name, L))
		if text == "" {
			return fn(L)
		}
		if isBackground(v.ctx) {
			defer v.env.Begin(text, true)()
			return fn(L)
		}
		defer activityTrackerFrom(v.ctx).begin(func() func() {
			return v.env.Begin(text, false)
		})()
		return fn(L)
	})
}

// describe builds the label of a call before secrets are removed; it only reads
// arguments with type checks, so it never raises.
func (v *vm) describe(name string, L *lua.LState) string {
	arg := func(n int) string { return textOf(L.Get(n)) }
	field := func(n int, key string) string { return textOf(opts(L, n).RawGetString(key)) }
	if verb, ok := simpleVerbs[name]; ok {
		return phrase(verb, arg(1))
	}
	switch name {
	case "print", "slice", "log", "docs":
		return "" // bookkeeping: no label
	case "sh":
		if label := shellLabel(arg(1)); label != "" {
			return "$ " + label
		}
		return "$"
	case "write":
		if opts(L, 3).RawGetString("append") == lua.LTrue {
			return phrase("appending to", arg(1))
		}
		return phrase("writing", arg(1))
	case "list":
		return phrase("listing", fallback(arg(1), "."))
	case "grep":
		// The pattern is free text that may be a secret, so only the directory shows.
		if p := field(2, "path"); p != "" {
			return phrase("searching in", p)
		}
		return "searching files"
	case "tmp":
		return "creating a temp file"
	case "models":
		return "listing models"
	case "session.rename":
		return "renaming session"
	case "new_agent":
		return phrase("running agent in", v.agentModel(L))
	case "send_agent":
		return "messaging an agent"
	case "choice":
		return "waiting for your choice"
	}
	if ns, op, _ := strings.Cut(name, "."); ns == "tasks" {
		return v.jobLabel(op, L)
	}
	return phrase("running", name) // tools from init.lua and anything else
}

// jobLabel describes a call of the tasks namespace.
func (v *vm) jobLabel(op string, L *lua.LState) string {
	id := textOf(L.Get(1))
	switch op {
	case "spawn":
		return phrase("starting task", id)
	case "list":
		return "listing tasks"
	case "get":
		return phrase("checking task", id)
	case "wait":
		if t, ok := L.Get(1).(*lua.LTable); ok {
			return phrase("waiting for", fmt.Sprintf("%d tasks", t.Len()))
		}
		return phrase("waiting for task", id)
	case "tail":
		return phrase("reading the output of task", id)
	case "stop":
		return phrase("stopping task", id)
	}
	return phrase("running", "tasks."+op)
}

// agentModel is the model an agent call names, or the session's, with its effort after a slash.
func (v *vm) agentModel(L *lua.LState) string {
	o := opts(L, 1)
	model := textOf(o.RawGetString("model"))
	if model == "" {
		model = v.env.Info().Model
	}
	if effort := textOf(o.RawGetString("effort")); effort != "" {
		return model + "/" + effort
	}
	return model
}

// textOf returns a string or number argument as text; other values give "".
func textOf(v lua.LValue) string {
	switch x := v.(type) {
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	}
	return ""
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// phrase joins the non-empty parts with spaces.
func phrase(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " ")
}

// shellLabel describes a bash command for the working indicator: its first line, with
// "…" when more follows. scrubLabel removes secrets and the UI cuts it to the width.
func shellLabel(script string) string {
	first, _, multi := strings.Cut(strings.TrimSpace(script), "\n")
	first = strings.Join(strings.Fields(first), " ")
	if first != "" && multi {
		first += " …"
	}
	return first
}

// The secret patterns are heuristics; they cannot know every secret.
var (
	bearerValue  = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{6,}`)
	assignedKey  = regexp.MustCompile(`(?i)([\w.-]*(?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|auth|credential|private[_-]?key|access[_-]?key)[\w.-]*\s*[:=]\s*)("[^"]*"|'[^']*'|\S+)`)
	flagValue    = regexp.MustCompile(`(?i)(--?(?:token|secret|passw(?:or)?d|api[_-]?key|auth)\s+)("[^"]*"|'[^']*'|\S+)`)
	vendorSecret = regexp.MustCompile(`\b(sk|pk|ghp|gho|ghs|ghu|ghr|github_pat|xox[abprs])[-_][A-Za-z0-9_-]{8,}`)
	urlCreds     = regexp.MustCompile(`(://)[^/\s:@]+:[^@\s]+@`)
	userFlag     = regexp.MustCompile(`(\s(?:-u|--user)\s+)\S+`)
)

// scrubText replaces secret-looking values with [redacted].
func scrubText(s string) string {
	s = bearerValue.ReplaceAllString(s, "$1 [redacted]")
	s = assignedKey.ReplaceAllString(s, "${1}[redacted]")
	s = flagValue.ReplaceAllString(s, "${1}[redacted]")
	s = urlCreds.ReplaceAllString(s, "${1}[redacted]@")
	s = userFlag.ReplaceAllString(s, "${1}[redacted]")
	return vendorSecret.ReplaceAllString(s, "[redacted]")
}

// scrubLabel makes a label one line, removes secrets and bounds its length.
func scrubLabel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …" // first line of a command only
	}
	s = scrubText(strings.Join(strings.Fields(s), " "))
	if utf8.RuneCountInString(s) > maxLabel {
		s = string([]rune(s)[:maxLabel-1]) + "…"
	}
	return s
}
