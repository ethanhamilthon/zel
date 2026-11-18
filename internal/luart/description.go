package luart

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"zel/internal/config"
)

//go:embed lua_main.md
var luaMain string

// ToolText says what the description of the lua tool covers.
type ToolText struct {
	Headless  bool // zel -p: no user, no choice()
	Nest      bool // may start zel -p subagents
	Timeout   int  // seconds per call
	Functions []config.Function
}

// ToolDescription is everything Zel tells the model about the lua tool. Lines
// marked [main], [headless] or [nest] are kept only in that case.
func ToolDescription(t ToolText) string {
	if t.Timeout <= 0 {
		t.Timeout = config.Timeout
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(luaMain), "\n") {
		keep := true
		for marker, ok := range map[string]bool{"[main]": !t.Headless, "[headless]": t.Headless, "[nest]": t.Nest} {
			if rest, found := strings.CutPrefix(line, marker); found {
				line, keep = rest, ok
			}
		}
		if keep {
			lines = append(lines, line)
		}
	}
	text := strings.ReplaceAll(strings.Join(lines, "\n"), "{timeout}", strconv.Itoa(t.Timeout))
	if len(t.Functions) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text + "\n\n## Functions of the config (globals, called with one table)\n")
	for _, f := range t.Functions {
		fmt.Fprintf(&b, "`%s{...}` - %s\n", f.Name, f.Description)
	}
	return strings.TrimSpace(b.String())
}
