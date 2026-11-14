package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Limits that bound the work of laying out one text. Anything larger is cut with
// an explicit marker: the full text is still in the session, the log or the file.
const (
	maxLayoutBytes   = 1 << 20  // text laid out on a page or in the chat
	maxMarkdownBytes = 64 << 10 // larger texts are wrapped as plain text
)

// clampText cuts text to limit bytes at a rune boundary and names what is left out.
func clampText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n… %s not shown", byteSize(len(text)-cut))
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// plainLine reports whether line renders exactly as itself when it fits in
// width: printable ASCII only, so no escape, tab, wide or combining characters.
func plainLine(line string, width int) bool {
	if len(line) > width {
		return false
	}
	for i := 0; i < len(line); i++ {
		if c := line[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// wrapText wraps a text page body to the menu width.
func (m model) wrapText(text string, w int) []string {
	return wrapLines(text, max(1, w-2))
}

// wrapLines splits text into lines and wraps each to width. Escape sequences
// are stripped; lines that need no wrapping skip the costly ANSI machinery.
func wrapLines(text string, width int) []string {
	if strings.IndexByte(text, 0x1b) >= 0 {
		text = ansi.Strip(text)
	}
	lines := make([]string, 0, strings.Count(text, "\n")+1)
	for {
		line, rest, more := strings.Cut(text, "\n")
		switch {
		case line == "":
			lines = append(lines, "")
		case plainLine(line, width):
			lines = append(lines, line)
		default:
			lines = append(lines, strings.Split(ansi.Wrap(line, width, ""), "\n")...)
		}
		if !more {
			return lines
		}
		text = rest
	}
}
