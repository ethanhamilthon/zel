package tui

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"zel/internal/core"
)

// Run runs the terminal interface until the user exits.
// The caller remains responsible for closing the agent and releasing session ownership.
func Run(ctx context.Context, a *core.Runtime, cwd string) error {
	keyboard := keyboardProtocol{}
	defer keyboard.close()
	m := newModel(a, cwd)
	defer m.events.Close()
	termOut = os.Stdout
	output := &cursorOutput{ReadWriteCloser: os.Stdout}
	// Keep mouse reporting off by default so the terminal owns selection and copying.
	// The tracing entry view enables it temporarily for wheel scrolling.
	program := tea.NewProgram(m, tea.WithOutput(output), tea.WithAltScreen(), tea.WithFilter(keyboard.filter))
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
		err = errors.Join(err, m.failure)
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
	// Visible assistant output replaces the indicator until work resumes.
	if !status.Cancelling && len(m.entries) > m.turnIndex {
		last := m.entries[len(m.entries)-1]
		if last.Kind == "assistant" && strings.TrimSpace(last.Text) != "" {
			return ""
		}
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

// turnSteps counts the tool calls and thinking blocks of the running turn. The
// chat hides those entries, but they still arrive and keep the count live.
// Completed answers carry authoritative totals, including steps missing from live
// events. Continue counting subsequent work from that total without resetting it.
func (m model) turnSteps() int {
	steps := 0
	for i := max(0, m.turnIndex); i < len(m.entries); i++ {
		record := m.entries[i]
		switch record.Kind {
		case "code", "reasoning":
			steps++
		case "assistant":
			if !record.Preview && record.Meta != nil {
				steps = max(steps, record.Meta.Steps)
			}
		}
	}
	return steps
}

func (m model) handleControl(e core.Event) (tea.Model, tea.Cmd) {
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
		m.entries = core.ApplyEvent(m.entries, *e.Agent)
		if meta := e.Agent.Meta; meta != nil && meta.ContextInput != nil {
			m.contextTokens = *meta.ContextInput
		}
		if time.Since(m.frameAt) >= frameInterval {
			m.refresh(follow)
		} else {
			m.dirty = true
		}
	case "fatal":
		m.failure = errors.New(e.Text)
		m.ctl.LogEvent("error", e.Text)
		m.ctl.Stop()
		return m, tea.Quit
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

func (m *model) restoreUnsent(unsent []core.Input) {
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
		if mu.page == menuText && mu.traceIndex >= 0 && mu.traceIndex < len(mu.trace) {
			mu.text = traceText(mu.trace[mu.traceIndex])
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
