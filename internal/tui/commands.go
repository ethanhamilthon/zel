package tui

import (
	"context"
	"fmt"
	"time"

	"zel/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

const keyboardHelp = "Ctrl+K: open the menu (sessions, model, tracing, rename, info, quit)\n" +
	"Ctrl+N: new session · Ctrl+M: models · Ctrl+R: resume · Ctrl+I: info · Ctrl+T: tracing (Ctrl+M/I require Kitty keyboard protocol)\n" +
	"Enter: send · Shift+Enter / Alt+Enter / Ctrl+J: newline\n" +
	"Sending while zel works: the message waits until the running call ends, then the model reads it\n" +
	"Clipboard: Ctrl+V (Cmd+V when forwarded)\n" +
	"Edit a queued message to withdraw it; Enter queues the edited draft again\n" +
	"Up on empty input: recall a message the model has not read yet\n" +
	"Select and copy with terminal shortcuts; use its gesture to open links\n" +
	"PgUp/PgDn: scroll timeline\n" +
	"The chat shows only user messages and answers; the Tracing menu has the rest\n" +
	"Tracing entry: Left/Right switch entries · Up/Down scroll · Esc returns to the list\n" +
	"Esc: dismiss UI · Ctrl+D: cancel current turn · Ctrl+Q: save and quit"

func help() string {
	return keyboardHelp + "\n\nzel.toml changes apply by themselves (hot reload).\nHost access: the bash tool runs commands with your permissions; not an OS security sandbox."
}

func sessionTitle(s core.SessionSummary) string {
	title := []rune(s.Title)
	if len(title) == 0 {
		return "(no message)"
	}
	if len(title) > 50 {
		return string(title[:49]) + "…"
	}
	return string(title)
}

func sessionTime(t time.Time) string {
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("2006-01-02")
}

type modelsLoaded struct {
	key    string
	models []core.ModelChoice
	errs   map[string]string
	err    error
}

// prefetchModels warms the model list in the background when the TUI starts.
type prefetchModels struct{}

// modelsCacheTTL is how long a listed set of models opens the model menu without a refresh.
const modelsCacheTTL = 10 * time.Minute

// fetchModels lists models in the background unless a fresh list for the
// configured providers is cached; a stale list stays visible while it refreshes.
func (m *model) fetchModels() tea.Cmd {
	settings := m.ctl.Settings()
	if len(settings.Providers) == 0 {
		return nil
	}
	key := fmt.Sprint(settings.Providers)
	if key != m.modelsKey {
		m.models, m.modelsKey, m.modelsLoading = nil, key, false
	}
	if m.modelsLoading || (m.models != nil && time.Since(m.modelsAt) < modelsCacheTTL) {
		return nil
	}
	m.modelsLoading = true
	ctl := m.ctl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		models, errs, err := ctl.Models(ctx)
		return modelsLoaded{key, models, errs, err}
	}
}

func (m model) quit() (tea.Model, tea.Cmd) {
	m.dismissFileCompletion()
	m.cancelPrompt()
	closed, err := m.ctl.Quit()
	if err != nil {
		return m.reject(err)
	}
	if closed {
		return m, tea.Quit
	}
	m.refresh(true)
	return m, nil
}

func (m *model) resetCompletions() {
	m.dismissFileCompletion()
	m.promptItems = nil
	m.fileIndexCache = nil
}

func (m model) submit(err error) (tea.Model, tea.Cmd) {
	if err != nil {
		return m.reject(err)
	}
	m.refresh(true)
	return m, nil
}

func (m model) reject(err error) (tea.Model, tea.Cmd) {
	m = m.closeMenu()
	m.add("error", err.Error())
	m.refresh(true)
	return m, nil
}
