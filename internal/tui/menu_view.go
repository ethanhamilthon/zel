package tui

import (
	"fmt"
	"strings"

	"zel/internal/core"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// drawList draws items as two columns: the labels from the left edge, then the
// details. The selected row is in the accent color and ends with ‹.
// With spread (menu pages), the details are flush with the right edge of the
// box and the arrow takes the last column. Otherwise the details follow the
// widest label and the arrow stands right after the widest row, so it never
// hangs at the screen edge.
func (m *app) drawList(x, y, width int, items []item, selected int, spread bool) {
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
	if spread && detailW > 0 {
		rowW = room // the details end at the box edge, the arrow is right of them
	}
	for i, it := range items {
		label, detail := grayColor, darkGray
		if it.header() {
			label = traceColors["user"]
		}
		if i == selected {
			label, detail = accentColor, accentColor
			m.at(x+rowW+1, y+i, "‹", 1, accentColor)
		}
		m.at(x, y+i, ansi.Truncate(it.label, labelW, "…"), labelW, label)
		if detailW > 0 {
			text := ansi.Truncate(it.detail, detailW, "…")
			at := x + labelW + 2
			if spread {
				at = x + rowW - textWidth(text)
			}
			m.at(at, y+i, text, detailW, detail)
		}
	}
}

func rowWidth(r row) (w int) {
	for _, c := range r {
		w += c.width
	}
	return w
}

// Every page is a box of half the screen, centered on it, and its content
// starts at the box's top left: the title, then a list (with its search line),
// a text or a form. On a small screen the box keeps a minimum size. The title
// and the search line stay; only the list or the text below them scrolls.
// There are no key hints.
const menuMinWidth, menuMinHeight = 60, 16

func (m *app) drawMenu(width, height int) {
	mu := m.menu
	w := min(max(menuMinWidth, width/2), max(1, width-4))
	h := min(max(menuMinHeight, height/2), max(1, height-2))
	x, y := max(0, (width-w)/2), max(0, (height-h)/2)
	mu.width = w
	note := firstLine(mu.body) // a failure of the models request, or Loading…
	if mu.loading || m.modelsLoading && mu.page == "models" {
		note = "Loading…"
	}
	m.at(x, y, mu.title, w, accentColor)
	y += 2
	room := max(1, h-2) // rows under the title
	switch {
	case mu.form != nil:
		f := mu.form
		m.at(x, y, fmt.Sprintf("%s · %d/%d", f.fields[f.step], f.step+1, len(f.fields)), w, grayColor)
		m.drawEditor(&f.input, x, y+1, w, 3, false)
		m.at(x, y+4, f.err, w, errorColor)
	case len(mu.list.items) > 0:
		m.drawEditor(&mu.search, x, y, w, 1, false)
		shown := max(1, room-3) // the search line, a gap and the note line
		items := mu.list.visible()
		mu.list.clamp()
		first := max(0, min(mu.list.selected-shown+1, len(items)-shown))
		m.drawList(x, y+2, w, items[first:min(len(items), first+shown)], mu.list.selected-first, true)
		m.at(x, y+2+shown, note, w, grayColor)
	default:
		rows := m.bodyRows(w)
		mu.offset = max(0, min(mu.offset, max(0, len(rows)-room)))
		for i, r := range rows[mu.offset:min(len(rows), mu.offset+room)] {
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
		rows := plainRows(traceHeader(t), w, darkGray)
		rows = append(rows, row{})
		rows = append(rows, entryRows(t, w)...)
		if t.Kind == "bash" {
			rows = append(rows, row{})
			rows = append(rows, plainRows("── output", w, darkGray)...)
			out, color := t.Output, traceColors["output"]
			if t.Status != "" {
				color = traceColors["error"]
			}
			if out == "" {
				out, color = "(none yet)", darkGray
				if t.Duration > 0 {
					out = "(empty)"
				}
			}
			rows = append(rows, plainRows(out, w, color)...)
		}
		return rows
	}
	return plainRows(mu.body, w, grayColor)
}

// entryRows is the text of a trace entry: a bash command highlighted, an agent
// answer as Markdown, the rest plain in the color of its kind.
func entryRows(t core.TraceEntry, w int) []row {
	if t.Kind == "bash" && len(t.Text) <= 64<<10 {
		if highlighted, ok := highlightBash(t.Text, w); ok {
			return styledRows(strings.Join(highlighted, "\n"), w, style(traceColors["bash"]))
		}
	}
	if t.Kind == "agent" && len(t.Text) <= 64<<10 {
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
