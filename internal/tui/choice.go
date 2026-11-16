package tui

import (
	"fmt"
	"strings"
	"unicode"

	"zel/internal/core"
	"zel/internal/entities"

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
		m.choiceInput = nil
	}
	m.refresh(true)
}

// choiceKey handles a key while the agent's question replaces the composer. Ctrl+D
// (stop the turn) and Ctrl+Q (quit) keep working; other keys do nothing.
func (m model) choiceKey(v tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	n := len(m.choice.Options) + 1
	answer := func(index int) (tea.Model, tea.Cmd, bool) {
		m.keepChoice(m.ctl.AnswerChoice(m.choice.ID, index))
		return m, nil, true
	}
	if m.choiceWriting && v.String() != "ctrl+d" && v.String() != "ctrl+q" {
		switch v.String() {
		case "esc":
			m.choiceWriting = false
		case "enter":
			text := strings.TrimSpace(m.choiceInput.Value())
			if text != "" {
				m.keepChoice(m.ctl.AnswerChoiceText(m.choice.ID, text))
				return m, nil, true
			}
		default:
			if v.Paste && len(v.Runes) == 0 {
				return m, nil, true // an image-only clipboard: answers are text
			}
			cmd := m.updateChoiceInput(v)
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
			if m.choiceInput == nil {
				input := newComposer()
				input.CharLimit = 4096
				input.Placeholder = "Your answer"
				m.choiceInput = &input
			}
			m.refresh(true)
			return m, nil, true
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

// keepChoice closes the answered question and keeps its record in the chat.
func (m *model) keepChoice(record core.Record, ok bool) {
	m.choice, m.choiceWriting = nil, false
	if ok {
		m.entries = append(m.entries, record)
	}
	m.refresh(true)
}

// choiceInputHeight is the most rows the custom answer may take: the screen
// without the question title and the bottom margin.
func (m model) choiceInputHeight() int { return max(1, m.height-3) }

// updateChoiceInput edits the custom answer like the composer: it grows with
// its lines, and Alt+Enter, Shift+Enter or Ctrl+J insert a newline.
func (m *model) updateChoiceInput(msg tea.Msg) tea.Cmd {
	cmd := updateTextarea(m.choiceInput, msg, m.choiceInputHeight())
	fitTextarea(m.choiceInput, m.width, m.choiceInputHeight())
	return cmd
}

// choiceBlock is the question and its options, drawn in the composer's place.
func (m model) choiceBlock() string {
	width := max(1, m.width)
	title := lipgloss.NewStyle().Foreground(agentColor).Bold(true)
	dim, accent := lipgloss.NewStyle().Foreground(grayColor), lipgloss.NewStyle().Foreground(accentColor)
	rows := []string{title.Render(ansi.Truncate(sanitizeLabel(m.choice.Title), width, "…"))}
	if m.choiceWriting {
		fitTextarea(m.choiceInput, width, m.choiceInputHeight())
		lines := strings.Split(m.choiceInput.View(), "\n")
		for i, line := range lines {
			lines[i] = ansi.Truncate(line, width, "")
		}
		return strings.Join(append(rows, lines...), "\n")
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

// renderChoiceRecord draws an answered question as "question: answer" with the
// question in the accent color and the answer in the user message color.
func renderChoiceRecord(text string, width int) string {
	title, answer, _ := strings.Cut(text, entities.ChoiceSeparator)
	head := []rune(sanitizeLabel(title) + ": ")
	orig := append(head, []rune(answer)...)
	rows := strings.Split(ansi.Wrap(string(orig), width, ""), "\n")
	accent, user := lipgloss.NewStyle().Foreground(accentColor), lipgloss.NewStyle().Foreground(userColor)
	pos := 0
	for i, row := range rows {
		var b strings.Builder
		var run []rune
		inHead := true
		flush := func() {
			if len(run) == 0 {
				return
			}
			if inHead {
				b.WriteString(accent.Render(string(run)))
			} else {
				b.WriteString(user.Render(string(run)))
			}
			run = run[:0]
		}
		for _, r := range row {
			for pos < len(orig) && orig[pos] != r && unicode.IsSpace(orig[pos]) {
				pos++
			}
			isHead := pos < len(head)
			if isHead != inHead {
				flush()
				inHead = isHead
			}
			run = append(run, r)
			pos++
		}
		flush()
		rows[i] = b.String()
	}
	return strings.Join(rows, "\n")
}

// wantsMouse reports whether mouse reporting should be on. It stays off by
// default so the terminal owns selection; the tracing entry view scrolls with
// the wheel, and the custom answer moves its cursor with a click.
func (m model) wantsMouse() bool {
	return m.traceEntryView() || (m.menu == nil && m.choice != nil && m.choiceWriting)
}

// clickChoiceInput moves the cursor of the custom answer to the clicked cell.
// The answer starts one row below the composer top, under the question title.
func (m *model) clickChoiceInput(x, y int) tea.Cmd {
	if y < m.viewport.topMargin() || y >= m.viewport.Height {
		return nil
	}
	r := y - m.viewport.topMargin() + m.viewport.YOffset - m.inputTop - 1
	rows := composerLayout(m.choiceInput.Value(), m.choiceInput.Width())
	offset, ok := textareaOffset(m.choiceInput, rows, x, r)
	if !ok {
		return nil
	}
	moveCursor(m.choiceInput, offset, len(rows)+1)
	cmd := m.updateChoiceInput(composerNoop{})
	m.refresh(false)
	return cmd
}
