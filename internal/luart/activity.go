package luart

import (
	"context"
	"fmt"
	"net/url"
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
	"view": "viewing", "fs.read": "reading", "fs.edit": "editing", "fs.patch": "editing",
	"fs.glob": "finding files", "fs.exists": "checking",
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
	case "print", "slice", "json.encode", "json.decode", "log.info", "log.warn":
		return "" // bookkeeping: no label
	case "sh":
		if label := shellLabel(arg(1)); label != "" {
			return "running bash: " + label
		}
		return "running bash"
	case "sleep":
		return phrase("sleeping", arg(1), "sec")
	case "fs.write":
		if opts(L, 3).RawGetString("append") == lua.LTrue {
			return phrase("appending to", arg(1))
		}
		return phrase("writing", arg(1))
	case "fs.list":
		return phrase("listing", fallback(arg(1), "."))
	case "fs.grep":
		// The pattern is free text that may be a secret, so only the directory shows.
		if p := field(2, "path"); p != "" {
			return phrase("searching in", p)
		}
		return "searching files"
	case "fs.tmp":
		return "creating a temp file"
	case "web.get":
		return phrase("GET", safeURL(arg(1)))
	case "web.post":
		return phrase("POST", safeURL(arg(1)))
	case "web.request":
		return phrase(fallback(strings.ToUpper(field(1, "method")), "GET"), safeURL(field(1, "url")))
	case "state.get":
		return "reading state"
	case "state.set":
		// Keys are free text and may hold secrets: the label names the action only.
		if L.Get(2) == lua.LNil {
			return "deleting state"
		}
		return "writing state"
	case "state.list":
		return "listing state keys"
	case "log.notice":
		return "showing a notice"
	case "session.models":
		return "listing models"
	case "session.rename":
		return "renaming session"
	case "agents.spawn":
		return phrase("starting agent in", v.agentModel(L))
	case "agents.run":
		return phrase("running agent in", v.agentModel(L))
	}
	if ns, op, _ := strings.Cut(name, "."); ns == "tasks" || ns == "agents" {
		return v.jobLabel(ns, op, L)
	}
	return phrase("running", name) // plugin functions (zel.fn) and anything else
}

// jobLabel describes a call of the tasks or agents namespace.
func (v *vm) jobLabel(ns, op string, L *lua.LState) string {
	noun, id := strings.TrimSuffix(ns, "s"), textOf(L.Get(1))
	switch op {
	case "spawn":
		if ns == "tasks" {
			return phrase("starting task", textOf(opts(L, 2).RawGetString("name")))
		}
	case "list":
		return "listing " + ns
	case "get", "status":
		return phrase("checking", noun, id)
	case "wait":
		return phrase("waiting for", noun, id)
	case "wait_all":
		n := 0
		if t, ok := L.Get(1).(*lua.LTable); ok {
			n = t.Len()
		}
		return phrase("waiting for", fmt.Sprintf("%d %ss", n, noun))
	case "result":
		return phrase("reading the result of", noun, id)
	case "tail":
		return phrase("reading the output of", noun, id)
	case "stop":
		return phrase("stopping", noun, id)
	}
	return phrase("running", ns+"."+op)
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

// safeURL shows the scheme and host. Credentials, paths, queries and fragments
// are where secrets go, so only the presence of a path, query or fragment shows
// ("…"). Text that is not a URL with a scheme and host shows as "…".
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "…"
	}
	out := u.Scheme + "://" + u.Host
	if p := u.EscapedPath(); p != "" && p != "/" {
		out += "/…"
	}
	if u.RawQuery != "" {
		out += "?…"
	}
	if u.Fragment != "" || u.RawFragment != "" {
		out += "#…"
	}
	return out
}

// inertCommands are programs whose plain path arguments show in a label. Every
// other program shows its name only: its arguments may be credentials, and no
// pattern tells them apart reliably.
var inertCommands = map[string]bool{"cd": true, "ls": true, "cat": true, "head": true, "tail": true, "wc": true, "pwd": true}

var (
	programWord = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,64}$`)
	plainWord   = regexp.MustCompile(`^[A-Za-z0-9._/~+-]{1,256}$`)
)

// shellLabel describes a bash command for the working indicator. It shows the
// program name, then "…" when more follows. The arguments show only for an inert
// program on one line whose arguments are all plain words; a command with quotes,
// assignments, operators or substitutions shows no argument at all.
func shellLabel(script string) string {
	first, _, multi := strings.Cut(strings.TrimSpace(script), "\n")
	words := strings.Fields(first)
	if len(words) == 0 {
		return ""
	}
	prog := words[0]
	if !programWord.MatchString(prog) {
		return "…"
	}
	if len(words) == 1 && !multi {
		return prog
	}
	args := words[1:]
	if !multi && inertCommands[prog] && plainArgs(args) {
		return prog + " " + strings.Join(args, " ")
	}
	return prog + " …"
}

// plainArgs reports whether every argument is a plain path-like word. Flags are
// not plain: a flag is often followed by its value.
func plainArgs(words []string) bool {
	for _, w := range words {
		if strings.HasPrefix(w, "-") || !plainWord.MatchString(w) {
			return false
		}
	}
	return true
}

// The secret patterns are heuristics; they cannot know every secret.
var (
	bearerValue  = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{6,}`)
	assignedKey  = regexp.MustCompile(`(?i)([\w.-]*(?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|auth|credential|private[_-]?key|access[_-]?key)[\w.-]*\s*[:=]\s*)("[^"]*"|'[^']*'|\S+)`)
	flagValue    = regexp.MustCompile(`(?i)(--?(?:token|secret|passw(?:or)?d|api[_-]?key|auth)\s+)(\S+)`)
	vendorSecret = regexp.MustCompile(`\b(sk|pk|ghp|gho|ghs|ghu|ghr|github_pat|xox[abprs])[-_][A-Za-z0-9_-]{8,}`)
)

// scrubText replaces secret-looking values with [redacted].
func scrubText(s string) string {
	s = bearerValue.ReplaceAllString(s, "$1 [redacted]")
	s = assignedKey.ReplaceAllString(s, "${1}[redacted]")
	s = flagValue.ReplaceAllString(s, "${1}[redacted]")
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
