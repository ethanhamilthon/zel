package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zel/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

const keyboardHelp = "Ctrl+K: open the menu (sessions, model, tasks, tracing, rename, info, reload, quit)\n" +
	"Ctrl+N: new session · Ctrl+M: models · Ctrl+R: resume · Ctrl+I: info · Ctrl+T: tracing (Ctrl+M/I require Kitty keyboard protocol)\n" +
	"Enter: send · Shift+Enter / Alt+Enter / Ctrl+J: newline\n" +
	"Sending while zel works: a running command moves to the background (menu: Tasks) and the model reads your message next\n" +
	"Clipboard: Ctrl+V (Cmd+V when forwarded)\n" +
	"Edit a queued message to withdraw it; Enter queues the edited draft again\n" +
	"Up on empty input: recall a message the model has not read yet\n" +
	"Select and copy with terminal shortcuts; use its gesture to open links\n" +
	"PgUp/PgDn: scroll timeline · Ctrl+Y: copy last answer\n" +
	"The chat shows only user messages and answers; the Tracing menu has the rest\n" +
	"Tracing entry: Left/Right switch entries · Up/Down scroll · Esc returns to the list\n" +
	"Esc: dismiss UI · Ctrl+D: cancel current turn · Ctrl+Q: save and quit"

func help() string {
	return keyboardHelp + "\n\nHost access: the lua tool runs code and shell commands with your permissions; not an OS security sandbox."
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

// taskRow splits a background task into the menu's label and detail columns:
// "failed", "done", "stopped" or how long it has been running.
func taskRow(t core.Task) (label, detail string) {
	detail = t.Status
	if t.Status == "running" {
		detail = formatDuration(time.Since(t.Started))
	}
	return t.ID + " " + strings.Join(strings.Fields(t.Command), " "), detail
}

// taskStatus is the status shown in the header of a task page.
func taskStatus(t core.Task) string {
	_, detail := taskRow(t)
	return detail
}

// findTask returns the listed task with the given id.
func (m model) findTask(id string) (core.Task, bool) {
	for _, t := range m.menu.tasks {
		if t.ID == id {
			return t, true
		}
	}
	return core.Task{}, false
}

// taskText is the body of a task page: the command followed by the live output.
func (m model) taskText(id, output string) string {
	t, _ := m.findTask(id)
	return strings.Join(strings.Fields(t.Command), " ") + "\n\n" + output
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
