package tui

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var luaLexer = chroma.Coalesce(lexers.Get("lua"))

// luaTokenColor maps a token to the theme: keywords in the accent, comments
// dim, literals in fixed warm tones, everything else in the code gray.
func luaTokenColor(t chroma.TokenType) lipgloss.Color {
	switch {
	case t.InCategory(chroma.Comment):
		return darkGray
	case t.InCategory(chroma.Keyword), t == chroma.NameBuiltin:
		return inputBlue
	case t.InCategory(chroma.LiteralString):
		return lipgloss.Color("#d4b483")
	case t.InCategory(chroma.LiteralNumber):
		return lipgloss.Color("#f5b041")
	case t.InCategory(chroma.Operator), t.InCategory(chroma.Punctuation):
		return muted
	}
	return codeGray
}

// highlightLua lays out Lua code for the trace view: tokens colored, lines
// hard-wrapped to width (code is not word-wrapped). ok is false when the code
// cannot be tokenized; the caller then falls back to plain text.
func highlightLua(code string, width int) (lines []string, ok bool) {
	if luaLexer == nil {
		return nil, false
	}
	if strings.IndexByte(code, 0x1b) >= 0 {
		code = ansi.Strip(code)
	}
	it, err := luaLexer.Tokenise(nil, code)
	if err != nil {
		return nil, false
	}
	width = max(1, width)
	styles := map[lipgloss.Color]lipgloss.Style{}
	var line strings.Builder
	var run strings.Builder
	var runColor lipgloss.Color
	used := 0
	flushRun := func() {
		if run.Len() == 0 {
			return
		}
		st, ok := styles[runColor]
		if !ok {
			st = lipgloss.NewStyle().Foreground(runColor)
			styles[runColor] = st
		}
		line.WriteString(st.Render(run.String()))
		run.Reset()
	}
	endLine := func() {
		flushRun()
		lines = append(lines, line.String())
		line.Reset()
		used = 0
	}
	for tok := it(); tok != chroma.EOF; tok = it() {
		color := luaTokenColor(tok.Type)
		if color != runColor {
			flushRun()
			runColor = color
		}
		for _, r := range strings.ReplaceAll(tok.Value, "\t", "    ") {
			if r == '\n' {
				endLine()
				continue
			}
			w := ansi.StringWidth(string(r))
			if used+w > width && used > 0 {
				endLine()
			}
			run.WriteRune(r)
			used += w
		}
	}
	if run.Len() > 0 || line.Len() > 0 {
		endLine()
	}
	for len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, true
}
