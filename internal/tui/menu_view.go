package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// listWidth is the width of the widest row of a list: label, gap, detail.
func listWidth(items []item) int {
	label, detail := 0, 0
	for _, it := range items {
		label, detail = max(label, textWidth(it.label)), max(detail, textWidth(it.detail))
	}
	if detail > 0 {
		return label + 2 + detail
	}
	return label
}

// drawList draws items as two columns: the labels, then the details aligned in
// a second column. The selected row is in the accent color and ends with ‹.
// The arrow stands right after the widest row, so it never hangs at the screen edge.
func (m *app) drawList(x, y, width int, items []item, selected int) {
	labelW, detailW := 0, 0
	for _, it := range items {
		labelW, detailW = max(labelW, textWidth(it.label)), max(detailW, textWidth(it.detail))
	}
	room := max(1, width-2) // keeps a column for the arrow
	if detailW > 0 {
		detailW = min(detailW, max(1, room/2))
		labelW = min(labelW, max(1, room-detailW-2))
	} else {
		labelW = min(labelW, room)
	}
	rowW := labelW
	if detailW > 0 {
		rowW += 2 + detailW
	}
	for i, it := range items {
		label, detail := grayColor, darkGray
		if i == selected {
			label, detail = accentColor, accentColor
			m.at(x+rowW+1, y+i, "‹", 1, accentColor)
		}
		m.at(x, y+i, ansi.Truncate(it.label, labelW, "…"), labelW, label)
		if detailW > 0 {
			m.at(x+labelW+2, y+i, ansi.Truncate(it.detail, detailW, "…"), detailW, detail)
		}
	}
}

func rowWidth(r row) (w int) {
	for _, c := range r {
		w += c.width
	}
	return w
}

// drawMenu centers the page on the screen, both ways: the title, then a list,
// a text or a form. There are no key hints.
func (m *app) drawMenu(width, height int) {
	mu := m.menu
	maxW := max(1, width-4)
	var w, h int // the size of the block
	var rows []row
	var shown int
	note := firstLine(mu.body) // a failure of the models request, or Loading…
	if mu.loading || m.modelsLoading && mu.page == "models" {
		note = "Loading…"
	}
	switch {
	case mu.form != nil:
		w, h = min(64, maxW), 2+2+3+1
	case len(mu.list.items) > 0:
		w = max(max(textWidth(mu.title), listWidth(mu.list.items)+2), 20)
		w = min(w, min(64, maxW))
		shown = min(len(mu.list.items), max(1, height-7))
		h = 4 + shown
		if note != "" {
			h++
		}
	default:
		rows = m.bodyRows(min(76, maxW))
		shown = min(len(rows), max(1, height-4))
		w = textWidth(mu.title)
		for _, r := range rows {
			w = max(w, rowWidth(r))
		}
		w = min(w, maxW)
		h = 2 + shown
	}
	h = min(h, height)
	x, y := max(0, (width-w)/2), max(0, (height-h)/2)
	mu.width = w
	m.at(x, y, mu.title, w, accentColor)
	y += 2
	switch {
	case mu.form != nil:
		f := mu.form
		m.at(x, y, fmt.Sprintf("%s · %d/%d", f.fields[f.step], f.step+1, len(f.fields)), w, grayColor)
		m.drawEditor(&f.input, x, y+1, w, 3, false)
		m.at(x, y+4, f.err, w, errorColor)
	case len(mu.list.items) > 0:
		m.drawEditor(&mu.search, x, y, w, 1, false)
		items := mu.list.visible()
		mu.list.clamp()
		first := max(0, min(mu.list.selected-shown+1, len(items)-shown))
		m.drawList(x, y+2, w, items[first:min(len(items), first+shown)], mu.list.selected-first)
		m.at(x, y+2+shown, note, w, grayColor)
	default:
		mu.offset = max(0, min(mu.offset, max(0, len(rows)-shown)))
		for i, r := range rows[mu.offset:min(len(rows), mu.offset+shown)] {
			m.atRow(x, y+i, r, w)
		}
		m.screen.HideCursor()
	}
	if m.operation {
		m.at(0, height-1, "Working…", width, accentColor)
	}
}

// bodyRows is the text of a page without a list: Info, Help or a trace entry.
func (m *app) bodyRows(w int) []row {
	mu := m.menu
	if mu.page == "entry" && mu.traceIndex < len(mu.trace) {
		t := mu.trace[mu.traceIndex]
		if t.Kind == "bash" && len(t.Text) <= 64<<10 {
			if highlighted, ok := highlightBash(t.Text, w); ok {
				return styledRows(strings.Join(highlighted, "\n"), w, style(traceColors["bash"]))
			}
		}
		if t.Kind == "assistant" && len(t.Text) <= 64<<10 {
			if text, ok := renderMarkdown(t.Text, w, false); ok {
				return styledRows(text, w, style(agentColor))
			}
		}
		color := traceColors[t.Kind]
		if color == "" {
			color = grayColor
		}
		return plainRows(t.Text, w, color)
	}
	return plainRows(mu.body, w, grayColor)
}
func (m *app) at(x, y int, text string, width int, color lipgloss.Color) {
	rows := plainRows(text, width, color)
	if len(rows) > 0 {
		m.atRow(x, y, rows[0], width)
	}
}
func (m *app) atRow(x, y int, r row, width int) {
	at := 0
	for _, c := range r {
		if at+c.width > width {
			break
		}
		putSpan(m.screen, x+at, y, c)
		at += c.width
	}
}
