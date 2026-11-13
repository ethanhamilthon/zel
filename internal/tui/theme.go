package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
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

// accentIndex finds an accent by hex value; unknown and empty values are the default.
func accentIndex(accent string) int {
	for i, c := range accentColors {
		if strings.EqualFold(c.hex, strings.TrimSpace(accent)) {
			return i
		}
	}
	return 0
}

// applyTheme applies the accent to every accent-bearing UI element; neutral and
// error colors stay fixed.
func applyTheme(accent string) {
	if accent = strings.TrimSpace(accent); accent == "" {
		accent = accentColors[0].hex
	}
	for _, color := range []*lipgloss.Color{&inputBlue, &cyan, &assistantColor, &infoColor, &warnColor} {
		*color = lipgloss.Color(accent)
	}
	markdownStyle = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, inputBlue, inputBlue, inputBlue, inputBlue, inputBlue)
}
