package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *app) drawMenu(width, height int) {
	mu := m.menu
	w := max(1, min(64, width-4))
	x := max(0, (width-w)/2)
	y := max(0, min(3, (height-5)/2))
	mu.x = x
	mu.y = y
	mu.width = w
	m.at(x, y, mu.title, w, accentColor)
	y += 2
	if mu.form != nil {
		f := mu.form
		m.at(x, y, fmt.Sprintf("%s · %d/%d", f.fields[f.step], f.step+1, len(f.fields)), w, grayColor)
		y += 2
		m.inputHeight = max(1, min(3, height-y-3))
		m.drawEditor(&f.input, x, y, w, m.inputHeight, f.kind == "provider" && f.step == 2)
		m.inputY = y
		if f.err != "" {
			m.at(x, y+2, f.err, w, errorColor)
		}
		m.at(x, height-2, "Tab / Shift+Tab: field · Enter: next · Esc: back", w, darkGray)
	} else if len(mu.list.items) > 0 {
		m.drawEditor(&mu.search, x, y, w, 1, false)
		m.inputY = y
		m.inputHeight = 1
		y += 2
		items := mu.list.visible()
		mu.list.clamp()
		visible := max(0, height-y-3)
		first := max(0, mu.list.selected-visible+1)
		mu.top = first
		mu.visible = visible
		for i := first; i < min(len(items), first+visible); i++ {
			it := items[i]
			color := grayColor
			prefix := "  "
			if i == mu.list.selected {
				color = accentColor
				prefix = "› "
			}
			text := prefix + it.label
			if it.detail != "" {
				text += "  ·  " + it.detail
			}
			m.at(x, y+i-first, text, w, color)
		}
		mu.y = y
		hint := "↑/↓ select · Enter open · Esc back"
		if mu.loading || m.modelsLoading && mu.page == "models" {
			hint = "Loading… · Esc back"
		}
		if mu.body != "" {
			hint = firstLine(mu.body)
		}
		m.at(x, height-2, hint, w, darkGray)
	} else {

		var rows []row
		if mu.page == "entry" && mu.traceIndex < len(mu.trace) {
			t := mu.trace[mu.traceIndex]
			if t.Kind == "bash" && len(t.Text) <= 64<<10 {
				if highlighted, ok := highlightBash(t.Text, w); ok {
					rows = styledRows(strings.Join(highlighted, "\n"), w, style(traceColors["bash"]))
				}
			}
			if rows == nil && t.Kind == "assistant" && len(t.Text) <= 64<<10 {
				if text, ok := renderMarkdown(t.Text, w, false); ok {
					rows = styledRows(text, w, style(agentColor))
				}
			}
			if rows == nil {
				color := traceColors[t.Kind]
				if color == "" {
					color = grayColor
				}
				rows = plainRows(t.Text, w, color)
			}
		} else {
			rows = plainRows(mu.body, w, grayColor)
		}
		visible := max(0, height-y-2)
		mu.offset = max(0, min(mu.offset, max(0, len(rows)-visible)))
		for i, r := range rows[mu.offset:min(len(rows), mu.offset+visible)] {
			m.atRow(x, y+i, r, w)
		}
		m.at(x, height-2, "↑/↓ scroll · ←/→ trace entry · Esc back", w, darkGray)
		m.screen.HideCursor()
	}
	if m.operation {
		m.at(x, height-1, "Working…", w, accentColor)
	}
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
