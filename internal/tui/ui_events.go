package tui

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"zel/internal/agent"
	"zel/internal/control"
)

// Run runs the terminal interface until the user exits.
// The caller remains responsible for closing the agent and releasing session ownership.
func Run(ctx context.Context, a *agent.Agent, cwd string) error {
	keyboard := keyboardProtocol{}
	defer keyboard.close()
	m := newModel(a, cwd)
	termOut = os.Stdout
	output := &cursorOutput{ReadWriteCloser: os.Stdout}
	program := tea.NewProgram(m, tea.WithOutput(output), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFilter(keyboard.filter))
	running, stop := context.WithCancel(ctx)
	defer stop()
	go forwardControl(running, m.events, program)
	go func() {
		<-running.Done()
		program.Quit()
	}()
	final, err := program.Run()
	keyboard.close()
	stop()
	if m, ok := final.(model); ok {
		m.term.reset()
		m.ctl.Close()
		if m.fileScanCancel != nil {
			m.fileScanCancel()
		}
	}
	return err
}

func forwardControl(ctx context.Context, events *eventQueue, program *tea.Program) {
	for {
		event, ok := events.Pop(ctx)
		if !ok {
			return
		}
		program.Send(event)
	}
}

type workingTick struct{ started time.Time }

// workingFrame is the refresh interval for the working loader and elapsed time.
const workingFrame = 80 * time.Millisecond

func workingPulse(started time.Time) tea.Cmd {
	return tea.Tick(workingFrame, func(time.Time) tea.Msg { return workingTick{started} })
}

// workingStars is the working indicator's animation, one frame per workingFrame.
var workingStars = []string{"✶", "✸", "✹", "✺", "✹", "✸"}

// workingBlock is the last element of the chat, right above the input: the working
// indicator with the elapsed time and the steps of the turn. What the agent does
// in detail (commands, output, thoughts) is in the Tracing menu.
func (m model) workingBlock() string {
	status := m.ctl.Status()
	if !status.Busy {
		return ""
	}
	parts := []string{formatDuration(time.Since(status.Started))}
	if status.Cancelling {
		parts = append(parts, "cancelling")
	} else {
		parts = append(parts, formatSteps(m.turnSteps()))
	}
	star := workingStars[int(max(0, time.Since(status.Started))/workingFrame)%len(workingStars)]
	text := ansi.Truncate(strings.Join(parts, " · "), max(1, m.width-4), "…")
	return "  " + lipgloss.NewStyle().Foreground(inputBlue).Render(star) + " " + lipgloss.NewStyle().Foreground(muted).Render(text) + "\n\n"
}

// turnStart is the index of the first entry of the running turn.
func (m model) turnStart() int {
	if m.turnIndex > 0 {
		return m.turnIndex
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].Kind == "assistant" {
			return i + 1
		}
	}
	return 0
}

// turnSteps counts the tool calls and thinking blocks of the running turn. The
// chat hides those entries, but they still arrive and keep the count live.
func (m model) turnSteps() int {
	steps := 0
	for i := len(m.entries) - 1; i >= m.turnStart() && i >= 0; i-- {
		switch m.entries[i].Kind {
		case "code", "reasoning":
			steps++
		}
	}
	return steps
}

func (m model) handleControl(e control.Event) (tea.Model, tea.Cmd) {
	follow := m.viewport.AtBottom()
	var cmd tea.Cmd
	switch e.Kind {
	case "started":
		m.term.start(e.Title)
	case "done":
		m.term.finish(e.Status, e.Title)
	case "session":
		m.term.state = "idle"
		m.term.rename(e.Session.Title)
	default:
		if e.Title != "" {
			m.term.rename(e.Title)
		}
	}
	switch e.Kind {
	case "started":
		m.turnIndex = len(m.entries)
		m.refresh(true)
		cmd = workingPulse(m.ctl.Status().Started)
	case "agent":
		m.entries = agent.ApplyEvent(m.entries, *e.Agent)
		if meta := e.Agent.Meta; meta != nil && meta.ContextInput != nil {
			m.contextTokens = *meta.ContextInput
		}
		if time.Since(m.frameAt) >= frameInterval {
			m.refresh(follow)
		} else {
			m.dirty = true
		}
	case "notice", "error":
		m.ctl.LogEvent(e.Kind, e.Text)
	case "session":
		m.entries, m.contextTokens = slices.Clone(e.Session.Records), e.Session.Context
		m.turnIndex = len(m.entries)
		m.cache = nil
		m.pasteGeneration++
		m.pasting = false
		m.dismissFileCompletion()
		m.fileIndexCache = nil
		m.refresh(true)
	case "done":
		if e.Status == "cancelled" {
		} else if e.Status == "error" {
		}
		m.contextTokens = e.Context
		if len(e.Unsent) > 0 {
			m.restoreUnsent(e.Unsent)
		}
		m.syncSteer()
		m.refresh(follow)
	}
	select {
	case <-m.ctl.Closed():
		return m, tea.Quit
	default:
	}
	return m, cmd
}

func (m *model) restoreUnsent(unsent []agent.Input) {
	if m.pendingSteer != 0 {
		for _, in := range unsent {
			if in.QueueID == m.pendingSteer && m.input.Value() == m.pendingText {
				m.input.Reset()
			}
		}
		m.pendingSteer, m.pendingText = 0, ""
	}
	var texts []string
	for _, in := range unsent {
		if in.Text != "" {
			texts = append(texts, in.Text)
		}
	}
	if draft := m.input.Value(); draft != "" {
		texts = append(texts, draft)
	}
	m.setComposer(strings.Join(texts, "\n\n"))
}

func (m *model) setComposer(text string) {
	m.input.SetValue(text)
	m.hideCompletion = false
	m.dismissFileCompletion()
}

type menuTick struct{}

func menuPulse() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return menuTick{} })
}

// Refresh snapshots without changing the query, selection or scroll position.
func (m *model) refreshMenu() {
	mu := m.menu
	if mu == nil {
		return
	}
	page := mu.page
	if page == menuText {
		page = mu.parent
	}
	switch page {
	case menuInfo:
		if info, err := m.ctl.Info(); err == nil {
			mu.info = infoRows(info)
		}
	case menuTasks, menuTask:
		if tasks, err := m.ctl.Tasks(); err == nil {
			mu.tasks = tasks
		}
		if mu.page == menuText {
			if text, err := m.ctl.TaskOutput(mu.task); err == nil {
				mu.text = text
			}
		}
	case menuTracing:
		mu.trace = m.sessionTrace()
		mu.listCache = nil
		if mu.page == menuText && mu.traceIndex < len(mu.trace) {
			e := mu.trace[mu.traceIndex]
			mu.text = traceTitle(e) + "\n\n" + traceText(e)
		}
	case menuResume:
		if sessions, err := m.ctl.Sessions(); err == nil {
			mu.sessions = sessions
		}
	}
	// Text pages retain their parent list selection; their own rows are empty.
	if mu.page != menuText {
		mu.selected = min(mu.selected, max(0, len(m.menuRows())-1))
	}
}
