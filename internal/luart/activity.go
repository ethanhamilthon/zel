package luart

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// Activity labels say what a host operation does, in a few words, for the working
// indicator: "reading ~/.zel/init.lua", "sleeping 20 sec". They show real arguments
// where that helps and never the values that can carry secrets: request bodies and
// headers, environment values, state values, the text of a log message.

// maxLabel bounds a label in runes; the UI truncates again to the terminal width.
const maxLabel = 160

// backgroundKey marks the context of a background task (tasks.spawn). Operations
// started there are background work: they never take over the foreground
// indicator, which shows the operation the turn is waiting on.
type backgroundKey struct{}

// WithBackground returns ctx marked as background work for host operations.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func isBackground(ctx context.Context) bool {
	return ctx != nil && ctx.Value(backgroundKey{}) != nil
}

// hostFn creates a host function that reports its activity while it runs. Its
// label is computed from the arguments before the call. The activity ends on
// every exit: returns, errors, and cancellation (the deferred end).
func (v *vm) hostFn(name string, fn lua.LGFunction) *lua.LFunction {
	return v.L.NewFunction(func(L *lua.LState) int {
		text := v.label(name, L)
		if text == "" {
			return fn(L)
		}
		end := v.env.Begin(text, isBackground(v.ctx))
		defer end()
		return fn(L)
	})
}

// label describes what the host function name does with the arguments on L.
// Bookkeeping functions that finish at once have no label ("").
func (v *vm) label(name string, L *lua.LState) string {
	return scrubLabel(v.describe(name, L))
}

// describe builds the label before secrets are removed. It reads the arguments
// with type checks only, so it never raises.
func (v *vm) describe(name string, L *lua.LState) string {
	arg := func(n int) string { return textOf(L.Get(n)) }
	field := func(n int, key string) string {
		if t, ok := L.Get(n).(*lua.LTable); ok {
			return textOf(t.RawGetString(key))
		}
		return ""
	}
	flag := func(n int, key string) bool {
		t, ok := L.Get(n).(*lua.LTable)
		return ok && t.RawGetString(key) == lua.LTrue
	}
	ns, op, _ := strings.Cut(name, ".")
	switch name {
	case "sh":
		return phrase("running bash:", arg(1))
	case "sleep":
		return phrase("sleeping", arg(1), "sec")
	case "view":
		return phrase("viewing", arg(1))
	case "print", "slice", "json.encode", "json.decode", "log.info", "log.warn":
		return ""
	case "fs.read":
		return phrase("reading", arg(1))
	case "fs.write":
		if flag(3, "append") {
			return phrase("appending to", arg(1))
		}
		return phrase("writing", arg(1))
	case "fs.edit", "fs.patch":
		return phrase("editing", arg(1))
	case "fs.list":
		return phrase("listing", cmpDefault(arg(1), "."))
	case "fs.glob":
		return phrase("finding files", arg(1))
	case "fs.grep":
		return phrase("searching for", arg(1), inPath(field(2, "path")))
	case "fs.exists":
		return phrase("checking", arg(1))
	case "fs.tmp":
		return "creating a temp file"
	case "web.request":
		return phrase(cmpDefault(strings.ToUpper(field(1, "method")), "GET"), safeURL(field(1, "url")))
	case "web.get":
		return phrase("GET", safeURL(arg(1)))
	case "web.post":
		return phrase("POST", safeURL(arg(1)))
	case "state.get":
		return phrase("reading state", arg(1))
	case "state.set":
		if L.Get(2) == lua.LNil {
			return phrase("deleting state", arg(1))
		}
		return phrase("writing state", arg(1))
	case "state.list":
		return "listing state keys"
	case "log.notice":
		return "showing a notice"
	case "session.models":
		return "listing models"
	case "session.rename":
		return "renaming session"
	case "agents.spawn", "agents.run":
		verb := "starting"
		if name == "agents.run" {
			verb = "running"
		}
		return phrase(verb, "agent in", v.agentModel(L))
	}
	switch ns {
	case "tasks", "agents":
		return v.jobLabel(ns, op, L)
	case "zel":
		return "" // registration while init.lua loads
	}
	return phrase("running", name)
}

// jobLabel describes a call of the tasks or agents namespace.
func (v *vm) jobLabel(ns, op string, L *lua.LState) string {
	noun := strings.TrimSuffix(ns, "s")
	id := textOf(L.Get(1))
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

// agentModel is the model an agent call names: its own option or the session's,
// with the effort after a slash when one is given.
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

func cmpDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func inPath(path string) string {
	if path == "" {
		return ""
	}
	return "in " + path
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

// safeURL shows a URL without its user, query and fragment, where credentials and
// tokens usually are. A query is marked "?…". Anything that does not parse as an
// absolute URL goes through scrubText instead.
func safeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return scrubText(raw)
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?…"
	}
	return out
}

// The secret patterns are heuristics for what a command line or a label can
// carry; they cannot know every secret. Values are replaced with [redacted].
var (
	bearerValue  = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{6,}`)
	assignedKey  = regexp.MustCompile(`(?i)([\w.-]*(?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|auth|credential|private[_-]?key|access[_-]?key)[\w.-]*\s*[:=]\s*)("[^"]*"|'[^']*'|\S+)`)
	flagValue    = regexp.MustCompile(`(?i)(--?(?:token|secret|passw(?:or)?d|api[_-]?key|auth)\s+)(\S+)`)
	vendorSecret = regexp.MustCompile(`\b(sk|pk|ghp|gho|ghs|ghu|ghr|github_pat|xox[abprs])[-_][A-Za-z0-9_-]{8,}`)
)

// scrubText replaces the values of secret-looking words with [redacted].
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
		s = strings.TrimSpace(s[:i]) + " …" // only the first line of a command
	}
	s = scrubText(strings.Join(strings.Fields(s), " "))
	if utf8.RuneCountInString(s) > maxLabel {
		s = string([]rune(s)[:maxLabel-1]) + "…"
	}
	return s
}
