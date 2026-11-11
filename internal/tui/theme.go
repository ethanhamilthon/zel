package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// showTurnStats controls metadata on answer headers.
var showTurnStats = true

// Internal tokens share a single project accent; neutral and error colors stay fixed.
var themeTokens = map[string]*lipgloss.Color{
	"input_bg": &inputBackground, "select_bg": &selectBackground, "foreground": &foreground,
	"muted": &muted, "dim": &darkGray, "code": &codeGray, "accent": &inputBlue, "heading": &cyan,
	"assistant": &assistantColor, "info": &infoColor, "warn": &warnColor, "error": &errorColor,
}

var defaultTheme = func() map[string]lipgloss.Color {
	out := map[string]lipgloss.Color{}
	for name, color := range themeTokens {
		out[name] = *color
	}
	return out
}()

// applyTheme applies the project accent to every accent-bearing UI element.
func applyTheme(theme map[string]string) {
	for name, color := range themeTokens {
		*color = defaultTheme[name]
	}
	accent := strings.TrimSpace(theme["accent"])
	if accent == "" {
		accent = "#d97757"
	}
	for _, color := range []*lipgloss.Color{&inputBlue, &cyan, &assistantColor, &infoColor, &warnColor} {
		*color = lipgloss.Color(accent)
	}
	markdownStyle = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, inputBlue, inputBlue, inputBlue, inputBlue, inputBlue)
}
