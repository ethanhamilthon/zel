package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"zel/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

// promptExpanded is an expansion result; session and draft say where the message came from.
type promptExpanded struct {
	seq            int
	session, draft string
	text           string
	err            error
}

func (m model) hasPromptCompletions() bool {
	return m.promptPending == 0 && !m.hideCompletion && len(m.promptItems) > 0
}

// completionList returns the open list and its selected index; the # list wins,
// since a # token and an @ token cannot both hold the cursor.
func (m model) completionList() ([]string, int) {
	if m.hasPromptCompletions() {
		return m.promptItems, m.promptSelected % len(m.promptItems)
	}
	if m.hasFileCompletions() {
		return m.fileCompletions, m.fileSelected % len(m.fileCompletions)
	}
	return nil, 0
}

// checkPromptCompletion opens the # list for the token at the cursor; it reads
// names only, so no callback runs while typing.
func (m *model) checkPromptCompletion() {
	m.promptItems = nil
	if m.hideCompletion || m.ctl == nil {
		return
	}
	start, end, query, ok := core.PromptAt([]rune(m.input.Value()), m.composerCursorOffset())
	if !ok {
		return
	}
	var items []string
	for _, name := range m.ctl.PromptNames() {
		if strings.HasPrefix(name, query) {
			items = append(items, name)
		}
	}
	if len(items) > 0 {
		m.promptStart, m.promptEnd, m.promptItems, m.promptSelected = start, end, items, 0
	}
}

// acceptPromptCompletion writes the selected name as #name; the list stays closed until the next edit.
func (m *model) acceptPromptCompletion() tea.Cmd {
	if !m.hasPromptCompletions() {
		return nil
	}
	before, runes := m.input.Value(), []rune(m.input.Value())
	start := max(0, min(m.promptStart, len(runes)))
	end := max(start, min(m.promptEnd, len(runes)))
	token := []rune("#" + m.promptItems[m.promptSelected])
	m.rememberToken(string(token))
	m.input.SetValue(string(runes[:start]) + string(token) + string(runes[end:]))
	if m.input.Value() != before {
		m.withdrawSteer()
	}
	cmd := m.setCursorOffset(start + len(token))
	m.promptItems, m.hideCompletion = nil, true
	return cmd
}

// promptIsComplete reports whether the token already is the selected name: Enter then submits.
func (m model) promptIsComplete() bool {
	runes := []rune(m.input.Value())
	start, end := max(0, min(m.promptStart, len(runes))), max(0, min(m.promptEnd, len(runes)))
	return string(runes[start:end]) == "#"+m.promptItems[m.promptSelected]
}

// expandPrompt runs the expansion outside the UI loop; the composer is read-only until the result arrives.
func (m model) expandPrompt(text string) (tea.Model, tea.Cmd) {
	if m.pendingSteer != 0 {
		return m, nil
	}
	m.promptSeq++
	seq, ctl := m.promptSeq, m.ctl
	ctx, cancel := context.WithCancel(context.Background())
	m.promptCancel = cancel
	m.promptPending, m.promptItems = seq, nil
	session, draft := ctl.Status().Session, m.input.Value()
	m.refresh(true)
	return m, func() tea.Msg {
		out, err := ctl.ExpandPrompt(ctx, text)
		return promptExpanded{seq: seq, session: session, draft: draft, text: out, err: err}
	}
}

// cancelPrompt stops the expansion in flight. Nothing is sent and the draft stays
// in the composer; the late result is ignored, since no expansion is pending.
func (m *model) cancelPrompt() {
	if m.promptPending == 0 {
		return
	}
	m.promptPending = 0
	if m.promptCancel != nil {
		m.promptCancel()
		m.promptCancel = nil
	}
	m.entries = core.ApplyEvent(m.entries, core.TurnEvent{Kind: "prompt_error", Text: "prompt not sent: cancelled; your draft is kept"})
	m.refresh(true)
}

// promptFailed shows why a message was not sent in the chat; the draft stays in the composer.
func (m model) promptFailed(err error) (tea.Model, tea.Cmd) {
	m.entries = core.ApplyEvent(m.entries, core.TurnEvent{Kind: "prompt_error", Text: err.Error()})
	m.refresh(true)
	return m, nil
}

// finishPrompt sends the expanded message, or keeps the draft and says why not.
func (m model) finishPrompt(done promptExpanded) (tea.Model, tea.Cmd) {
	if done.seq != m.promptPending {
		return m, nil
	}
	m.promptPending = 0
	if m.promptCancel != nil {
		m.promptCancel()
		m.promptCancel = nil
	}
	switch {
	case m.ctl.Status().Session != done.session:
		return m.promptFailed(errors.New("the session changed while the prompt was expanding; your draft is kept"))
	case done.err != nil:
		return m.promptFailed(fmt.Errorf("prompt not sent: %v; your draft is kept", done.err))
	case m.input.Value() != done.draft:
		return m.promptFailed(errors.New("the draft changed while the prompt was expanding; your draft is kept"))
	}
	return m.deliver(core.Input{Text: done.text}, done.draft)
}
