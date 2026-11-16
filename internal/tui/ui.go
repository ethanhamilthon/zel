package tui

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"zel/internal/core"
)

type entry = core.Record
type renderedEntry struct {
	source   entry
	width    int
	maxLines int
	draft    bool
	Text     string
	rows     []string
}
type model struct {
	ctl                       *core.Controller
	display                   *display
	events                    *eventQueue
	input                     textarea.Model
	pendingSteer, steerSerial uint64
	pendingText               string
	viewport                  pager
	entries                   []entry
	contextTokens             int64
	turnIndex                 int
	statsKey, themeKey        string
	attachments               string
	pasteGeneration           int
	pasting                   bool
	width, height             int
	cwd                       string
	menu                      *menuState
	failure                   error
	hideCompletion            bool
	cache                     []renderedEntry
	inputValue                string
	inputWidth, inputHeight   int
	inputTop                  int
	layoutValue               string
	layoutWidth               int
	layout                    []composerRow
	startCmd                  tea.Cmd
	models                    []core.ModelChoice
	modelsLoading             bool
	modelsKey                 string
	modelsAt                  time.Time
	fileGen, fileDebounceGen  int
	fileLoading               bool
	fileCompletions           []string
	fileSelected              int
	fileTokenStart            int
	fileTokenEnd              int
	fileIndexCache            []string
	fileIndexCacheTime        time.Time
	fileIndexCwd              string
	fileScanCancel            context.CancelFunc
	fileDismissed             bool
	fileDismissedValue        string
	fileDismissedCursor       int
	promptItems               []string // # completions of the token at the cursor
	promptStart, promptEnd    int
	promptSelected            int
	promptSeq                 int
	promptPending             int // seq of the expansion in flight; 0 when none
	promptCancel              context.CancelFunc
	choice                    *core.Choice // the agent's question; replaces the composer while set
	choiceSel                 int
	tasks                     []core.ActiveTask // running background tasks, one chat row each
	frameAt                   time.Time
	dirty                     bool
	renderPending             bool
	redrawScheduled           bool
	pulsing                   bool       // a loader tick is outstanding
	pulseGen                  uint64     // generation of the outstanding tick
	history                   screenRows // the chat rows, rebuilt only when an entry, the width or the theme changed
	historyStale              bool
	menuPulsing, menuDirty    bool // a menu timer tick is outstanding / the session logged something since the last snapshot
}

const (
	frameInterval = 33 * time.Millisecond
	renderBudget  = 25 * time.Millisecond
)

type redraw struct{}

// hasRows reports whether task rows show under the chat; they animate.
func (m model) hasRows() bool { return len(m.tasks) > 0 }

func newComposer() textarea.Model {
	input := textarea.New()
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit, input.MaxHeight, input.MaxWidth = 0, 0, 0
	input.SetHeight(1)
	input.Focus()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	input.FocusedStyle, input.BlurredStyle = composerStyle(), composerStyle()
	input.Cursor.SetMode(cursor.CursorStatic)
	// A zero-width marker locates the native terminal cursor after rendering.
	input.Cursor.Style = lipgloss.NewStyle().Transform(func(s string) string {
		return "\x1b[0m" + composerCursorMarker + lipgloss.NewStyle().Foreground(userHighlight).Render(s)
	})
	return input
}

// composerStyle colors the composer: its text matches sent user messages and
// the line being edited is highlighted.
func composerStyle() textarea.Style {
	base := lipgloss.NewStyle().Foreground(userColor)
	current := lipgloss.NewStyle().Foreground(userHighlight)
	return textarea.Style{Base: base, CursorLine: current, Text: base, Placeholder: base.Foreground(grayColor), EndOfBuffer: base}
}

func newModel(a *core.Runtime, cwd string) model {
	input := newComposer()

	events := newEventQueue()
	ctl := core.NewController(a, func(e core.Event) { events.Push(e) })
	linkBase = cwd
	m := model{ctl: ctl, events: events, input: input, viewport: pager{Width: 80, Height: 24}, width: 80, height: 24, cwd: cwd}
	if home, err := core.ZelHome(cwd); err == nil {
		m.attachments = filepath.Join(home, "attachments")
	}
	if session, err := ctl.Session(); err == nil {
		m.entries, m.contextTokens = session.Records, session.Context
	}
	m.tasks = ctl.ActiveTasks()
	m.refresh(true)
	if len(ctl.Settings().Providers) == 0 {
		m.menu = &menuState{}
		m.openProviderForm("", true)
	} else if ctl.Settings().Model == "" {
		m.menu = &menuState{page: menuModel}
	}
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.startCmd, func() tea.Msg { return prefetchModels{} }, m.ensureMenuPulse())
}
func (m *model) resizeInput() {
	value, width, height := m.input.Value(), max(1, m.width), max(1, m.height-2)
	if m.inputWidth == width && m.inputHeight == height && m.inputValue == value {
		return
	}
	m.inputWidth, m.inputHeight, m.inputValue = width, height, value
	m.input.SetWidth(width)
	lines := 0
	for _, line := range strings.Split(value, "\n") {
		lines += len(wrapRows([]rune(line), m.input.Width()))
	}
	m.input.SetHeight(min(height, max(1, lines)))
}

// updateInput runs the textarea update with its maximum height. The textarea
// scrolls its viewport to the cursor during Update using its current height, so
// with a stale smaller height an inserted newline would scroll earlier lines out
// of view before the height is grown.
func (m *model) updateInput(msg tea.Msg) tea.Cmd {
	previous := m.input.Height()
	m.input.SetHeight(max(1, m.height-2))
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.input.SetHeight(previous)
	m.inputValue, m.inputWidth = "\x00", 0
	return cmd
}

// syncTheme applies [colors] when they changed (a menu or hot reload); it
// reports whether they did, so the caller redraws.
func (m *model) syncTheme() bool {
	colors := m.ctl.Settings().Colors
	key := strings.Join(colors[:], " ")
	if key == m.themeKey {
		return false
	}
	m.themeKey, m.cache, m.menuDirty = key, nil, true
	applyTheme(colors)
	m.input.FocusedStyle, m.input.BlurredStyle = composerStyle(), composerStyle()
	// The textarea draws through a pointer to one of these, taken by Focus/Blur.
	if m.input.Focused() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	return true
}

func (m *model) refresh(follow bool) {
	m.syncTheme()
	m.resizeInput()
	m.viewport.Width = max(1, m.width)
	m.viewport.Height = max(1, m.height)
	// Timeline rows already begin at the first content row; never reserve a header margin.
	m.viewport.TopMargin = 0
	head, tail := m.timelineSegments(renderBudget)
	m.viewport.SetSegments(head, tail)
	m.frameAt, m.dirty = time.Now(), false
	if follow {
		m.viewport.GotoBottom()
	}
}

// add keeps messages in chat and sends all other client output to Tracing.
func (m *model) add(kind, text string) {
	switch kind {
	case "assistant", "user":
		m.entries = core.ApplyEvent(m.entries, core.TurnEvent{Kind: kind, Text: text})
		m.refresh(false)
	default:
		m.ctl.LogEvent(kind, text)
		m.menuDirty = true
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	wasTraceEntry := m.traceEntryView()
	if m.syncTheme() { // hot reload changed the colors: redraw everything with them
		m.refresh(m.viewport.AtBottom())
	}
	if e, ok := msg.(core.Event); !ok || e.Kind != "done" {
		m.syncSteer()
	}
	next, cmd := m.update(msg)
	n, ok := next.(model)
	if !ok {
		return next, cmd
	}
	if wasTraceEntry && !n.traceEntryView() {
		cmd = tea.Batch(cmd, tea.DisableMouse)
	}
	if pulse := n.ensureMenuPulse(); pulse != nil {
		cmd = tea.Batch(cmd, pulse)
	}
	if n.display != nil {
		n.display.draw(n.render(), n.width, n.height)
	}

	if (n.dirty || n.renderPending) && !n.redrawScheduled {
		n.redrawScheduled = true
		wait := max(time.Millisecond, frameInterval-time.Since(n.frameAt))
		cmd = tea.Batch(cmd, tea.Tick(wait, func(time.Time) tea.Msg { return redraw{} }))
	}
	return n, cmd
}

func (m *model) stop(kind, text string) (tea.Model, tea.Cmd) {
	m.add(kind, text)
	m.refresh(true)
	return *m, nil
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	msg = normalizeTerminalKey(msg)
	follow := m.viewport.AtBottom()
	switch v := msg.(type) {
	case fileDebounceMsg:
		return m.handleFileDebounce(v)
	case fileScanMsg:
		return m.handleFileScan(v)
	case clipboardResult:
		return m, m.edit(true, func() tea.Cmd { return m.acceptClipboard(v) })
	case prefetchModels:
		return m, m.fetchModels()
	case modelsLoaded:
		if !m.modelsLoading || v.key != m.modelsKey {
			return m, nil
		}
		m.modelsLoading = false
		var failed []string
		for name, err := range v.errs {
			failed = append(failed, name+": "+err)
		}
		sort.Strings(failed)
		if m.menu != nil {
			switch {
			case v.err != nil:
				m.menu.status = v.err.Error()
			case len(v.models) == 0 && len(failed) > 0:
				m.menu.status = "Could not list models: " + strings.Join(failed, "; ")
			case len(v.models) == 0:
				m.menu.status = "No models returned by providers"
			case len(failed) > 0:
				m.menu.status = "Some providers failed to list models: " + strings.Join(failed, "; ")
			}
		}
		if len(v.models) == 0 && m.models != nil {
			m.refresh(true)
			return m, nil // keep the cached list when a refresh fails
		}
		current := m.ctl.Settings().Provider
		m.models, m.modelsAt = v.models, time.Now()
		slices.SortStableFunc(m.models, func(a, b core.ModelChoice) int {
			rank := func(c core.ModelChoice) int { return map[bool]int{true: 0, false: 1}[c.Provider == current] }
			return cmp.Compare(rank(a), rank(b))
		})
		m.refresh(true)
		return m, nil
	case menuTick:
		m.menuPulsing = false
		cmd := m.menuSnapshotCmd()
		return m, tea.Batch(cmd, m.ensureMenuPulse())
	case menuSnapshotMsg:
		m.applyMenuSnapshot(v.snap)
		return m, nil
	case redraw:
		m.redrawScheduled = false
		if m.dirty || m.renderPending {
			m.refresh(follow)
		}
		if m.hasRows() {
			return m, m.pulse()
		}
		return m, nil
	case workingTick:
		if v.gen != m.pulseGen || !m.pulsing {
			return m, nil // a tick of an earlier timer
		}
		m.pulsing = false
		if m.ctl == nil {
			return m, nil
		}
		if !m.ctl.Status().Busy && !m.hasRows() {
			m.refresh(follow)
			return m, nil
		}
		m.refresh(follow)
		return m, m.pulse()
	case subagentsChanged:
		m.tasks = m.ctl.ActiveTasks()
		m.refresh(follow)
		if m.hasRows() {
			return m, m.pulse()
		}
		return m, nil
	case choiceChanged:
		m.syncChoice()
		return m, nil
	case promptExpanded:
		return m.finishPrompt(v)
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, v.Width), max(1, v.Height)
		m.refresh(follow)
		return m, nil
	case tea.MouseMsg:
		if m.menu != nil {
			return m.menuMouse(v)
		}
		if v.Action == tea.MouseActionPress && v.Button == tea.MouseButtonWheelUp {
			return m, m.scrollUp(3)
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case linkOpened:
		if v.err != nil {
			return m.stop("error", "Could not open "+v.target+": "+v.err.Error())
		}
		return m, nil
	case core.Event:
		return m.handleControl(v)
	case menuShortcut:
		if m.menu != nil && m.menu.page == menuForm {
			return m, nil
		}
		return m.openMenuPage(menuPage(v))
	case tea.KeyMsg:
		if m.choice != nil && m.menu == nil {
			if next, cmd, handled := m.choiceKey(v); handled {
				return next, cmd
			}
		}
		if m.promptPending != 0 {
			switch v.String() {
			case "ctrl+d":
				// The composer stays read-only, but Ctrl+D still stops the active turn
				// and cancels the expansion: a slow completion never holds a turn.
				m.cancelPrompt()
				if m.ctl.Status().Busy {
					m.requestCancel()
				}
				m.refresh(true)
				return m, nil
			case "ctrl+q":
				// quit below
			default:
				return m, nil // the composer is read-only while a prompt expands
			}
		}
		if m.menu != nil && m.menu.page == menuForm && v.String() != "ctrl+c" && v.String() != "ctrl+d" && v.String() != "ctrl+q" {
			return m.formKey(v)
		}
		// Terminals paste only text: an image-only clipboard arrives as an
		// empty bracketed paste (foot's Ctrl+Shift+V, Hyprland's Cmd+V).
		if v.Paste && len(v.Runes) == 0 {
			if m.menu != nil {
				return m, nil
			}
			return m.pasteClipboard()
		}
		if v.String() == "ctrl+t" {
			return m.openMenuPage(menuTracing)
		}
		if v.String() == "ctrl+n" {
			return m.switchSession("")
		}
		if v.String() == "ctrl+r" {
			return m.openMenuPage(menuResume)
		}
		if m.menu == nil && !v.Paste && v.Type == tea.KeyRunes && string(v.Runes) == "/" && m.input.Value() == "" && m.ctl.Settings().UI.SlashMenu {
			cmd := m.openMenu()
			return m, cmd
		}
		if v.String() == "ctrl+k" {
			if m.menu != nil {
				return m.closeMenu(), nil
			}
			cmd := m.openMenu()
			return m, cmd
		}
		if m.menu != nil && v.String() != "ctrl+c" && v.String() != "ctrl+d" && v.String() != "ctrl+q" {
			return m.menuKey(v)
		}
		switch v.String() {
		case "ctrl+v":
			return m.pasteClipboard()
		case "ctrl+c":
			// Copying belongs to the terminal, not the application.
			return m, nil
		case "ctrl+d":
			if m.ctl.Status().Busy {
				m.requestCancel()
				return m, nil
			}
			return m, nil
		case "ctrl+q":
			return m.quit()
		case "esc":
			if m.fileLoading || m.hasFileCompletions() {
				m.dismissFileCompletion()
				m.refresh(true)
				return m, nil
			}
			m.hideCompletion = true
			m.refresh(true)
			return m, nil
		case "pgup", "pgdown", "ctrl+up", "ctrl+down", "ctrl+home", "ctrl+end":
			switch v.String() {
			case "ctrl+home":
				m.viewport.GotoTop()
			case "ctrl+end":
				m.viewport.GotoBottom()
			case "ctrl+up":
				m.viewport.ScrollUp(1)
			case "ctrl+down":
				m.viewport.ScrollDown(1)
			default:
				m.viewport, _ = m.viewport.Update(msg)
			}
			return m, nil
		case "tab":
			if m.hasPromptCompletions() {
				cmd := m.acceptPromptCompletion()
				m.refresh(true)
				return m, cmd
			}
			if m.hasFileCompletions() {
				cmd := m.completeFile(true)
				m.refresh(true)
				return m, cmd
			}
		case "up", "down":
			delta := 1
			if v.String() == "up" {
				delta = -1
			}
			if m.hasPromptCompletions() {
				n := len(m.promptItems)
				m.promptSelected = (m.promptSelected + delta + n) % n
				m.refresh(true)
				return m, nil
			}
			if m.hasFileCompletions() {
				n := len(m.fileCompletions)
				m.fileSelected = (m.fileSelected + delta + n) % n
				m.refresh(true)
				return m, nil
			}
			if v.String() == "up" && m.input.Value() == "" && len(m.ctl.Status().Queued) > 0 {
				in, err := m.ctl.Recall()
				if err != nil {
					return m.reject(err)
				}
				if in != nil {
					m.setComposer(in.Text)
					m.input.CursorEnd()
					m.refresh(true)
					return m, nil
				}
			}
			if v.String() == "up" {
				return m, m.scrollUp(1)
			}
			return m, m.scrollDown(1)
		case "enter":
			if m.pasting {
				return m.stop("notice", "Clipboard read in progress; wait before submitting.")
			}
			if m.hasPromptCompletions() && !m.promptIsComplete() {
				cmd := m.acceptPromptCompletion()
				m.refresh(true)
				return m, cmd
			}
			if m.hasFileCompletions() {
				cmd := m.acceptFileCompletion()
				m.refresh(true)
				return m, cmd
			}
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			if m.ctl.HasPromptTokens(text) {
				return m.expandPrompt(text)
			}
			return m.sendInput(core.Input{Text: text})
		}
		return m, m.edit(follow, func() tea.Cmd { return m.updateInput(msg) })
	}
	if m.menu != nil {
		if m.menu.page == menuForm {
			var cmd tea.Cmd
			m.menu.form.input, cmd = m.menu.form.input.Update(msg)
			return m, cmd
		}
		if searchableMenu(m.menu.page) {
			cmd := m.updateMenuSearch(msg)
			return m, cmd
		}
		return m, nil
	}
	return m, m.edit(follow, func() tea.Cmd { return m.updateInput(msg) })
}

// scrollUp traverses the composer's visual rows before scrolling chat.
func (m *model) scrollUp(rows int) tea.Cmd {
	return m.edit(false, func() tea.Cmd {
		var cmds []tea.Cmd
		for range rows {
			if m.input.Line() == 0 && m.input.LineInfo().RowOffset == 0 {
				m.viewport.ScrollUp(1)
			} else {
				cmds = append(cmds, m.updateInput(tea.KeyMsg{Type: tea.KeyUp}))
			}
		}
		return tea.Batch(cmds...)
	})
}

// scrollDown returns to the composer before moving its cursor down.
func (m *model) scrollDown(rows int) tea.Cmd {
	return m.edit(false, func() tea.Cmd {
		var cmds []tea.Cmd
		for range rows {
			if !m.viewport.AtBottom() {
				m.viewport.ScrollDown(1)
			} else {
				cmds = append(cmds, m.updateInput(tea.KeyMsg{Type: tea.KeyDown}))
			}
		}
		return tea.Batch(cmds...)
	})
}

func (m *model) edit(follow bool, change func() tea.Cmd) tea.Cmd {
	valBefore, curBefore := m.input.Value(), m.composerCursorOffset()
	cmd := change()
	changed := m.input.Value() != valBefore
	moved := m.composerCursorOffset() != curBefore
	if changed {
		m.withdrawSteer()
		m.hideCompletion = false
	}
	var fileCmd tea.Cmd
	if changed || moved {
		m.fileDismissed = false
		fileCmd = m.checkFileCompletion()
		m.checkPromptCompletion()
	}
	m.refresh(follow || changed)
	return tea.Batch(cmd, fileCmd)
}

// View is a placeholder at runtime, where display draws render's frames itself.
func (m model) View() string {
	if m.display != nil {
		return displayFrame
	}
	return m.render()
}

func (m model) render() string {
	if m.menu != nil {
		return m.menuView()
	}
	return frame(strings.Split(m.viewport.View(), "\n"))
}

func (m model) sendInput(input core.Input) (tea.Model, tea.Cmd) {
	return m.deliver(input, m.input.Value())
}

// deliver sends input to the agent, or queues it while a turn runs. draft is the
// composer text the input came from: it stays in the composer while the input is
// queued (editing withdraws it), and is cleared once the agent takes the input.
func (m model) deliver(input core.Input, draft string) (tea.Model, tea.Cmd) {
	if m.pendingSteer != 0 {
		return m, nil
	}
	m.steerSerial++
	input.QueueID = m.steerSerial
	if err := m.ctl.Input(input); err != nil {
		return m.reject(err)
	}
	if slices.ContainsFunc(m.ctl.Status().Queued, func(in core.Input) bool { return in.QueueID == input.QueueID }) {
		m.pendingSteer, m.pendingText = input.QueueID, draft
	} else {
		m.input.Reset()
	}
	m.hideCompletion = false
	m.dismissFileCompletion()
	m.refresh(true)
	return m, nil
}

func (m *model) requestCancel() {
	m.ctl.Stop()
	m.refresh(false)
}

// syncSteer clears the composer when the agent has accepted its queued input.
func (m *model) syncSteer() {
	if m.pendingSteer == 0 {
		return
	}
	for _, in := range m.ctl.Status().Queued {
		if in.QueueID == m.pendingSteer {
			return
		}
	}
	m.pendingSteer = 0
	if m.input.Value() == m.pendingText {
		m.input.Reset()
	}
	m.pendingText = ""
	m.refresh(false)
}

func (m *model) withdrawSteer() {
	if m.pendingSteer != 0 {
		m.ctl.Withdraw(m.pendingSteer)
	}
	m.pendingSteer, m.pendingText = 0, ""
}
