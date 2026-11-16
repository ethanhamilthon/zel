package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestEditorGraphemes(t *testing.T) {
	for _, cluster := range []string{"я", "e\u0301", "👩🏽‍💻", "🇺🇳"} {
		t.Run(cluster, func(t *testing.T) {
			e := editor{}
			e.set("a" + cluster + "b")
			e.pos = len("a" + cluster)
			e.key(tcell.NewEventKey(tcell.KeyLeft, 0, 0), 20, true)
			if e.pos != 1 {
				t.Fatalf("left split a grapheme: %d", e.pos)
			}
			e.key(tcell.NewEventKey(tcell.KeyDelete, 0, 0), 20, true)
			if e.text != "ab" {
				t.Fatalf("delete: %q", e.text)
			}
			e.set("a" + cluster)
			e.key(tcell.NewEventKey(tcell.KeyBackspace2, 0, 0), 20, true)
			if e.text != "a" {
				t.Fatalf("backspace: %q", e.text)
			}
		})
	}
}
func TestEditorLayoutAndMouse(t *testing.T) {
	e := editor{}
	e.set("ab界c\nя\te\u0301")
	rows := e.rows(4)
	if len(rows) != 4 {
		t.Fatalf("rows: %+v", rows)
	}
	e.place(rows, 3, 0)
	if e.pos != len("ab界") {
		t.Fatalf("wide click: %d", e.pos)
	}
	e.place(rows, 1, 3)
	if e.pos != len(e.text) {
		t.Fatalf("combining click: %d", e.pos)
	}
	e.set("abcd")
	rows = e.rows(4)
	x, y := e.location(rows)
	if x != 0 || y != 1 {
		t.Fatalf("cursor at wrap: %d,%d", x, y)
	}
	e.set("one\ntwo")
	e.key(tcell.NewEventKey(tcell.KeyUp, 0, 0), 20, true)
	if e.pos != 3 {
		t.Fatalf("vertical navigation: %d", e.pos)
	}
	e.key(tcell.NewEventKey(tcell.KeyHome, 0, 0), 20, true)
	if e.pos != 0 {
		t.Fatal("home")
	}
}
func TestEditorAtomicCompletionsAndInput(t *testing.T) {
	e := editor{}
	e.set("#plan @\"my file.go\"")
	e.tokens = []string{"#plan", "@\"my file.go\""}
	e.key(tcell.NewEventKey(tcell.KeyBackspace2, 0, 0), 80, true)
	if e.text != "#plan " {
		t.Fatalf("atomic deletion: %q", e.text)
	}
	e.pos = 0
	e.key(tcell.NewEventKey(tcell.KeyRight, 0, 0), 80, true)
	if e.pos != len("#plan") {
		t.Fatalf("atomic right: %d", e.pos)
	}
	e = editor{}
	e.insert("русский\r\nemoji 👩🏽‍💻\x1b\x00")
	if strings.ContainsAny(e.text, "\r\x1b\x00") || !strings.Contains(e.text, "\n") {
		t.Fatalf("paste sanitation: %q", e.text)
	}
	e.key(tcell.NewEventKey(tcell.KeyCtrlJ, 0, 0), 20, true)
	if !strings.HasSuffix(e.text, "\n") {
		t.Fatal("newline")
	}
}
func TestEditorAnswerLimit(t *testing.T) {
	e := editor{limit: 4}
	e.insert("я界👩🏽‍💻")
	if e.text != "я界" {
		t.Fatalf("limit split a grapheme: %q", e.text)
	}
	e.insert("ab")
	e.insert("c")
	if e.text != "я界ab" {
		t.Fatalf("limit: %q", e.text)
	}
}

func TestEditorNarrowAndLongDraft(t *testing.T) {
	for width := 1; width < 8; width++ {
		e := editor{}
		e.set(strings.Repeat("界яe\u0301\n", 300))
		rows := e.rows(width)
		for _, r := range rows {
			if r.start > r.end || r.end > len(e.text) {
				t.Fatalf("invalid row %+v", r)
			}
		}
		for _, b := range boundaries(e.text) {
			e.pos = b
			x, y := e.location(rows)
			if x < 0 || y < 0 || y >= len(rows) {
				t.Fatal("invalid cursor")
			}
		}
	}
}
