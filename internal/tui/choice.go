package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// choiceChanged tells the UI that the agent's question appeared or went away.
type choiceChanged struct{}

// syncChoice reads the pending question; a new one starts on its first option.
func (m *model) syncChoice() {
	was := m.choice
	m.choice = m.ctl.PendingChoice()
	if m.choice == nil || was == nil || was.ID != m.choice.ID {
		m.choiceSel, m.choiceWriting = 0, false
		m.choiceInput = newComposer()
		m.choiceInput.CharLimit = 4096
	}
	m.refresh(true)
}

// choiceKey handles a key while the agent's question replaces the composer. Ctrl+D
// (stop the turn) and Ctrl+Q (quit) keep working; other keys do nothing.
func (m model) choiceKey(v tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	n := len(m.choice.Options) + 1
	answer := func(index int) (tea.Model, tea.Cmd, bool) {
		m.ctl.AnswerChoice(m.choice.ID, index)
		m.choice = nil
		m.refresh(true)
		return m, nil, true
	}
	if m.choiceWriting && v.String() != "ctrl+d" && v.String() != "ctrl+q" {
		switch v.String() {
		case "esc":
			m.choiceWriting = false
		case "enter":
			text := strings.TrimSpace(m.choiceInput.Value())
			if text != "" {
				m.ctl.AnswerChoiceText(m.choice.ID, text)
				m.choice, m.choiceWriting = nil, false
			}
		default:
			var cmd tea.Cmd
			m.choiceInput.SetWidth(max(1, m.width-2))
			m.choiceInput, cmd = m.choiceInput.Update(v)
			m.refresh(true)
			return m, cmd, true
		}
		m.refresh(true)
		return m, nil, true
	}
	switch key := v.String(); key {
	case "ctrl+d":
		m.requestCancel()
		return m, nil, true
	case "ctrl+q":
		return m, nil, false
	case "up", "k", "shift+tab":
		m.choiceSel = (m.choiceSel + n - 1) % n
	case "down", "j", "tab":
		m.choiceSel = (m.choiceSel + 1) % n
	case "enter":
		if m.choiceSel == n-1 {
			m.choiceWriting = true
			if m.choiceInput.CharLimit == 0 {
				m.choiceInput = newComposer()
				m.choiceInput.CharLimit = 4096
			}
			m.choiceInput.SetWidth(max(1, m.width-2))
			m.choiceInput.Placeholder = "Your answer"
			break
		}
		return answer(m.choiceSel)
	case "esc":
		return answer(-1)
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' && int(key[0]-'0') < n {
			return answer(int(key[0] - '1'))
		}
		return m, nil, true
	}
	m.refresh(true)
	return m, nil, true
}

// choiceBlock is the question and its options, drawn in the composer's place.
func (m model) choiceBlock() string {
	width := max(1, m.width)
	title := lipgloss.NewStyle().Foreground(agentColor).Bold(true)
	dim, accent := lipgloss.NewStyle().Foreground(grayColor), lipgloss.NewStyle().Foreground(accentColor)
	rows := []string{title.Render(ansi.Truncate(sanitizeLabel(m.choice.Title), width, "…"))}
	if m.choiceWriting {
		m.choiceInput.SetWidth(max(1, width-2))
		return strings.Join(append(rows, m.choiceInput.View(), dim.Render(ansi.Truncate("Enter to send · Esc to go back", width, "…"))), "\n")
	}
	options := append(append([]string(nil), m.choice.Options...), "Write your own answer…")
	count := len(options)
	visible := min(count, max(1, m.height-5))
	start := min(max(0, m.choiceSel-visible+1), count-visible)
	for i := start; i < start+visible; i++ {
		label := ansi.Truncate(sanitizeLabel(options[i]), max(0, width-2), "…")
		if i == m.choiceSel {
			if width >= 2 {
				rows = append(rows, title.Render(label)+accent.Render(" <"))
			} else {
				rows = append(rows, accent.Render("<"))
			}
		} else {
			rows = append(rows, dim.Render(label))
		}
	}
	if count > visible {
		rows = append(rows, dim.Render(fmt.Sprintf("  %d of %d", m.choiceSel+1, count)))
	}

	return strings.Join(rows, "\n")
}
