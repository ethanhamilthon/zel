package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"zel/internal/config"
)

// showTurnStats controls metadata on answer headers.
var showTurnStats = true

// accentColors are the accents offered in Settings; the first is the default.
var accentColors = []struct{ name, hex string }{
	{"Clay", "#d97757"}, {"Coral", "#ff7f6e"}, {"Rose", "#e06c95"}, {"Pink", "#f38ba8"},
	{"Magenta", "#d16ad8"}, {"Purple", "#a78bfa"}, {"Lavender", "#b4befe"}, {"Indigo", "#7c8cf8"},
	{"Blue", "#5fa8ff"}, {"Sky", "#74c7ec"}, {"Cyan", "#4fd1d9"}, {"Teal", "#80b3ad"},
	{"Mint", "#7fd1a8"}, {"Green", "#8bc34a"}, {"Lime", "#c3e88d"}, {"Yellow", "#f9d56e"},
	{"Amber", "#f5b041"}, {"Orange", "#ff9e57"}, {"Sand", "#d4b483"}, {"Silver", "#b0b8c4"},
}

// accentIndex finds an accent of the palette by hex value; -1 when it is not there
// (Settings shows it as "Custom").
func accentIndex(accent string) int {
	for i, c := range accentColors {
		if strings.EqualFold(c.hex, strings.TrimSpace(accent)) {
			return i
		}
	}
	return -1
}

// accentName is the palette name of an accent, or "Custom".
func accentName(accent string) string {
	if i := accentIndex(accent); i >= 0 {
		return accentColors[i].name
	}
	return "Custom"
}

// applyTheme sets every UI color from [colors]; the TUI has no other hex values
// except the accent palette and the literal tones of highlighted bash.
func applyTheme(c config.Colors) {
	color := func(key string) lipgloss.Color { return lipgloss.Color(c.Get(key)) }
	bgColor, accentColor, agentColor, userColor = color("bg"), color("accent"), color("agent"), color("user")
	userHighlight, grayColor, darkGray, errorColor = color("user_highlight"), color("gray"), color("dark_gray"), color("error")
	notice := color("trace.notice")
	traceColors = map[string]lipgloss.Color{
		"system": color("trace.system"), "user": color("trace.user"), "assistant": color("trace.agent"), "agent": color("trace.agent"),
		"bash": color("trace.bash"), "notice": notice, "compaction": notice, "task": notice,
		"output": color("trace.output"), "error": color("trace.error"),
	}
	a := accentColor
	markdownStyle = fmt.Sprintf(markdownTemplate, a, a, a, a, a, a, a, a)
}
