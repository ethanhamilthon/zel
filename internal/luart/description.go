package luart

import (
	_ "embed"
	"fmt"
	"strings"

	"zel/internal/config"
)

//go:embed lua_main.md
var luaMain string

//go:embed lua_headless.md
var luaHeadless string

const mainChoice = "- `choice(question, {options...}) -> id`: ask the user; the answer arrives as a notification.\n"

func ToolDescription(headless bool, timeout int, functions []config.Function) string {
	if timeout <= 0 {
		timeout = 30
	}
	text := fmt.Sprintf(luaMain, timeout)
	if headless {
		text += luaHeadless
	} else {
		text += mainChoice
	}
	if len(functions) == 0 {
		return strings.TrimSpace(text)
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\nConfigured functions (pass one table):\n")
	for _, f := range functions {
		fmt.Fprintf(&b, "- %s{...}: %s\n", f.Name, f.Description)
	}
	return strings.TrimSpace(b.String())
}
