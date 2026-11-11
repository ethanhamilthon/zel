package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// showTurnStats controls metadata on answer headers.
var showTurnStats = true

// themeTokens maps [ui.theme] keys to the colors they set.
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

// applyTheme sets the colors from [ui.theme]; unset tokens keep their defaults.
func applyTheme(theme map[string]string) {
	for name, color := range themeTokens {
		*color = defaultTheme[name]
		if value := strings.TrimSpace(theme[name]); value != "" {
			*color = lipgloss.Color(value)
		}
	}
	markdownStyle = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, muted, infoColor, errorColor, inputBlue, inputBlue)
}
