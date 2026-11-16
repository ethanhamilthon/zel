package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// backgroundSGR is the escape sequence that sets the theme background, or ""
// when colors.bg is empty (the terminal's own background).
func backgroundSGR() string {
	if bgColor == "" {
		return ""
	}
	seq := lipgloss.ColorProfile().Color(string(bgColor)).Sequence(true)
	if seq == "" {
		return ""
	}
	return "\x1b[" + seq + "m"
}

// withBackground fills a screen row with the theme background: the row starts
// with it, every reset inside the row restores it, and spaces pad the row to
// width, so no cell keeps the terminal's background.
func withBackground(row string, width int, bg string) string {
	if bg == "" {
		return row
	}
	var b strings.Builder
	b.Grow(len(row) + len(bg)*2 + width)
	b.WriteString(bg)
	for i := 0; i < len(row); {
		start := strings.Index(row[i:], "\x1b[")
		if start < 0 {
			b.WriteString(row[i:])
			break
		}
		start += i
		b.WriteString(row[i:start])
		end := start + 2
		for end < len(row) && row[end] >= 0x30 && row[end] <= 0x3f {
			end++
		}
		for end < len(row) && row[end] >= 0x20 && row[end] <= 0x2f {
			end++
		}
		if end >= len(row) {
			b.WriteString(row[start:])
			break
		}
		b.WriteString(row[start : end+1])
		if row[end] == 'm' && clearsBackground(row[start+2:end]) {
			b.WriteString(bg)
		}
		i = end + 1
	}
	if pad := width - ansi.StringWidth(row); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteString("\x1b[m")
	return b.String()
}

// clearsBackground reports whether the SGR parameters reset the background:
// an empty list, a 0, or a 49, not counting the values of 38/48/58 colors.
func clearsBackground(params string) bool {
	if params == "" {
		return true
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if before, _, ok := strings.Cut(p, ":"); ok { // 38:2:r:g:b carries its values itself
			p = before
			if p == "38" || p == "48" || p == "58" {
				continue
			}
		}
		switch strings.TrimLeft(p, "0") {
		case "", "49":
			return true
		case "38", "48", "58":
			if i+1 < len(parts) && parts[i+1] == "5" {
				i += 2
			} else if i+1 < len(parts) && parts[i+1] == "2" {
				i += 4
			}
		}
	}
	return false
}
