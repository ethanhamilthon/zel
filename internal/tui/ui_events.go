package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	output := &cursorOutput{ReadWriteCloser: os.Stdout}
	// Keep mouse reporting off by default so the terminal owns selection and copying.
	// The tracing entry view enables it temporarily for wheel scrolling.
	m.display = &display{out: output}
	program := tea.NewProgram(m, tea.WithOutput(output), tea.WithAltScreen(), tea.WithFilter(keyboard.filter))
	running, stop := context.WithCancel(ctx)
	defer stop()
	go forwardControl(running, m.events, program)
	// Agents report through the runtime; the UI only hears about changes.
	a.OnSubagentsChanged(func() { go program.Send(subagentsChanged{}) })
	defer a.OnSubagentsChanged(nil)
	a.OnChoiceChanged(func() { go program.Send(choiceChanged{}) })
	defer a.OnChoiceChanged(nil)
	go func() {
		<-running.Done()
		program.Quit()
	}()
	final, err := program.Run()
	keyboard.close()
	stop()
	if m, ok := final.(model); ok {
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

// workingTick is a tick of the loader timer. The model keeps one timer chain:
// a tick of another generation is stale and ignored.
type workingTick struct{ gen uint64 }

// subagentsChanged tells the UI that the running agents or tasks, or a step count, changed.
type subagentsChanged struct{}

// workingFrame is the refresh interval for the working loader and elapsed time.
const workingFrame = 80 * time.Millisecond

// pulse arms the loader timer unless a tick is already outstanding, so any number
// of callers can ask for animation without multiplying the timers.
func (m *model) pulse() tea.Cmd {
	if m.pulsing {
		return nil
	}
	m.pulsing = true
	m.pulseGen++
	gen := m.pulseGen
	return tea.Tick(workingFrame, func(time.Time) tea.Msg { return workingTick{gen} })
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
	star := workingStars[int(max(0, time.Since(status.Started))/workingFrame)%len(workingStars)]
	text := ansi.Truncate(workingText(status, m.turnSteps(), m.ctl.Settings().UI.ShowBash), max(1, m.width-2), "…")
	return lipgloss.NewStyle().Foreground(accentColor).Render(star) + " " + lipgloss.NewStyle().Foreground(grayColor).Render(text) + "\n\n"
}

// workingText is the text of the working indicator: elapsed time, steps (or
// cancelling), queued input, then the activity of the foreground host operation.
// The activity goes last, so a narrow terminal cuts it before time and steps.
func workingText(status core.Status, steps int, showBash bool) string {
	parts := []string{formatDuration(time.Since(status.Started))}
	if status.Cancelling {
		parts = append(parts, "cancelling")
	} else {
		parts = append(parts, formatSteps(steps))
	}
	switch queued := len(status.Queued); {
	case queued == 1:
		parts = append(parts, "queued")
	case queued > 1:
		parts = append(parts, fmt.Sprintf("%d queued", queued))
	}
	if !status.Cancelling && showBash {
		parts = append(parts, cmp.Or(activityLine(status, time.Now()), "thinking..."))
	}
	return strings.Join(parts, " · ")
}

// activityStep is how long each line of a multi-line command stays in the indicator.
const activityStep = 150 * time.Millisecond

// activityLine is the row of the activity to show now: a running multi-line
// command cycles through its lines, anything else shows its first line.
func activityLine(status core.Status, now time.Time) string {
	lines := strings.Split(status.Activity, "\n")
	if !status.Live || len(lines) < 2 {
		return lines[0]
	}
	return lines[int(max(0, now.Sub(status.LiveSince))/activityStep)%len(lines)]
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

// taskBlock renders one indicator row for each running background task. A
// background subagent is a task too, so it shows up here once.
func (m model) taskBlock() string {
	var rows []string
	for _, task := range m.tasks {
		rows = append(rows, m.formatTaskRow(task))
	}
	if len(rows) == 0 {
		return ""
	}
	return strings.Join(rows, "\n\n") + "\n\n"
}

// taskBlink is the half period of the task marker: shown, then hidden.
const taskBlink = 600 * time.Millisecond

// formatTaskRow renders a running background task: [blinking dot] name · working-time
func (m model) formatTaskRow(task core.ActiveTask) string {
	elapsed := time.Since(task.Started)
	icon := "●"
	if (elapsed/taskBlink)%2 == 1 {
		icon = " "
	}
	name := task.Name
	if name == "" {
		name = task.ID
	}
	pad := max(1, m.width-ansi.StringWidth(icon)-1)
	text := ansi.Truncate(name+" · "+formatDuration(elapsed), pad, "…")
	return lipgloss.NewStyle().Foreground(accentColor).Render(icon) + " " + lipgloss.NewStyle().Foreground(grayColor).Render(text)
}

func (m model) handleControl(e core.Event) (tea.Model, tea.Cmd) {
	m.menuDirty = true // anything the session did may show on a live page
	follow := m.viewport.AtBottom()
	var cmd tea.Cmd
	switch e.Kind {
	case "started":
		m.turnIndex = len(m.entries)
		m.refresh(true)
		cmd = m.pulse()
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
		m.tasks = m.ctl.ActiveTasks()
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
	if cmd == nil && m.hasRows() {
		cmd = m.pulse()
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
	m.promptItems = nil
}

type menuTick struct{}

// menuPulse is the timer of the open live pages (Info, Tracing, Resume).
// It has one owner: m.menuPulsing says whether a tick is outstanding.
func menuPulse() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return menuTick{} })
}

// liveMenuPage reports the list page whose data changes while the menu is open;
// the text pages opened from it follow their parent.
func liveMenuPage(mu *menuState) (menuPage, bool) {
	if mu == nil {
		return 0, false
	}
	page := mu.page
	if page == menuText {
		page = mu.parent
	}
	switch page {
	case menuInfo, menuTracing, menuResume:
		return page, true
	}
	return 0, false
}

// ensureMenuPulse arms the menu timer when a live page is open and none is
// outstanding. Static pages never poll.
func (m *model) ensureMenuPulse() tea.Cmd {
	if _, live := liveMenuPage(m.menu); !live || m.menuPulsing {
		return nil
	}
	m.menuPulsing = true
	return menuPulse()
}

// menuRequest is what a snapshot of a live page needs from the UI thread.
type menuRequest struct {
	menu  *menuState
	page  menuPage
	text  bool              // a text page of the list is open
	live  []core.TraceEntry // streaming entries that are not in the session trace yet
	index int
}

// menuSnapshot is the data of a live page, read off the UI thread and applied
// as one immutable unit.
type menuSnapshot struct {
	menu     *menuState
	page     menuPage
	info     []menuRow
	trace    []core.TraceEntry
	sessions []core.SessionSummary
}

func (m *model) menuRequest() (menuRequest, bool) {
	page, live := liveMenuPage(m.menu)
	if !live {
		return menuRequest{}, false
	}
	mu := m.menu
	req := menuRequest{menu: mu, page: page, text: mu.page == menuText, index: mu.traceIndex}
	if page == menuTracing {
		req.live = m.liveTrace()
	}
	return req, true
}

// load reads the snapshot. It touches no model state, so it can run anywhere.
func (r menuRequest) load(ctl *core.Controller) menuSnapshot {
	snap := menuSnapshot{menu: r.menu, page: r.page}
	switch r.page {
	case menuInfo:
		if info, err := ctl.Info(); err == nil {
			snap.info = infoRows(info)
		}
	case menuTracing:
		snap.trace = visibleTrace(append(ctl.Trace(), r.live...))
	case menuResume:
		if sessions, err := ctl.Sessions(); err == nil {
			snap.sessions = sessions
		}
	}
	return snap
}

type menuSnapshotMsg struct{ snap menuSnapshot }

// applyMenuSnapshot publishes a snapshot unless the menu moved on. Data that did
// not change keeps its revision, so cached rows, filters and layouts stay valid.
func (m *model) applyMenuSnapshot(snap menuSnapshot) {
	mu := m.menu
	if mu == nil || mu != snap.menu {
		return
	}
	mu.loading, mu.loaded = false, true
	if page, live := liveMenuPage(mu); !live || page != snap.page {
		return
	}
	changed := false
	switch snap.page {
	case menuInfo:
		if snap.info != nil && !reflect.DeepEqual(mu.info, snap.info) {
			mu.info, changed = snap.info, true
		}
	case menuTracing:
		if !slices.EqualFunc(mu.trace, snap.trace, func(a, b core.TraceEntry) bool {
			return a.Kind == b.Kind && a.At.Equal(b.At) && a.Text == b.Text
		}) {
			mu.trace, changed = snap.trace, true
		}
		if mu.page == menuText && mu.traceIndex >= 0 && mu.traceIndex < len(mu.trace) {
			if text := traceText(mu.trace[mu.traceIndex]); text != mu.text {
				mu.text = text
			}
		}
	case menuResume:
		if snap.sessions != nil && !reflect.DeepEqual(mu.sessions, snap.sessions) {
			mu.sessions, changed = snap.sessions, true
		}
	}
	if changed {
		mu.rev++
		mu.listCache = nil
	}
	// Text pages retain their parent list selection; their own rows are empty.
	if mu.page != menuText {
		mu.selected = min(mu.selected, max(0, len(m.menuRows())-1))
	}
}

// refreshMenu loads and applies a snapshot synchronously, for explicit refreshes
// after a user action. The periodic refresh goes through menuSnapshotCmd.
func (m *model) refreshMenu() {
	if req, ok := m.menuRequest(); ok {
		m.applyMenuSnapshot(req.load(m.ctl))
	}
}

// menuSnapshotCmd loads a snapshot in the background when the page may have
// changed: while the session is active, after a UI event, or on first open. An
// idle session with an unchanged page costs nothing.
func (m *model) menuSnapshotCmd() tea.Cmd {
	mu := m.menu
	if mu == nil || mu.loading {
		return nil
	}
	status := m.ctl.Status()
	active := status.Busy || status.Tasks > 0 || m.hasRows()
	if mu.loaded && !active && !mu.wasActive && !m.menuDirty {
		return nil
	}
	mu.wasActive, m.menuDirty = active, false
	req, ok := m.menuRequest()
	if !ok {
		return nil
	}
	mu.loading = true
	ctl := m.ctl
	return func() tea.Msg { return menuSnapshotMsg{req.load(ctl)} }
}
