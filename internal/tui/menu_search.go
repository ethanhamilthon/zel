package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"zel/internal/core"
)

func searchableMenu(page menuPage) bool {
	return page == menuMain || page == menuResume || page == menuModel || page == menuEffort || page == menuTracing || page == menuProviders
}

func menuLabelMatches(label, query string) bool {
	return strings.HasPrefix(strings.ToLower(label), strings.ToLower(query))
}

func matchWords(query string, match func(word string) bool) bool {
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if word = strings.Trim(word, "`\"'"); word != "" && !match(word) {
			return false
		}
	}
	return true
}

func searchMatches(text, query string) bool {
	text = strings.ToLower(text)
	return matchWords(query, func(w string) bool { return strings.Contains(text, w) })
}

// fuzzyMatch reports whether pattern is a subsequence of text (case-insensitive runes).
func fuzzyMatch(text, pattern string) bool {
	if pattern == "" {
		return true
	}
	pr, pi := []rune(pattern), 0
	for _, r := range text {
		if r == pr[pi] {
			if pi++; pi == len(pr) {
				return true
			}
		}
	}
	return false
}

// fuzzyMatches reports whether every word in query appears as a fuzzy subsequence in text.
func fuzzyMatches(text, query string) bool {
	text = strings.ToLower(text)
	return matchWords(query, func(w string) bool { return fuzzyMatch(text, w) })
}

func modelChoiceMatches(c core.ModelChoice, query string) bool {
	mLower, pLower := strings.ToLower(c.Model), strings.ToLower(c.Provider)
	return matchWords(query, func(w string) bool { return fuzzyMatch(mLower, w) || fuzzyMatch(pLower, w) })
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

// filteredModels returns models whose visible names or providers match the search query fuzzily.
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
		if c.Model != "" && modelChoiceMatches(c, m.menu.query) {
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
		*text += spaceReplacer.Replace(string(v.Runes))
	default:
		return false
	}
	return true
}

var spaceReplacer = strings.NewReplacer("\n", " ", "\r", " ")

// updateMenuSearch also routes asynchronous textarea paste results to the menu,
// keeping them out of the main composer.
func (m *model) updateMenuSearch(msg tea.Msg) tea.Cmd {
	input := m.menuInput(max(1, m.menuWidth()-2))
	if key, ok := msg.(tea.KeyMsg); ok && key.Paste {
		key.Runes = []rune(spaceReplacer.Replace(string(key.Runes)))
		msg = key
	}
	previous := input.Value()
	next, cmd := input.Update(msg)
	*input = next
	value := spaceReplacer.Replace(input.Value())
	if value != input.Value() {
		input.SetValue(value)
	}
	m.menu.query = value
	if value != previous {
		m.menu.selected = 0
	}
	return cmd
}
