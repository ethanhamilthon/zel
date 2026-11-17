package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

// editor is shared by the composer, custom answers, search and settings forms.
// Positions are UTF-8 byte offsets, always on grapheme boundaries.
type editor struct {
	text        string
	pos, scroll int
	tokens      []string
	limit       int
}

type editRow struct{ start, end int }

func (e *editor) set(s string) { e.text = ""; e.pos = 0; e.scroll = 0; e.tokens = nil; e.insert(s) }
func cleanInput(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}
func (e *editor) insert(s string) {
	s = cleanInput(s)
	if e.limit > 0 {
		remaining := max(0, e.limit-utf8.RuneCountInString(e.text))
		end := 0
		g := uniseg.NewGraphemes(s)
		for g.Next() {
			n := utf8.RuneCountInString(g.Str())
			if n > remaining {
				break
			}
			remaining -= n
			_, end = g.Positions()
		}
		s = s[:end]
	}
	e.text = e.text[:e.pos] + s + e.text[e.pos:]
	e.pos += len(s)
}
func boundaries(s string) []int {
	b := []int{0}
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		_, end := g.Positions()
		b = append(b, end)
	}
	return b
}
func (e *editor) step(direction int) int {
	next := 0
	if direction > 0 {
		next = len(e.text)
	}
	for _, b := range boundaries(e.text) {
		if direction < 0 && b < e.pos {
			next = b
		}
		if direction > 0 && b > e.pos {
			next = b
			break
		}
	}
	for _, token := range e.tokens {
		for start := 0; start < len(e.text); {
			i := strings.Index(e.text[start:], token)
			if i < 0 {
				break
			}
			i += start
			end := i + len(token)
			if (i == 0 || unicode.IsSpace(runeBefore(e.text, i))) && (end == len(e.text) || unicode.IsSpace(runeAt(e.text, end))) {
				if direction < 0 && i < e.pos && e.pos <= end {
					next = i
				}
				if direction > 0 && i <= e.pos && e.pos < end {
					next = end
				}
			}
			start = end
		}
	}
	return next
}
func runeBefore(s string, i int) rune { r, _ := utf8.DecodeLastRuneInString(s[:i]); return r }
func runeAt(s string, i int) rune     { r, _ := utf8.DecodeRuneInString(s[i:]); return r }
func (e *editor) rows(width int) []editRow {
	width = max(1, width)
	rows := []editRow{}
	start, x := 0, 0
	g := uniseg.NewGraphemes(e.text)
	for g.Next() {
		a, b := g.Positions()
		ch := g.Str()
		w := g.Width()
		if ch == "\t" {
			w = 4 - x%4
		}
		if ch == "\n" {
			rows = append(rows, editRow{start, a})

			start = b
			x = 0
			continue
		}
		if x+w > width && x > 0 {
			rows = append(rows, editRow{start, a})
			start = a
			x = 0
		}
		x += w
	}
	rows = append(rows, editRow{start, len(e.text)})
	if x >= width {
		rows = append(rows, editRow{len(e.text), len(e.text)})
	}
	return rows
}
func (e *editor) location(rows []editRow) (int, int) {
	row := 0
	for i, r := range rows {
		if r.start <= e.pos {
			row = i
		}
	}
	return textWidth(e.text[rows[row].start:e.pos]), row
}
func textWidth(s string) int {
	x := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		if g.Str() == "\t" {
			x += 4 - x%4
		} else {
			x += g.Width()
		}
	}
	return x
}
func (e *editor) place(rows []editRow, x, y int) {
	r := rows[max(0, min(y, len(rows)-1))]
	e.pos = r.start
	at := 0
	g := uniseg.NewGraphemes(e.text[r.start:r.end])
	for g.Next() {
		a, b := g.Positions()
		w := g.Width()
		if g.Str() == "\t" {
			w = 4 - at%4
		}
		if x < at+(w+1)/2 {
			e.pos = r.start + a
			break
		}
		at += w
		e.pos = r.start + b
	}
	// Clicks on completed tokens land on an edge, just like arrow navigation.
	for _, token := range e.tokens {
		for start := 0; start < len(e.text); {
			i := strings.Index(e.text[start:], token)
			if i < 0 {
				break
			}
			i += start
			end := i + len(token)
			if i < e.pos && e.pos < end {
				if e.pos-i < end-e.pos {
					e.pos = i
				} else {
					e.pos = end
				}
				return
			}
			start = end
		}
	}
}

// key edits text only. Submission and navigation between UI elements belong to callers.
func (e *editor) key(k *tcell.EventKey, width int, multiline bool) bool {
	before, pos := e.text, e.pos
	switch k.Key() {
	case tcell.KeyRune:
		if k.Modifiers()&(tcell.ModCtrl|tcell.ModAlt) == 0 {
			e.insert(string(k.Rune()))
		}
	case tcell.KeyLeft:
		e.pos = e.step(-1)
	case tcell.KeyRight:
		e.pos = e.step(1)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		n := e.step(-1)
		e.text = e.text[:n] + e.text[e.pos:]
		e.pos = n
	case tcell.KeyDelete:
		n := e.step(1)
		e.text = e.text[:e.pos] + e.text[n:]
	case tcell.KeyHome, tcell.KeyCtrlA:
		e.pos = strings.LastIndex(e.text[:e.pos], "\n") + 1
	case tcell.KeyEnd, tcell.KeyCtrlE:
		if n := strings.IndexByte(e.text[e.pos:], '\n'); n >= 0 {
			e.pos += n
		} else {
			e.pos = len(e.text)
		}
	case tcell.KeyUp, tcell.KeyDown:
		rows := e.rows(width)
		x, y := e.location(rows)
		if k.Key() == tcell.KeyUp {
			y--
		} else {
			y++
		}
		e.place(rows, x, y)
	case tcell.KeyCtrlU:
		e.text = e.text[e.pos:]
		e.pos = 0
	case tcell.KeyCtrlW:
		end := e.pos
		for e.pos > 0 && unicode.IsSpace(runeBefore(e.text, e.pos)) {
			e.pos = e.step(-1)
		}
		for e.pos > 0 && !unicode.IsSpace(runeBefore(e.text, e.pos)) {
			e.pos = e.step(-1)
		}
		e.text = e.text[:e.pos] + e.text[end:]
	case tcell.KeyEnter, tcell.KeyCtrlJ:
		if multiline {
			e.insert("\n")
		}
	case tcell.KeyTab:
		if multiline {
			e.insert("\t")
		}
	}
	return before != e.text || pos != e.pos
}
