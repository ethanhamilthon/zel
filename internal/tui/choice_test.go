package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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
	for _, want := range []string{"Какой цвет выбрать", "Синий ❯", "Красный", "Зелёный"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "draft text") || strings.Contains(m.timeline(), composerCursorMarker) {
		t.Fatalf("the composer is still shown:\n%s", plain)
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(ansi.Strip(m.timeline()), "Зелёный ❯") {
		t.Fatalf("down does not move:\n%s", ansi.Strip(m.timeline()))
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown}) // wraps around
	if m.choiceSel != 0 {
		t.Fatalf("selection %d", m.choiceSel)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.choiceSel != 2 {
		t.Fatalf("selection %d", m.choiceSel)
	}
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
	if !strings.Contains(block, "oooooooooo ❯") || !strings.Contains(block, "10 of 12") || strings.Contains(block, "\no\n") {
		t.Fatalf("window around the selection:\n%s", block)
	}
}

func TestChoiceHasNoNumbersOrFooterAndFitsNarrowScreens(t *testing.T) {
	m := bareModel(t, "/project")
	m.height = 20
	m.choice = &core.Choice{ID: 1, Title: "q", Options: []string{"First", "Second", "Third"}, Waiting: 3}
	m.width = 60
	if block := ansi.Strip(m.choiceBlock()); block != "q\nFirst ❯\nSecond\nThird" {
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
