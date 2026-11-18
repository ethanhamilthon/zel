// Package docs embeds the reference, so the agent can read how zel works with
// zel docs and the text always matches the binary.
package docs

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed lua-api.md examples/*.lua examples/*.json examples/lua/*.lua examples/README.md
var files embed.FS

// Topic is one piece of the reference.
type Topic struct{ Name, Summary string }

// sections maps a heading of lua-api.md to its topic. A heading without a topic
// is a bug, so the tests fail when lua-api.md gets a section that is not listed.
var sections = map[string]Topic{
	"The lua tool": {"tool", "how a lua call runs: limits, output, Tracing"},
	"API":          {"api", "the defaults module and zel CLI: sh, read, edit, choice, ..."},
	"Config":       {"config", "init.lua, zel.lua, settings, systems, functions, commands, loads"},
	"connect.json": {"connect", "providers and the selected model"},
	"Hooks":        {"hooks", "hook functions, events, return values"},
	"Prompts":      {"prompts", "#name expansion in the user's composer, show_prompt"},
	"Colors":       {"colors", "colors: the TUI theme, key by key"},
}

var examples = Topic{"examples", "init.lua, zel.lua, connect.json and a loaded module"}

// order is the order of the topics in the index.
var order = []string{"The lua tool", "API", "Config", "connect.json", "Hooks", "Prompts", "Colors"}

// parse splits lua-api.md into its "## " sections; the text before the first one
// is the intro and has no topic.
func parse(src string) map[string]string {
	out := map[string]string{}
	var name string
	var buf []string
	fenced := false
	flush := func() {
		if name != "" {
			out[name] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if h, ok := strings.CutPrefix(line, "## "); ok && !fenced {
			flush()
			name, buf = strings.TrimSpace(h), nil
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

// Topics lists the topics in the order of the index.
func Topics() []Topic {
	var list []Topic
	for _, h := range order {
		list = append(list, sections[h])
	}
	return append(list, examples)
}

// Index is what docs() returns without a topic.
func Index() string {
	var b strings.Builder
	b.WriteString("zel docs. Run zel docs TOPIC for one of:\n")
	for _, t := range Topics() {
		fmt.Fprintf(&b, "  zel docs %s  -- %s\n", t.Name, t.Summary)
	}
	return b.String()
}

// Get returns the text of a topic, or false for an unknown name.
func Get(name string) (string, bool) {
	if name == examples.Name {
		return exampleText(), true
	}
	src, err := files.ReadFile("lua-api.md")
	if err != nil {
		return "", false
	}
	parsed := parse(string(src))
	for h, t := range sections {
		if t.Name == name {
			return parsed[h], true
		}
	}
	return "", false
}

func exampleText() string {
	var b strings.Builder
	readme, _ := files.ReadFile("examples/README.md")
	b.WriteString(strings.TrimSpace(string(readme)))
	names := []string{"init.lua", "zel.lua", "connect.json", "lua/web.lua"}
	for _, name := range names {
		data, _ := files.ReadFile("examples/" + name)
		fmt.Fprintf(&b, "\n\n# --- %s\n%s", name, strings.TrimSpace(string(data)))
	}
	return b.String()
}
