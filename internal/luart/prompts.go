package luart

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	lua "github.com/yuin/gopher-lua"
)

// Prompt completions: zel.complete(name, fn) registers #name; a message's tokens
// are replaced by what fn returns when the message is sent.

// completion is one registered expansion; Layer names the init.lua that registered it.
type completion struct {
	Name, Layer string
	fn          *lua.LFunction
}

var promptName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// PromptToken is a #name token; Start and End are rune offsets, [Start, End).
type PromptToken struct {
	Start, End int
	Name       string
}

func isPromptChar(r rune) bool {
	return r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// PromptTokens finds the #name tokens of text: a # at the start or after white
// space, then name characters. A bare # is not a token; names are not checked here.
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

// PromptNames lists the names of the snapshot, sorted; the registry is final once LoadConfig returns.
func (c *Config) PromptNames() []string {
	names := make([]string, 0, len(c.completions))
	for name := range c.completions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HasPrompt reports whether name is registered in the snapshot.
func (c *Config) HasPrompt(name string) bool {
	_, ok := c.completions[name]
	return ok
}

// Expand replaces the registered tokens in one pass: each name runs once per call,
// unknown names stay as typed, and the result is not expanded again. An error
// names the failing token; the caller keeps the original text.
func (c *Config) Expand(ctx context.Context, text string) (string, error) {
	values := map[string]string{}
	runes := []rune(text)
	var b strings.Builder
	pos := 0
	for _, tok := range PromptTokens(text) {
		def, ok := c.completions[tok.Name]
		if !ok {
			continue
		}
		value, done := values[tok.Name]
		if !done {
			out, err := c.runCompletion(ctx, def)
			if err != nil {
				return "", fmt.Errorf("#%s: %s", tok.Name, errorMessage(err))
			}
			value, values[tok.Name] = out, out
		}
		b.WriteString(string(runes[pos:tok.Start]) + value)
		pos = tok.End
	}
	b.WriteString(string(runes[pos:]))
	return b.String(), nil
}

// runCompletion calls one completion under the vm lock and requires non-empty text.
func (c *Config) runCompletion(ctx context.Context, def completion) (string, error) {
	var out struct{ text string }
	err := c.run(ctx, func(ctx context.Context) error {
		L := c.v.L
		prev := c.v.ctx
		c.v.ctx = ctx
		L.SetContext(ctx)
		defer func() { c.v.ctx = prev; L.RemoveContext() }()
		if err := L.CallByParam(lua.P{Fn: def.fn, NRet: 1, Protect: true}); err != nil {
			return err
		}
		res := L.Get(-1)
		L.Pop(1)
		text, ok := res.(lua.LString)
		if !ok || strings.TrimSpace(string(text)) == "" {
			return errors.New("must return the instructions as non-empty text")
		}
		out.text = string(text)
		return nil
	})
	if err != nil {
		return "", err
	}
	return out.text, nil
}
