package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zel/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
)

const keyboardHelp = "Ctrl+K: open the menu (sessions, model, tasks, tracing, rename, info, reload, quit)\n" +
	"Ctrl+M: models · Ctrl+R: resume · Ctrl+I: info · Ctrl+T: tracing · Ctrl+F: files · Ctrl+E: editor (Ctrl+M/I require Kitty keyboard protocol)\n" +
	"Enter: send · Shift+Enter / Alt+Enter / Ctrl+J: newline\n" +
	"Sending while zel works: a running command moves to the background (menu: Tasks) and the model reads your message next\n" +
	"Clipboard: Ctrl+V (Cmd+V when forwarded)\n" +
	"Edit a queued message to withdraw it; Enter queues the edited draft again\n" +
	"Up on empty input: recall a message the model has not read yet\n" +
	"Click a link or a path in backticks to open it\n" +
	"PgUp/PgDn or mouse wheel: scroll timeline\n" +
	"The chat shows only user messages and answers; the Tracing menu has the rest\n" +
	"Esc / Ctrl+C: cancel current turn · Ctrl+Q: save and quit"

func help() string {
	return keyboardHelp + "\n\nHost access: the bash tool runs commands with your permissions; not an OS security sandbox."
}

func sessionTitle(s agent.SessionSummary) string {
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

// taskRow splits a background task into the menu's label and detail columns.
func taskRow(t agent.Task) (label, detail string) {
	status := t.Status
	if t.ExitCode != nil && *t.ExitCode != 0 {
		status += fmt.Sprintf(" (%d)", *t.ExitCode)
	}
	end := t.Ended
	if end.IsZero() {
		end = time.Now()
	}
	return t.ID + " $ " + strings.Join(strings.Fields(t.Command), " "), status + " " + formatDuration(end.Sub(t.Started))
}

type modelsLoaded struct {
	key    string
	models []agent.ModelChoice
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
