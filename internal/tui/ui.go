package tui

import (
	"cmp"
	"context"
	"fmt"
	"os"
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
	editorActive              bool
	ctl                       *core.Controller
	term                      termState
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
	directoryGen              uint64
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
	frameAt                   time.Time
	dirty                     bool
	renderPending             bool
	redrawScheduled           bool
}

const (
	frameInterval = 33 * time.Millisecond
	renderBudget  = 25 * time.Millisecond
)

type redraw struct{}

func newComposer() textarea.Model {
	input := textarea.New()
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.MaxHeight = 0
	input.MaxWidth = 0
	input.SetHeight(1)
	input.Focus()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	base := lipgloss.NewStyle().Foreground(foreground)
	style := textarea.Style{Base: base, CursorLine: base, Text: base, Placeholder: base.Foreground(muted), EndOfBuffer: base}
	input.FocusedStyle, input.BlurredStyle = style, style
	input.Cursor.SetMode(cursor.CursorStatic)
	// A zero-width marker locates the native terminal cursor after rendering.
	input.Cursor.Style = lipgloss.NewStyle().Transform(func(s string) string {
		return "\x1b[0m" + composerCursorMarker + lipgloss.NewStyle().Foreground(foreground).Render(s)
	})
	return input
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
	return tea.Batch(m.startCmd, func() tea.Msg { return prefetchModels{} }, menuPulse())
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
func (m *model) refresh(follow bool) {
	if theme := m.ctl.Settings().UI.Theme; fmt.Sprint(theme) != m.themeKey {
		m.themeKey, m.cache = fmt.Sprint(theme), nil
		applyTheme(theme)
		m.term.setBackground(theme["background"])
	}
	m.resizeInput()
	m.viewport.Width = max(1, m.width)
	m.viewport.Height = max(1, m.height)
	// Timeline rows already begin at the first content row; never reserve a header margin.
	m.viewport.TopMargin = 0
	m.viewport.SetLines(m.timelineRows(renderBudget))
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
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	wasTraceEntry := m.traceEntryView()
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
	case directoryResult:
		m.applyDirectory(v)
		return m, nil
	case fileEditorFinished:
		m.editorActive = false
		if v.err != nil {
			return m.reject(v.err)
		}
		if m.menu != nil && m.menu.page == menuFiles {
			return m, m.readDirectory(m.menu.directory)
		}
		m.refresh(true)
		return m, nil
	case editorFinished:
		m.editorActive = false
		if v.err != nil {
			os.Remove(v.path)
			return m.reject(v.err)
		}
		if m.input.Value() != v.original {
			return m.reject(fmt.Errorf("Draft changed while Editor was open; saved text kept at %s", v.path))
		}
		os.Remove(v.path)
		m.setComposer(v.text)
		m.refresh(true)
		return m, nil
	case menuTick:
		m.refreshMenu()
		return m, menuPulse()
	case redraw:
		m.redrawScheduled = false
		if m.dirty || m.renderPending {
			m.refresh(follow)
		}
		return m, nil
	case workingTick:
		status := m.ctl.Status()
		if !status.Busy || !v.started.Equal(status.Started) {
			return m, nil
		}
		m.term.tick()
		m.refresh(follow)
		return m, workingPulse(status.Started)
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
		if m.menu != nil && m.menu.page == menuForm && v.String() != "ctrl+c" && v.String() != "ctrl+d" && v.String() != "ctrl+y" && v.String() != "ctrl+q" {
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
		if v.String() == "ctrl+e" {
			return m.editInput()
		}
		if v.String() == "ctrl+f" {
			return m.openMenuPage(menuFiles)
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
		if m.menu != nil && v.String() != "ctrl+c" && v.String() != "ctrl+d" && v.String() != "ctrl+y" && v.String() != "ctrl+q" {
			return m.menuKey(v)
		}
		switch v.String() {
		case "ctrl+y":
			return m.copyAnswer()
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
			if m.hasFileCompletions() {
				cmd := m.acceptFileCompletion()
				m.refresh(true)
				return m, cmd
			}
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
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
	}
	m.refresh(follow || changed)
	return tea.Batch(cmd, fileCmd)
}
func (m model) View() string {
	if m.menu != nil {
		return m.menuView()
	}
	lines := strings.Split(m.viewport.View(), "\n")
	row, column := 0, 0
	for i, line := range lines {
		if at := strings.Index(line, composerCursorMarker); at >= 0 {
			row, column = i+1, lipgloss.Width(line[:at])+1
		}
		lines[i] = strings.ReplaceAll(line, composerCursorMarker, "")
	}
	return strings.Join(lines, "\n") + fmt.Sprintf("\x1b]1337;ZelCaret;%d;%d\x07", row, column)
}

func (m model) sendInput(input core.Input) (tea.Model, tea.Cmd) {
	if m.pendingSteer != 0 {
		return m, nil
	}
	m.steerSerial++
	input.QueueID = m.steerSerial
	if err := m.ctl.Input(input); err != nil {
		return m.reject(err)
	}
	if slices.ContainsFunc(m.ctl.Status().Queued, func(in core.Input) bool { return in.QueueID == input.QueueID }) {
		m.pendingSteer, m.pendingText = input.QueueID, m.input.Value()
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
