package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"zel/internal/core"
)

// The agent's question replaces the composer; the keys pick an option.
func TestChoiceReplacesComposerAndTakesKeys(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 60, 20
	m.input.SetValue("draft text")
	m.choice = &core.Choice{ID: 1, Title: "Какой цвет выбрать", Options: []string{"Синий", "Красный", "Зелёный"}}
	m.refresh(true)
	plain := ansi.Strip(m.timeline())
	for _, want := range []string{"Какой цвет выбрать", "Синий <", "Красный", "Зелёный"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "draft text") || strings.Contains(m.timeline(), composerCursorMarker) {
		t.Fatalf("the composer is still shown:\n%s", plain)
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(ansi.Strip(m.timeline()), "Зелёный <") {
		t.Fatalf("down does not move:\n%s", ansi.Strip(m.timeline()))
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown}) // custom answer
	if m.choiceSel != 3 {
		t.Fatal("missing custom answer")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown}) // wraps around
	if m.choiceSel != 0 {
		t.Fatalf("selection %d", m.choiceSel)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.choiceSel != 3 {
		t.Fatalf("selection %d", m.choiceSel)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp}) // back to the last preset
	// Typing does not reach the composer while a question is open.
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.input.Value() != "draft text" || m.choice == nil {
		t.Fatalf("composer %q, choice %v", m.input.Value(), m.choice)
	}
	// Enter answers and brings the composer back, draft included.
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.choice != nil || !strings.Contains(ansi.Strip(m.timeline()), "draft text") {
		t.Fatalf("answered question stays:\n%s", ansi.Strip(m.timeline()))
	}
}

func TestChoiceCustomAnswerPreservesDraftAndQueue(t *testing.T) {
	m, a := freshModel(t)
	m.menu = nil
	a.OnChoiceChanged(func() {})
	m.input.SetValue("unfinished draft")

	m.choice = &core.Choice{ID: 1, Title: "Question", Options: []string{"Preset"}}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.choice == nil {
		t.Fatal("empty custom answer submitted")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Свой ответ 123 kj")})
	if m.choiceInput.Value() != "Свой ответ 123 kj" || m.input.Value() != "unfinished draft" {
		t.Fatal("custom input corrupted")
	}
	if !strings.Contains(m.choiceBlock(), composerCursorMarker) {
		t.Fatal("missing custom caret")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.choice == nil || m.choiceWriting {
		t.Fatal("Esc should return to options")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.choice != nil || m.input.Value() != "unfinished draft" {
		t.Fatal("answer lost composer draft")
	}
}

func TestChoiceDigitAndEscAnswer(t *testing.T) {
	for name, k := range map[string]tea.KeyMsg{
		"digit": {Type: tea.KeyRunes, Runes: []rune("2")},
		"esc":   {Type: tea.KeyEsc},
	} {
		m := bareModel(t, "/project")
		m.choice = &core.Choice{ID: 1, Title: "q", Options: []string{"a", "b"}}
		m = key(m, k)
		if m.choice != nil {
			t.Fatalf("%s did not answer", name)
		}
	}
	m := bareModel(t, "/project")
	m.choice = &core.Choice{ID: 1, Title: "q", Options: []string{"a", "b"}}
	if m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")}); m.choice == nil {
		t.Fatal("a digit past the options answered")
	}
}

func TestChoiceListScrollsInASmallTerminal(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 40, 9
	var options []string
	for i := 1; i <= 12; i++ {
		options = append(options, strings.Repeat("o", i))
	}
	m.choice = &core.Choice{ID: 1, Title: "q", Options: options}
	m.choiceSel = 9
	block := ansi.Strip(m.choiceBlock())
	if !strings.Contains(block, "oooooooooo <") || !strings.Contains(block, "10 of 13") || strings.Contains(block, "\no\n") {
		t.Fatalf("window around the selection:\n%s", block)
	}
}

func TestChoiceHasNoNumbersOrFooterAndFitsNarrowScreens(t *testing.T) {
	m := bareModel(t, "/project")
	m.height = 20
	m.choice = &core.Choice{ID: 1, Title: "q", Options: []string{"First", "Second", "Third"}, Waiting: 3}
	m.width = 60
	if block := ansi.Strip(m.choiceBlock()); block != "q\nFirst <\nSecond\nThird\nWrite your own answer…" {
		t.Fatalf("unexpected choice layout:\n%s", block)
	}
	for width := 1; width <= 15; width++ {
		m.width = width
		for _, row := range strings.Split(m.choiceBlock(), "\n") {
			if ansi.StringWidth(row) > width {
				t.Fatalf("choice exceeds width %d: %q", width, row)
			}
		}
	}
}

// An answer stays in the chat as "question: answer"; the custom answer edits
// like the composer, without key hints, and takes mouse clicks.
func TestChoiceAnswerRecordAndCustomEditor(t *testing.T) {
	m, a := freshModel(t)
	m.menu = nil
	m.width, m.height = 30, 20
	a.OnChoiceChanged(func() {})
	if _, err := a.Ask("Color: pick", []string{"Blue"}); err != nil {
		t.Fatal(err)
	}
	m.syncChoice()
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.wantsMouse() {
		t.Fatal("custom answer does not take clicks")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})
	block := ansi.Strip(m.choiceBlock())
	if block != "Color: pick\none"+strings.Repeat(" ", 27)+"\ntwo"+strings.Repeat(" ", 27) || strings.Contains(block, "Esc") {
		t.Fatalf("custom editor:\n%q", block)
	}
	// A click on the first line puts the cursor after "on".
	m.refresh(true)
	y := m.inputTop + 1 - m.viewport.YOffset
	next, _ := m.Update(tea.MouseMsg{X: 2, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = next.(model)
	if m.choiceInput.Line() != 0 || m.choiceInput.LineInfo().ColumnOffset != 2 {
		t.Fatalf("click put the cursor at line %d col %d", m.choiceInput.Line(), m.choiceInput.LineInfo().ColumnOffset)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = settle(next.(model))
	if m.choice != nil || m.wantsMouse() || cmd == nil {
		t.Fatal("answer did not close the editor and release the mouse")
	}
	var last entry
	for _, e := range m.entries {
		if e.Kind == "choice" {
			last = e
		}
	}
	if last.Kind != "choice" || last.Text != "Color: pick\x1fone\ntwo" {
		t.Fatalf("record %+v", last)
	}
	plain := ansi.Strip(renderEntry(last, 30, -1))
	if plain != "Color: pick: one\ntwo" {
		t.Fatalf("record view %q", plain)
	}
}

func TestChoiceRecordColors(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	out := renderChoiceRecord("Question here\x1fan answer that wraps", 12)
	accent, user := sgrPrefix(lipgloss.NewStyle().Foreground(accentColor)), sgrPrefix(lipgloss.NewStyle().Foreground(userColor))
	if ansi.Strip(out) != "Question\nhere: an\nanswer that\nwraps" {
		t.Fatalf("layout %q", ansi.Strip(out))
	}
	rows := strings.Split(out, "\n")
	if !strings.HasPrefix(rows[0], accent) || !strings.Contains(rows[1], user+"an") || !strings.HasPrefix(rows[1], accent+"here:") || strings.Contains(rows[3], accent) {
		t.Fatalf("colors %q", out)
	}
}
