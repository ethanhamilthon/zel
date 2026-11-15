// Package docs embeds the Lua API reference, so the agent can read how zel works
// with docs() and the text always matches the binary.
package docs

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed lua-api.md examples/*.lua examples/README.md
var files embed.FS

// Topic is one piece of the reference.
type Topic struct{ Name, Summary string }

// sections maps a heading of lua-api.md to its topic. A heading without a topic
// is a bug, so the tests fail when lua-api.md gets a section that is not listed.
var sections = map[string]Topic{
	"Calls":              {"calls", "limits of a lua call, the VM, output, errors"},
	"Functions":          {"functions", "every host function: sh, files, tasks, new_agent, send_agent, choice, session"},
	"init.lua":           {"init", "init.lua format: instructions, tools, layers, reload"},
	"Hooks":              {"hooks", "session_created, message, turn_end, session_closed"},
	"Prompt completions": {"completions", "#name expansion in the user's composer"},
}

var examples = Topic{"examples", "complete init.lua files: global, project, web tools"}

// order is the order of the topics in the index.
var order = []string{"Calls", "Functions", "init.lua", "Hooks", "Prompt completions"}

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
	b.WriteString("zel docs. Call docs(topic) for one of:\n")
	for _, t := range Topics() {
		fmt.Fprintf(&b, "  docs(%q) -- %s\n", t.Name, t.Summary)
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

// Sections returns the headings of lua-api.md, for the tests.
func Sections() []string {
	src, _ := files.ReadFile("lua-api.md")
	var list []string
	for h := range parse(string(src)) {
		list = append(list, h)
	}
	return list
}

func exampleText() string {
	var b strings.Builder
	readme, _ := files.ReadFile("examples/README.md")
	b.WriteString(strings.TrimSpace(string(readme)))
	entries, _ := files.ReadDir("examples")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		data, _ := files.ReadFile("examples/" + e.Name())
		fmt.Fprintf(&b, "\n\n-- %s\n%s", e.Name(), strings.TrimSpace(string(data)))
	}
	return b.String()
}
