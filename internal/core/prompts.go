package core

import (
	"context"

	"sort"
	"strings"
	"time"
	"unicode"

	"zel/internal/config"
)

// Prompt completions: a prompt registers #name; the tokens
// of a message are replaced by the text, ${{ }} expanded and wrapped in
// <prompt name="name">, when it is sent. They exist only in the main session.

// promptTimeout bounds one expansion.
const promptTimeout = 10 * time.Second

// PromptToken is a #name token; Start and End are rune offsets, [Start, End).
type PromptToken struct {
	Start, End int
	Name       string
}

func isPromptChar(r rune) bool {
	return r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// PromptTokens finds the #name tokens of text: a # at the start or after white
// space, then name characters. A bare # is not a token.
func PromptTokens(text string) []PromptToken {
	runes := []rune(text)
	var out []PromptToken
	for i := 0; i < len(runes); i++ {
		if runes[i] != '#' || (i > 0 && !unicode.IsSpace(runes[i-1])) {
			continue
		}
		end := i + 1
		for end < len(runes) && isPromptChar(runes[end]) {
			end++
		}
		if end > i+1 {
			out = append(out, PromptToken{Start: i, End: end, Name: string(runes[i+1 : end])})
			i = end - 1
		}
	}
	return out
}

// PromptAt finds the token the cursor (a rune offset) is in: query is the name
// before the cursor, and [start, end) spans the whole token.
func PromptAt(runes []rune, cursor int) (start, end int, query string, ok bool) {
	i := min(max(cursor, 0), len(runes))
	begin := i
	for begin > 0 && isPromptChar(runes[begin-1]) {
		begin--
	}
	if begin == 0 || runes[begin-1] != '#' || (begin > 1 && !unicode.IsSpace(runes[begin-2])) {
		return 0, 0, "", false
	}
	end = i
	for end < len(runes) && isPromptChar(runes[end]) {
		end++
	}
	return begin - 1, end, string(runes[begin:i]), true
}

// completions of the session: those of its modes.
func (s *session) completions() map[string]string {
	cfg := s.conf.get()
	if cfg == nil {
		return nil
	}
	out := map[string]string{}
	for name, c := range cfg.Completions {
		if config.InModes(c.Modes, s.modes()) {
			out[name] = c.Text
		}
	}
	return out
}

// PromptPreview returns unexpanded completion text without running commands.
func (c *Controller) PromptPreview(name string) (text string) {
	c.view(func() { text = c.a.session().completions()[name] })
	return strings.Join(strings.Fields(text), " ")
}

// PromptNames lists the completion names, sorted.
func (c *Controller) PromptNames() []string {
	var names []string
	c.view(func() {
		for name := range c.a.session().completions() {
			names = append(names, name)
		}
	})
	sort.Strings(names)
	return names
}

// HasPromptTokens reports whether text names a completion; only then does sending expand.
func (c *Controller) HasPromptTokens(text string) bool {
	var known map[string]string
	c.view(func() { known = c.a.session().completions() })
	for _, tok := range PromptTokens(text) {
		if _, ok := known[tok.Name]; ok {
			return true
		}
	}
	return false
}

// ExpandPrompt expands the completions of text outside the controller lock.
// ctx is the caller's: when it ends first, no text is returned. The caller
// keeps its own text on any error.
func (c *Controller) ExpandPrompt(ctx context.Context, text string) (string, error) {
	var s *session
	c.view(func() { s = c.a.session() })
	return s.expandPrompt(ctx, text)
}

// ExpandPrompt is the variant without a controller.
func (a *Runtime) ExpandPrompt(ctx context.Context, text string) (string, error) {
	return a.session().expandPrompt(ctx, text)
}

// expandPrompt replaces the known tokens in one pass: each name runs once, unknown
// names stay as typed, and the result is not expanded again.
func (s *session) expandPrompt(parent context.Context, text string) (string, error) {
	if parent.Err() != nil {
		return "", fail(CodeFailed, "the prompt expansion was cancelled")
	}
	known := s.completions()
	if len(known) == 0 {
		return text, nil
	}
	ctx, cancel := context.WithTimeout(parent, promptTimeout)
	defer cancel()
	values := map[string]string{}
	runes := []rune(text)
	var b strings.Builder
	pos := 0
	for _, tok := range PromptTokens(text) {
		def, ok := known[tok.Name]
		if !ok {
			continue
		}
		value, done := values[tok.Name]
		if !done {
			out, err := config.Expand(ctx, def, s.expander(s.Settings().Env))
			switch {
			case parent.Err() != nil:
				return "", fail(CodeFailed, "the prompt expansion was cancelled")
			case ctx.Err() != nil:
				return "", fail(CodeFailed, "#%s: timed out after %s", tok.Name, promptTimeout)
			case err != nil:
				return "", fail(CodeFailed, "#%s: %s", tok.Name, err)
			case strings.TrimSpace(out) == "":
				return "", fail(CodeFailed, "#%s: the completion is empty", tok.Name)
			}
			value = "<prompt name=\"" + tok.Name + "\">\n" + strings.TrimSuffix(out, "\n") + "\n</prompt>"
			values[tok.Name] = value
		}
		b.WriteString(string(runes[pos:tok.Start]) + value)
		pos = tok.End
	}
	b.WriteString(string(runes[pos:]))
	return b.String(), nil
}
