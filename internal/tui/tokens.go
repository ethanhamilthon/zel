package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"zel/internal/core"
)

// tokenSpan is a completed token of the composer as a rune range [start, end).
type tokenSpan struct{ start, end int }

// rememberToken marks text (a #name or an @path the user picked from a list) as
// complete: wherever it stands as a whole word, the composer shows it in the
// accent color and edits it as one unit.
func (m *model) rememberToken(text string) {
	if m.completedTokens == nil {
		m.completedTokens = map[string]bool{}
	}
	m.completedTokens[text] = true
}

// tokenSpans finds the completed tokens of value, in order and without overlaps.
func (m *model) tokenSpans(value string) []tokenSpan {
	if len(m.completedTokens) == 0 || !strings.ContainsAny(value, "#@") {
		return nil
	}
	if m.spansValid && m.spansValue == value {
		return m.spans
	}
	runes := []rune(value)
	var spans []tokenSpan
	for _, tok := range core.PromptTokens(value) {
		if m.completedTokens["#"+tok.Name] {
			spans = append(spans, tokenSpan{tok.Start, tok.End})
		}
	}
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' || (i > 0 && !unicode.IsSpace(runes[i-1])) {
			continue
		}
		best := 0
		for text := range m.completedTokens {
			t := []rune(text)
			n := len(t)
			if t[0] != '@' || n <= best || i+n > len(runes) || string(runes[i:i+n]) != text {
				continue
			}
			if i+n == len(runes) || unicode.IsSpace(runes[i+n]) {
				best = n
			}
		}
		if best > 0 {
			spans = append(spans, tokenSpan{i, i + best})
			i += best - 1
		}
	}
	sortSpans(spans)
	m.spans, m.spansValue, m.spansValid = spans, value, true
	return spans
}

func sortSpans(spans []tokenSpan) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

// spanAround is the token that offset lies strictly inside.
func spanAround(spans []tokenSpan, offset int) (tokenSpan, bool) {
	for _, s := range spans {
		if s.start < offset && offset < s.end {
			return s, true
		}
	}
	return tokenSpan{}, false
}

// tokenKey deletes a whole completed token with Backspace right after it or
// Delete right before it; it reports false for any other key.
func (m *model) tokenKey(v tea.KeyMsg) (tea.Cmd, bool) {
	key := v.String()
	if key != "backspace" && key != "ctrl+h" && key != "delete" {
		return nil, false
	}
	value := m.input.Value()
	cursor := m.composerCursorOffset()
	for _, s := range m.tokenSpans(value) {
		if (key == "delete" && cursor == s.start) || (key != "delete" && cursor == s.end) {
			runes := []rune(value)
			m.input.SetValue(string(runes[:s.start]) + string(runes[s.end:]))
			return m.setCursorOffset(s.start), true
		}
	}
	return nil, false
}

// snapCursor keeps the cursor out of completed tokens: moving left it stops
// before a token, otherwise after it.
func (m *model) snapCursor(before int) {
	cursor := m.composerCursorOffset()
	s, ok := spanAround(m.tokenSpans(m.input.Value()), cursor)
	if !ok {
		return
	}
	target := s.end
	if cursor < before {
		target = s.start
	}
	moveCursor(&m.input, target, len(m.composerRows())+1)
	updateTextarea(&m.input, composerNoop{}, m.height-2)
}

// tokenColor paints the completed tokens of the visible composer rows lines in
// the accent color, keeping the cursor marker and the rest of the styling.
func (m *model) tokenColor(lines []string) []string {
	spans := m.tokenSpans(m.input.Value())
	if len(spans) == 0 {
		return lines
	}
	rows := m.composerRows()
	runes := []rune(m.input.Value())
	first := m.composerScroll(rows)
	accent := sgrPrefix(lipgloss.NewStyle().Foreground(accentColor))
	if accent == "" {
		return lines
	}
	for i := range lines {
		if first+i >= len(rows) {
			break
		}
		row := rows[first+i]
		var cells [][2]int
		for _, sp := range spans {
			a, b := max(sp.start, row.start), min(sp.end, row.end)
			if a < b {
				left := ansi.StringWidth(string(runes[row.start:a]))
				cells = append(cells, [2]int{left, left + ansi.StringWidth(string(runes[a:b]))})
			}
		}
		if len(cells) > 0 {
			lines[i] = paintCells(lines[i], cells, accent)
		}
	}
	return lines
}

// paintCells recolors the cell ranges of a styled line. Escape sequences stay
// where they are, once; after a range the line's own style is restored.
func paintCells(line string, cells [][2]int, color string) string {
	var b strings.Builder
	active, col, k, inside := "", 0, 0, false
	for len(line) > 0 {
		for k < len(cells) && col >= cells[k][1] {
			k++
		}
		want := k < len(cells) && col >= cells[k][0]
		if want != inside {
			if want {
				b.WriteString(color)
			} else {
				b.WriteString("\x1b[0m" + active)
			}
			inside = want
		}
		if line[0] == 0x1b {
			n := escapeLen(line)
			seq := line[:n]
			b.WriteString(seq)
			if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
				if seq == "\x1b[0m" || seq == "\x1b[m" {
					active = ""
				} else {
					active += seq
				}
				if inside {
					b.WriteString(color)
				}
			}
			line = line[n:]
			continue
		}
		r, n := utf8.DecodeRuneInString(line)
		b.WriteString(line[:n])
		col += runeWidth(r)
		line = line[n:]
	}
	if inside {
		b.WriteString("\x1b[0m" + active)
	}
	return b.String()
}

// escapeLen is the length of the escape sequence line starts with: CSI up to
// its final byte, OSC up to BEL or ST, otherwise the two-byte escape.
func escapeLen(line string) int {
	if len(line) < 2 {
		return len(line)
	}
	switch line[1] {
	case '[':
		for i := 2; i < len(line); i++ {
			if line[i] >= 0x40 && line[i] <= 0x7e {
				return i + 1
			}
		}
	case ']':
		for i := 2; i < len(line); i++ {
			if line[i] == 0x07 {
				return i + 1
			}
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '\\' {
				return i + 2
			}
		}
	default:
		return 2
	}
	return len(line)
}

// sgrPrefix is the escape sequence style opens text with; empty without colors.
func sgrPrefix(style lipgloss.Style) string {
	out := style.Render("x")
	if i := strings.Index(out, "x"); i > 0 {
		return out[:i]
	}
	return ""
}
