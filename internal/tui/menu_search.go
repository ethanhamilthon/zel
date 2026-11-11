package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"zel/internal/core"
)

func searchableMenu(page menuPage) bool {
	return page == menuMain || page == menuResume || page == menuModel || page == menuEffort || page == menuTracing || page == menuProviders || page == menuFiles
}

func searchMatches(text, query string) bool {
	text = strings.ToLower(text)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

// menuIndices preserves action identities when the visible rows are filtered.
func (m model) menuInput(width int) *textarea.Model {
	mu := m.menu
	if mu.search == nil {
		input := newComposer()
		input.Placeholder = "type to search"
		input.KeyMap.InsertNewline.SetEnabled(false)
		mu.search = &input
	}
	if mu.search.Value() != mu.query {
		mu.search.SetValue(mu.query)
	}
	mu.search.SetWidth(max(1, width-2))
	mu.search.SetHeight(1)
	return mu.search
}

// filteredModels returns the models that match every word of the search text
// in the model name or the provider name.
func (m model) filteredModels() []core.ModelChoice {
	settings := m.ctl.Settings()
	choices := []core.ModelChoice{{Provider: settings.Provider, Model: settings.Model}}
	for _, c := range m.models {
		if c.Provider != settings.Provider || c.Model != settings.Model {
			choices = append(choices, c)
		}
	}
	var found []core.ModelChoice
	for _, c := range choices {
		if c.Model != "" && searchMatches(c.Model+" "+c.Provider, m.menu.query) {
			found = append(found, c)
		}
	}
	return found
}

// editText applies a typed key to a one-line text and reports whether it changed.
func editText(text *string, v tea.KeyMsg) bool {
	switch v.Type {
	case tea.KeyBackspace:
		r := []rune(*text)
		if len(r) == 0 {
			return false
		}
		*text = string(r[:len(r)-1])
	case tea.KeySpace:
		*text += " "
	case tea.KeyRunes:
		*text += strings.NewReplacer("\n", " ", "\r", " ").Replace(string(v.Runes))
	default:
		return false
	}
	return true
}

// updateMenuSearch also routes asynchronous textarea paste results to the menu,
// keeping them out of the main composer.
func (m *model) updateMenuSearch(msg tea.Msg) tea.Cmd {
	input := m.menuInput(max(1, m.menuWidth()-2))
	if key, ok := msg.(tea.KeyMsg); ok && key.Paste {
		key.Runes = []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(string(key.Runes)))
		msg = key
	}
	previous := input.Value()
	next, cmd := input.Update(msg)
	*input = next
	value := strings.NewReplacer("\n", " ", "\r", " ").Replace(input.Value())
	if value != input.Value() {
		input.SetValue(value)
	}
	m.menu.query = value
	if value != previous {
		m.menu.selected = 0
	}
	return cmd
}
