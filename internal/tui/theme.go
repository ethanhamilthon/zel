package tui

import (
	"fmt"
	"zel/internal/core"

	"github.com/charmbracelet/lipgloss"
)

// applyTheme sets every UI color from [colors]; the TUI has no other hex values
// except the accent palette and the literal tones of highlighted Lua.
var activeColors core.Colors

func applyTheme(c core.Colors) {
	if c == activeColors {
		return
	}
	activeColors = c
	color := func(key string) lipgloss.Color { return lipgloss.Color(c.Get(key)) }
	bgColor, accentColor, agentColor, userColor = color("bg"), color("accent"), color("agent"), color("user")
	userHighlight, grayColor, darkGray, errorColor = color("user_highlight"), color("gray"), color("dark_gray"), color("error")
	notice := color("trace.notice")
	traceColors = map[string]lipgloss.Color{
		"system": color("trace.system"), "user": color("trace.user"), "assistant": color("trace.agent"), "agent": color("trace.agent"),
		"lua": color("trace.lua"), "notice": notice, "compaction": notice, "task": notice,
		"output": color("trace.output"), "error": color("trace.error"),
	}
	a := accentColor
	markdownStyle = fmt.Sprintf(markdownTemplate, a, a, a, a, a, a, a, a)
}
