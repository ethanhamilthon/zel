package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

type point struct{ x, y int }
type selection struct {
	rows       []row
	start, end point
	dragging   bool
}

func pointLess(a, b point) bool { return a.y < b.y || a.y == b.y && a.x < b.x }
func (m *app) mouse(e *tcell.EventMouse) {
	m.dirty = true
	x, y := e.Position()
	buttons := e.Buttons()
	width, _ := m.screen.Size()
	if buttons&(tcell.WheelUp|tcell.WheelDown) != 0 {
		delta := 3
		if buttons&tcell.WheelUp != 0 {
			delta = -3
		}
		if m.menu != nil {
			if len(m.menu.list.items) > 0 {
				m.menu.list.selected += delta
				m.menu.list.clamp()
			} else {
				m.menu.offset = max(0, m.menu.offset+delta)
			}
		} else {
			m.scroll = max(0, m.scroll-delta)
		}
		m.selected = nil
		return
	}
	if buttons&tcell.Button1 != 0 {
		if m.menu != nil {
			mu := m.menu
			if y >= m.inputY && y < m.inputY+m.inputHeight {
				ed := m.activeEditor()
				ed.place(ed.rows(mu.width), x-mu.x, ed.scroll+y-m.inputY)
				return
			}
			if mu.form == nil && len(mu.list.items) > 0 && y >= mu.y && y < mu.y+mu.visible {
				i := mu.top + y - mu.y
				if i < len(mu.list.visible()) {
					mu.list.selected = i
					if it, ok := mu.list.current(); ok {
						m.activate(it)
					}
				}
			}
			return
		}
		if y >= m.inputY && y < m.inputY+m.inputHeight {
			ed := m.activeEditor()
			ed.place(ed.rows(max(1, width-2)), x-1, y-m.inputY+ed.scroll)
			m.updateCompletions()
			m.selected = nil
			return
		}
		if m.choice != nil && !m.writing && y > m.inputY {
			i := m.choiceFirst + y - m.inputY - 1
			if y <= m.inputY+m.choiceVisible && i >= 0 && i <= len(m.choice.Options) {
				m.choiceIndex = i
				m.choiceKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
			}
			return
		}
		if y < m.chatHeight {
			if e.Modifiers()&tcell.ModCtrl != 0 && m.visibleStart+y < len(m.history) {
				at := 0
				for _, c := range m.history[m.visibleStart+y] {
					if x >= at && x < at+c.width && c.link != "" {
						target := c.link
						m.work("link", func() (any, error) { return nil, openLink(target) })
						return
					}
					at += c.width
				}
			}
			if m.selected == nil || !m.selected.dragging {
				rows := make([]row, m.chatHeight)
				for i := range rows {
					for col := 0; col < width; {
						s, st, w := m.screen.Get(col, i)
						if w < 1 {
							w = 1
						}
						rows[i] = append(rows[i], span{s, w, st, ""})
						col += w
					}
				}
				m.selected = &selection{rows: rows, start: point{x, y}, end: point{x, y}, dragging: true}
			} else {
				m.selected.end = point{x, y}
			}
		}
	} else if m.selected != nil && m.selected.dragging {
		m.selected.dragging = false
		if m.selected.start == m.selected.end {
			m.selected = nil
		} else {
			m.copySelection()
		}
	}
}

func (s *selection) text() string {
	a, b := s.start, s.end
	if pointLess(b, a) {
		a, b = b, a
	}
	var out []string
	for y := max(0, a.y); y <= min(b.y, len(s.rows)-1); y++ {
		var line strings.Builder
		x := 0
		for _, c := range s.rows[y] {
			g := uniseg.NewGraphemes(c.text)
			for g.Next() {
				if (y > a.y || x+g.Width() > a.x) && (y < b.y || x <= b.x) {
					line.WriteString(g.Str())
				}
				x += g.Width()
			}
		}
		out = append(out, strings.TrimRight(line.String(), " "))
	}
	return strings.Join(out, "\n")
}
func (m *app) copySelection() {
	if m.selected == nil {
		return
	}
	text := m.selected.text()
	m.screen.SetClipboard([]byte(text))
	if copyText := m.copyText; copyText != nil {
		m.work("copied", func() (any, error) { return nil, copyText(text) })
	}
}
func (m *app) drawSelection() {
	s := m.selected
	if s == nil {
		return
	}
	width, _ := m.screen.Size()
	a, b := s.start, s.end
	if pointLess(b, a) {
		a, b = b, a
	}
	for y, r := range s.rows {
		if y >= m.chatHeight {
			break
		}
		x := 0
		for _, c := range r {
			st := c.style
			if !pointLess(point{x, y}, a) && !pointLess(b, point{x, y}) {
				st = st.Reverse(true)
			}
			if x+c.width <= width {
				m.screen.Put(x, y, c.text, st)
			}
			x += c.width
		}
	}
}
