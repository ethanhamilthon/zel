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
		m.choiceSel = 0
	}
	m.refresh(true)
}

// choiceKey handles a key while the agent's question replaces the composer. Ctrl+D
// (stop the turn) and Ctrl+Q (quit) keep working; other keys do nothing.
func (m model) choiceKey(v tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	n := len(m.choice.Options)
	answer := func(index int) (tea.Model, tea.Cmd, bool) {
		m.ctl.AnswerChoice(m.choice.ID, index)
		m.choice = nil
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
		return answer(m.choiceSel)
	case "esc":
		return answer(-1)
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' && int(key[0]-'0') <= n {
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
	count := len(m.choice.Options)
	visible := min(count, max(1, m.height-6))
	start := min(max(0, m.choiceSel-visible+1), count-visible)
	for i := start; i < start+visible; i++ {
		label := fmt.Sprintf("%d %s", i+1, sanitizeLabel(m.choice.Options[i]))
		if i == m.choiceSel {
			rows = append(rows, accent.Render("❯ ")+title.Render(ansi.Truncate(label, max(1, width-2), "…")))
		} else {
			rows = append(rows, "  "+dim.Render(ansi.Truncate(label, max(1, width-2), "…")))
		}
	}
	if count > visible {
		rows = append(rows, dim.Render(fmt.Sprintf("  %d of %d", m.choiceSel+1, count)))
	}
	hint := "↑↓ choose · enter confirm · esc cancel"
	if m.choice.Waiting > 1 {
		hint = fmt.Sprintf("1/%d · ", m.choice.Waiting) + hint
	}
	rows = append(rows, dim.Render(ansi.Truncate(hint, width, "…")))
	return strings.Join(rows, "\n")
}
