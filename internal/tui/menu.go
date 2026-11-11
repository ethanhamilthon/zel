package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"zel/internal/agent"
)

// The menu (Ctrl+K) replaces the slash commands: a block centered on the screen
// with the page path on the left and its keyboard shortcut on the right.

const menuWidth = 56
const maxListItems = 10

// Version is supplied by the CLI at startup.
var Version = "dev"

type menuShortcut menuPage

type menuPage int

const (
	menuMain menuPage = iota
	menuResume
	menuModel
	menuEffort
	menuTasks
	menuTask
	menuTracing
	menuRename
	menuInfo
	menuHelp
	menuText
	menuProviders
	menuProvider
	menuForm
	menuSettings
	menuFiles
	menuConfirm
)

var menuTitles = map[menuPage]string{
	menuMain: "Menu", menuResume: "Menu/Resume", menuModel: "Menu/Models", menuEffort: "Menu/Models/Effort",
	menuTasks: "Menu/Tasks", menuTask: "Menu/Tasks/Task", menuTracing: "Menu/Tracing",
	menuProviders: "Menu/Providers", menuProvider: "Menu/Providers/Provider", menuForm: "Menu/Settings/Edit", menuSettings: "Menu/Settings", menuFiles: "Menu/Files", menuRename: "Menu/Rename", menuInfo: "Menu/Info", menuHelp: "Menu/Help",
}

type menuRow struct{ label, detail string }

var mainRows = []menuRow{
	{"Resume", "Saved sessions"}, {"New session", "Fresh chat, re-read config · Ctrl+N"}, {"Models", "Model, reasoning effort"},
	{"Tasks", "Background tasks"}, {"Tracing", "Everything the session did"}, {"Rename", "Rename this session"},
	{"Info", "Session, config, tokens"}, {"Reload", "Re-read the config"}, {"Help", "Keyboard shortcuts"},
	{"Quit", "Save and exit"}, {"Editor", "Edit input · Ctrl+E"},
	{"Providers", "Add, edit, remove"}, {"Settings", "Editor, output, timeouts"}, {"Files", "Browse and edit"},
}

type menuListCache struct {
	page     menuPage
	count    int
	rows     []menuRow
	query    string
	indices  []int
	filtered []menuRow
}

type menuState struct {
	listCache       *menuListCache
	form            *formState
	directory       string
	files           []fileItem
	providerNames   []string
	direct          bool // opened by a shortcut, not through Menu
	traceIndex      int
	page            menuPage
	selected        int
	offset          int      // first row of a text page
	parent          menuPage // where Esc returns from a text page
	title, text     string   // the title and body of a text page
	provider, model string   // the model chosen on the Model page, waiting for an effort
	task, name      string   // the task opened on the Tasks page; the Rename text
	query           string   // the search text of the current page
	search          *textarea.Model
	status          string
	confirm         string
	target          string
	sessions        []agent.SessionSummary
	info            []menuRow
	tasks           []agent.Task
	trace           []agent.TraceEntry
}

// File and trace sources change only when refreshed, not on cursor movement.
func (m model) rawMenuRows() []menuRow {
	mu := m.menu
	if mu.page != menuFiles && mu.page != menuTracing {
		return m.buildMenuRows()
	}
	count := len(mu.files)
	if mu.page == menuTracing {
		count = len(mu.trace)
	}
	if c := mu.listCache; c != nil && c.page == mu.page && c.count == count {
		return c.rows
	}
	rows := m.buildMenuRows()
	mu.listCache = &menuListCache{page: mu.page, count: count, rows: rows}
	return rows
}

func (m model) buildMenuRows() []menuRow {
	mu := m.menu
	var rows []menuRow
	switch mu.page {
	case menuMain:
		rows = slices.Clone(mainRows)
		if n := m.ctl.Status().Tasks; n > 0 {
			rows[3].detail = fmt.Sprintf("%d running", n)
		}
	case menuProviders:
		rows = []menuRow{{"Add provider", ""}}
		for _, name := range mu.providerNames {
			rows = append(rows, menuRow{name, m.ctl.Settings().Providers[name].BaseURL})
		}
	case menuConfirm:
		rows = []menuRow{{"Cancel", "Keep everything as it is"}, {"Confirm", mu.confirm}}
	case menuProvider:
		rows = []menuRow{{"Edit provider", mu.provider}, {"Delete provider", ""}}
	case menuSettings:
		s := m.ctl.Settings()
		rows = []menuRow{{"Editor", s.UI.Editor}, {"Output limit", fmt.Sprintf("%d bytes", s.ShellOutput)}, {"Bash timeout", fmt.Sprintf("%d seconds", s.ShellTimeout)}, {"Reasoning summary", fmt.Sprint(s.ReasoningSummary)}}
	case menuFiles:
		for _, f := range mu.files {
			detail := "file"
			if f.dir {
				detail = "directory"
			}
			rows = append(rows, menuRow{f.name, detail})
		}
		if len(rows) == 0 {
			rows = []menuRow{{"Unable to read directory", mu.status}}
		}
	case menuResume:
		for _, s := range mu.sessions {
			rows = append(rows, menuRow{sessionTitle(s), sessionTime(s.Updated)})
		}
		if rows == nil {
			rows = []menuRow{{"No saved sessions", ""}}
		}
	case menuModel:
		for _, c := range m.filteredModels() {
			detail := c.Provider
			settings := m.ctl.Settings()
			if c.Provider == settings.Provider && c.Model == settings.Model {
				detail += " · current"
			}
			rows = append(rows, menuRow{c.Model, detail})
		}
		switch {
		case rows != nil:
		case mu.query != "" && len(m.models) > 0:
			rows = []menuRow{{"No matches", ""}}
		case mu.status != "":
			rows = []menuRow{{mu.status, ""}}
		case m.modelsLoading:
			rows = []menuRow{{"Loading models...", ""}}
		default:
			rows = []menuRow{{"No models", ""}}
		}
	case menuEffort:
		settings := m.ctl.Settings()
		for _, effort := range agent.Efforts {
			row := menuRow{label: effort}
			if mu.model == settings.Model && mu.provider == settings.Provider && effort == settings.Effort {
				row.detail = "current"
			}
			rows = append(rows, row)
		}
	case menuTasks:
		for i := len(mu.tasks) - 1; i >= 0; i-- {
			label, detail := taskRow(mu.tasks[i])
			rows = append(rows, menuRow{label, detail})
		}
		if rows == nil {
			rows = []menuRow{{"No background tasks", ""}}
		}
	case menuTask:
		rows = []menuRow{{"Show output", ""}, {"Stop task", "kill"}}
	case menuTracing:
		for _, e := range mu.trace {
			rows = append(rows, menuRow{traceLabel(e), traceDetail(e)})
		}
		if rows == nil {
			rows = []menuRow{{"Nothing traced yet", ""}}
		}
	case menuInfo:
		rows = mu.info
	case menuRename:
		rows = []menuRow{{mu.name + "▏", "empty = default title"}}
	}
	if mu.page == menuModel {
		rows = append(rows, menuRow{"Retry model listing", ""}, menuRow{"Enter model manually", ""})
	}
	return rows
}

func searchableMenu(page menuPage) bool {
	return page == menuMain || page == menuResume || page == menuModel || page == menuEffort || page == menuTracing || page == menuProviders || page == menuFiles
}

func searchMatches(text, query string) bool {
	text = strings.ToLower(text)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

// menuIndices preserves action identities when the visible rows are filtered.
func (m model) menuIndices(rows []menuRow) []int {
	c := m.menu.listCache
	if c != nil && c.page == m.menu.page && c.filtered != nil && c.query == m.menu.query {
		return c.indices
	}
	return m.buildMenuIndices(rows)
}

func (m model) buildMenuIndices(rows []menuRow) []int {
	var indices []int
	query := m.menu.query
	kind := ""
	if m.menu.page == menuTracing && strings.HasPrefix(query, ":") {
		fields := strings.Fields(query)
		kind, query = strings.ToLower(strings.TrimPrefix(fields[0], ":")), strings.Join(fields[1:], " ")
	}
	for i, row := range rows {
		text := row.label + " " + row.detail
		if m.menu.page == menuResume && i < len(m.menu.sessions) {
			text = m.menu.sessions[i].Title + " " + row.detail
		}
		if m.menu.page == menuTracing && i < len(m.menu.trace) {
			e := m.menu.trace[i]
			if kind != "" && e.Kind != kind {
				continue
			}
			text += " " + e.Text
		} else if kind != "" {
			continue
		}
		if !searchableMenu(m.menu.page) || m.menu.page == menuModel || searchMatches(text, query) {
			indices = append(indices, i)
		}
	}
	return indices
}

func (m model) menuRows() []menuRow {
	rows := m.rawMenuRows()
	c := m.menu.listCache
	if c != nil && c.page == m.menu.page && c.filtered != nil && c.query == m.menu.query {
		return c.filtered
	}
	indices := m.menuIndices(rows)
	var found []menuRow
	for _, i := range indices {
		found = append(found, rows[i])
	}
	if len(found) == 0 {
		found = []menuRow{{"No matches", ""}}
	}
	if c != nil && c.page == m.menu.page {
		c.query, c.indices, c.filtered = m.menu.query, indices, found
	}
	return found
}

func (m model) menuInput(width int) *textarea.Model {
	mu := m.menu
	if mu.search == nil {
		input := newComposer()
		input.Placeholder = "type to search"
		input.KeyMap.InsertNewline.SetEnabled(false)
		mu.search = &input
	}
	if mu.search.Value() != mu.query {
		mu.search.SetValue(mu.query)
	}
	mu.search.SetWidth(max(1, width-2))
	mu.search.SetHeight(1)
	return mu.search
}

// filteredModels returns the models that match every word of the search text
// in the model name or the provider name.
func (m model) filteredModels() []agent.ModelChoice {
	settings := m.ctl.Settings()
	choices := []agent.ModelChoice{{Provider: settings.Provider, Model: settings.Model}}
	for _, c := range m.models {
		if c.Provider != settings.Provider || c.Model != settings.Model {
			choices = append(choices, c)
		}
	}
	var found []agent.ModelChoice
	for _, c := range choices {
		if c.Model != "" && searchMatches(c.Model+" "+c.Provider, m.menu.query) {
			found = append(found, c)
		}
	}
	return found
}

// editText applies a typed key to a one-line text and reports whether it changed.
func editText(text *string, v tea.KeyMsg) bool {
	switch v.Type {
	case tea.KeyBackspace:
		r := []rune(*text)
		if len(r) == 0 {
			return false
		}
		*text = string(r[:len(r)-1])
	case tea.KeySpace:
		*text += " "
	case tea.KeyRunes:
		*text += strings.NewReplacer("\n", " ", "\r", " ").Replace(string(v.Runes))
	default:
		return false
	}
	return true
}

// updateMenuSearch also routes asynchronous textarea paste results to the menu,
// keeping them out of the main composer.
func (m *model) updateMenuSearch(msg tea.Msg) tea.Cmd {
	input := m.menuInput(max(1, m.menuWidth()-2))
	if key, ok := msg.(tea.KeyMsg); ok && key.Paste {
		key.Runes = []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(string(key.Runes)))
		msg = key
	}
	previous := input.Value()
	next, cmd := input.Update(msg)
	*input = next
	value := strings.NewReplacer("\n", " ", "\r", " ").Replace(input.Value())
	if value != input.Value() {
		input.SetValue(value)
	}
	m.menu.query = value
	if value != previous {
		m.menu.selected = 0
	}
	return cmd
}

func (m model) menuView() string {
	if m.menu.page == menuForm {
		return m.formView()
	}
	pad, w := strings.Repeat(" ", m.menuLeft()), m.menuWidth()
	title, shortcut := m.menuHead()
	lines := make([]string, 0, m.height)
	lines = append(lines, make([]string, m.menuTop())...)
	lines = append(lines, pad+lipgloss.NewStyle().Foreground(foreground).Render(title)+
		strings.Repeat(" ", max(1, w-ansi.StringWidth(title)-ansi.StringWidth(shortcut)))+
		lipgloss.NewStyle().Foreground(muted).Render(shortcut), "")
	if searchableMenu(m.menu.page) {
		// The search input sits one blank line below the header.
		lines = append(lines, pad+renderRail(m.menuInput(max(1, w-2)).View(), w, false), "")
	}
	lines = append(lines, m.pageLines(pad, w)...)
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	row, column := 0, 0
	for i, line := range lines[:m.height] {
		if at := strings.Index(line, composerCursorMarker); at >= 0 {
			row, column = i+1, ansi.StringWidth(line[:at])+1
		}
		lines[i] = strings.ReplaceAll(line, composerCursorMarker, "")
	}
	return strings.Join(lines[:m.height], "\n") + fmt.Sprintf("\x1b]1337;ZelCaret;%d;%d\x07", row, column)
}

// menuBodyStart is the screen row of the first body line and menuBodyRoom the
// number of rows the body may use.
func (m model) menuBodyStart() int {
	start := m.menuTop() + 2 // the header and the blank line below it
	if searchableMenu(m.menu.page) {
		start += 2 // the search input and the blank line below it
	}
	return start
}

func (m model) menuBodyRoom() int { return max(1, m.height-m.menuBodyStart()) }

// menuLeft, menuTop and menuWidth place the menu block on the screen; the
// prepared entries of a text page scroll inside it.
func (m model) menuLeft() int { return max(0, (m.width-m.menuWidth())/2) }

func (m model) menuTop() int {
	header := 2
	if searchableMenu(m.menu.page) {
		header += 2
	}
	body := len(m.menuRows())
	if body > maxListItems {
		body = maxListItems + 1
	}
	if m.menu.page == menuRename {
		body++
	}
	if m.menu.page == menuText || m.menu.page == menuInfo {
		body = len(m.wrapText(m.menuText(), m.menuWidth()))
	}
	return max(0, (m.height-header-body)/2)
}

func (m model) menuWidth() int { return max(10, min(menuWidth, m.width-4)) }

// menuHead is the header of the current page: its path on the left, the shortcut
// that opens it on the right.
func (m model) menuHead() (title, shortcut string) {
	title, shortcut = menuTitles[m.menu.page], map[menuPage]string{menuMain: "Ctrl+K", menuModel: "Ctrl+M", menuResume: "Ctrl+R", menuInfo: "Ctrl+I", menuTracing: "Ctrl+T", menuFiles: "Ctrl+F"}[m.menu.page]
	if m.menu.page == menuMain {
		name := m.ctl.Settings().Name
		if name == "" {
			name = "Menu"
		}
		title = "zel " + strings.TrimPrefix(Version, "v") + "/" + name
	}
	if m.menu.page == menuFiles {
		title = "Menu/Files " + m.menu.directory
	}
	if m.menu.page == menuText {
		title = m.menu.title
		if m.menu.parent == menuTracing && m.menu.traceIndex >= 0 && m.menu.traceIndex < len(m.menu.trace) {
			shortcut = fmt.Sprintf("%d/%d", m.menu.traceIndex+1, len(m.menu.trace))
		}
	}
	return ansi.Truncate(title, max(1, m.menuWidth()-ansi.StringWidth(shortcut)-1), "…"), shortcut
}

// pageLines draws the body of the current page: text pages scroll, the others
// list their rows with the selected one in view.
func (m model) pageLines(pad string, w int) []string {
	if m.menu.page == menuText || m.menu.page == menuInfo {
		body := m.wrapText(m.menuText(), w)
		height := m.menuBodyRoom()
		start := min(max(0, m.menu.offset), max(0, len(body)-1))
		lines := make([]string, 0, height)
		for i := start; i < min(len(body), start+height); i++ {
			lines = append(lines, pad+lipgloss.NewStyle().Foreground(foreground).Render(body[i]))
		}
		return lines
	}
	rows := m.menuRows()
	width := w
	fg := lipgloss.NewStyle().Foreground(foreground)
	dim, accent := lipgloss.NewStyle().Foreground(muted), lipgloss.NewStyle().Foreground(inputBlue)
	first, visible := m.listWindow(len(rows))
	indices := m.menuIndices(m.rawMenuRows())
	var lines []string
	for i := first; i < min(len(rows), first+visible); i++ {
		detailMax := width / 2
		detail := ansi.Truncate(rows[i].detail, detailMax, "…")
		dw := ansi.StringWidth(detail)
		label := ansi.Truncate(rows[i].label, max(1, width-3-dw), "…")
		marker := "  "
		if i == m.menu.selected {
			marker = " <"
		}
		gap := strings.Repeat(" ", max(0, width-ansi.StringWidth(label)-2-dw))
		prefix := ""
		if m.menu.page == menuTracing && i < len(indices) {
			number := indices[i] + 1
			prefix = lipgloss.NewStyle().Foreground(darkGray).Render(fmt.Sprintf("%*d ", len(fmt.Sprint(len(m.menu.trace))), number))
			label = ansi.Truncate(label, max(1, width-3-dw-ansi.StringWidth(prefix)), "…")
			gap = strings.Repeat(" ", max(0, width-ansi.StringWidth(prefix)-ansi.StringWidth(label)-2-dw))
		}
		lines = append(lines, pad+prefix+fg.Render(label)+accent.Render(marker)+gap+dim.Render(detail))
	}
	if m.menu.page == menuRename {
		lines = append(lines, "")
	}
	if remaining := len(rows) - first - visible; remaining > 0 {
		lines = append(lines, pad+dim.Render(fmt.Sprintf("  %d more items", remaining)))
	}
	return lines
}

// listWindow is the range of rows a list page shows with the selected row in view.
func (m model) listWindow(rows int) (first, visible int) {
	room := m.menuBodyRoom()
	if rows > min(maxListItems, room) {
		room--
	} // reserve the remaining-items hint
	visible = max(1, min(rows, maxListItems, room))
	first = min(max(0, m.menu.selected-visible+1), max(0, rows-visible))
	return first, visible
}

// wrapText wraps a text page body to the menu width.
func (m model) wrapText(text string, w int) []string {
	var lines []string
	for _, line := range strings.Split(ansi.Strip(text), "\n") {
		if line == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, strings.Split(ansi.Wrap(line, max(1, w-2), ""), "\n")...)
	}
	return lines
}

func (m model) menuText() string {
	if m.menu.page != menuInfo {
		return m.menu.text
	}
	var lines []string
	for _, row := range m.menu.info {
		lines = append(lines, row.label)
	}
	return strings.Join(lines, "\n")
}

// scrollText scrolls the body of a text page by delta rows.
func (m *model) scrollText(delta int) {
	body := m.wrapText(m.menuText(), m.menuWidth())
	m.menu.offset = min(max(0, m.menu.offset+delta), max(0, len(body)-1))
}

func (m *model) openMenu() tea.Cmd {
	m.menu = &menuState{}
	m.sel, m.inputDrag = nil, false
	return m.fetchModels()
}

func (m model) closeMenu() model {
	m.directoryGen++
	m.menu = nil
	m.refresh(false)
	return m
}

func (m model) menuBack() (tea.Model, tea.Cmd) {
	mu := m.menu
	if mu.page == menuText && mu.parent == menuTracing {
		mu.page, mu.offset, mu.title, mu.text = menuTracing, 0, "", ""
		return m, nil
	}
	if mu.page == menuConfirm {
		mu.page, mu.selected = mu.parent, 0
		return m, nil
	}
	if mu.direct {
		return m.closeMenu(), nil
	}
	switch mu.page {
	case menuMain:
		return m.closeMenu(), nil
	case menuText:
		mu.page, mu.offset, mu.title, mu.text = mu.parent, 0, "", ""
		return m, nil
	case menuEffort:
		mu.page = menuModel
	case menuTask:
		mu.page = menuTasks
	default:
		mu.page = menuMain
	}
	mu.query, mu.search = "", nil
	mu.selected = 0
	return m, nil
}

func (m model) menuKey(v tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.menu.page == menuForm {
		return m.formKey(v)
	}
	mu := m.menu
	n := len(m.menuRows())
	if mu.page == menuText || mu.page == menuInfo {
		switch v.String() {
		case "esc":
			return m.menuBack()
		case "left", "right":
			if mu.page == menuText && mu.parent == menuTracing {
				delta := 1
				if v.String() == "left" {
					delta = -1
				}
				m.moveTrace(delta)
			}
		case "up", "pgup":
			m.scrollText(-max(1, m.menuBodyRoom()/2))
		case "down", "pgdown":
			m.scrollText(max(1, m.menuBodyRoom()/2))
		case "home":
			m.menu.offset = 0
		case "end":
			m.scrollText(1 << 30)
		}
		return m, nil
	}
	switch v.String() {
	case "esc":
		if mu.page == menuConfirm {
			return m.menuBack()
		}
		if mu.direct {
			return m.closeMenu(), nil
		}
		if searchableMenu(mu.page) && mu.query != "" {
			mu.query, mu.selected = "", 0
			return m, nil
		}
		return m.menuBack()
	case "up":
		if mu.page == menuTracing {
			mu.selected = max(0, mu.selected-1)
		} else {
			mu.selected = (mu.selected + n - 1) % n
		}
	case "down":
		if mu.page == menuTracing {
			mu.selected = min(n-1, mu.selected+1)
		} else {
			mu.selected = (mu.selected + 1) % n
		}
	case "enter":
		return m.menuActivate()
	default:
		switch mu.page {
		case menuRename:
			editText(&mu.name, v)
		default:
			if searchableMenu(mu.page) {
				cmd := m.updateMenuSearch(v)
				return m, cmd
			}
		}
	}
	return m, nil
}

func (m model) menuMouse(v tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.menu.page == menuForm {
		return m, nil
	}
	mu := m.menu
	if v.Action != tea.MouseActionPress {
		return m, nil
	}
	if mu.page == menuText || mu.page == menuInfo {
		switch v.Button {
		case tea.MouseButtonWheelUp:
			m.scrollText(-3)
		case tea.MouseButtonWheelDown:
			m.scrollText(3)
		}
		return m, nil
	}
	rows := m.menuRows()
	left, w := m.menuLeft(), m.menuWidth()
	listTop := m.menuBodyStart()
	switch v.Button {
	case tea.MouseButtonWheelUp:
		mu.selected = max(0, mu.selected-1)
	case tea.MouseButtonWheelDown:
		mu.selected = min(max(0, len(rows)-1), mu.selected+1)
	case tea.MouseButtonLeft:
		first, visible := m.listWindow(len(rows))
		if row := v.Y - listTop; v.X >= left && v.X < left+w && row >= 0 && row < visible {
			mu.selected = first + row
			return m.menuActivate()
		}
	}
	return m, nil
}

// menuActivate runs the selected row: it opens a page or an action and closes the menu.
func (m model) menuActivate() (tea.Model, tea.Cmd) {
	mu := m.menu
	index := mu.selected
	rows := m.menuRows()
	if index >= len(rows) {
		return m, nil
	}
	indices := m.menuIndices(m.rawMenuRows())
	if index < 0 || index >= len(indices) {
		return m, nil
	}
	index = indices[index]
	open := func(page menuPage) { mu.page, mu.selected, mu.query, mu.search = page, 0, "", nil }
	switch mu.page {
	case menuMain:
		switch index {
		case 0:
			if m.ctl.Status().Busy {
				return m.reject(fmt.Errorf("Wait for the active turn before resuming a session."))
			}
			mu.sessions, _ = m.ctl.Sessions()
			open(menuResume)
		case 1:
			return m.switchSession("")
		case 2:
			mu.query = ""
			open(menuModel)
			cmd := m.fetchModels()
			return m, cmd
		case 3:
			mu.tasks, _ = m.ctl.Tasks()
			open(menuTasks)
		case 4:
			mu.trace = m.sessionTrace()
			mu.listCache = nil
			mu.parent = menuMain
			open(menuTracing)
			mu.selected = max(0, len(mu.trace)-1)
		case 5:
			mu.name = ""
			open(menuRename)
		case 6:
			info, err := m.ctl.Info()
			if err != nil {
				return m.closeMenu().reject(err)
			}
			mu.info = infoRows(info)
			open(menuInfo)
		case 7:
			m = m.closeMenu()
			m.ctl.Reload()
			m.refresh(true)
		case 8:
			m.openText("Menu/Help", help())
			return m, nil
		case 10:
			return m.editInput()
		case 11:
			m.openProviders()
		case 12:
			open(menuSettings)
		case 13:
			open(menuFiles)
			return m, m.readDirectory(m.cwd)
		case 9:
			m = m.closeMenu()
			return m.quit()
		}
	case menuProviders:
		if index == 0 {
			m.openProviderForm("", false)
		} else if index-1 < len(mu.providerNames) {
			mu.provider = mu.providerNames[index-1]
			open(menuProvider)
		}
	case menuProvider:
		if index == 0 {
			m.openProviderForm(mu.provider, false)
		} else {
			mu.parent, mu.page, mu.selected = menuProvider, menuConfirm, 0
			mu.confirm, mu.target = "Delete provider and saved API key: "+mu.provider, "delete-provider"
			mu.query, mu.search = "", nil
		}
	case menuConfirm:
		if index == 0 {
			return m.menuBack()
		}
		if mu.target == "delete-provider" {
			settings := m.ctl.Settings()
			delete(settings.Providers, mu.provider)
			if settings.Provider == mu.provider {
				settings.Provider, settings.Model = "", ""
			}
			if err := m.ctl.UpdateSettings(settings); err != nil {
				return m.reject(err)
			}
			m.models = nil
			m.modelsAt = time.Time{}
			m.openProviders()
		} else {
			return m.performSwitch(mu.target)
		}
	case menuSettings:
		if index == 3 {
			settings := m.ctl.Settings()
			settings.ReasoningSummary = !settings.ReasoningSummary
			if err := m.ctl.UpdateSettings(settings); err != nil {
				return m.reject(err)
			}
		} else {
			m.openSettingForm(index)
		}
	case menuFiles:
		if index < len(mu.files) {
			f := mu.files[index]
			path := filepath.Join(mu.directory, f.name)
			if f.dir {
				return m, m.readDirectory(path)
			} else {
				return m.editFile(path)
			}
		}
	case menuResume:
		if index < len(mu.sessions) {
			return m.switchSession(mu.sessions[index].ID)
		}
	case menuModel:
		found := m.filteredModels()
		if index == len(m.rawMenuRows())-2 {
			m.modelsAt = time.Time{}
			return m, m.fetchModels()
		}
		if index == len(m.rawMenuRows())-1 {
			m.openManualModel()
			return m, nil
		}
		if index < len(found) {
			mu.provider, mu.model = found[index].Provider, found[index].Model
			open(menuEffort)
			mu.selected = slices.Index(agent.Efforts, "medium")
		}
	case menuEffort:
		m = m.closeMenu()
		return m.submit(m.ctl.SelectModel(mu.provider, mu.model, agent.Efforts[index]))
	case menuTasks:
		if index < len(mu.tasks) {
			mu.task = mu.tasks[len(mu.tasks)-1-index].ID
			open(menuTask)
		}
	case menuTracing:
		if index < len(mu.trace) {
			mu.traceIndex = index
			e := mu.trace[index]
			m.openText("Menu/Tracing/"+e.Kind, traceTitle(e)+"\n\n"+traceText(e))
			return m, nil
		}
	case menuTask:
		if index == 0 {
			output, err := m.ctl.TaskOutput(mu.task)
			if err != nil {
				return m.reject(err)
			}
			m.openText("Menu/Tasks/"+mu.task, output)
			return m, nil
		}
		m = m.closeMenu()
		if err := m.ctl.StopTask(mu.task); err != nil {
			return m.reject(err)
		}
		m.add("notice", "Task "+mu.task+" stopped.")
		m.refresh(true)
		return m, nil
	case menuRename:
		m = m.closeMenu()
		return m.submit(m.ctl.Rename(strings.TrimSpace(mu.name)))
	}
	return m, nil
}

// openText shows a scrollable text page over the current one; Esc returns to it.
func (m *model) openText(title, text string) {
	m.menu.parent, m.menu.page, m.menu.offset = m.menu.page, menuText, 0
	m.menu.title, m.menu.text = title, text
	if m.menu.parent != menuTracing {
		m.menu.query, m.menu.search = "", nil
	}
}

// openMenuPage uses the same page-loading actions as the main menu.
func (m model) openMenuPage(page menuPage) (tea.Model, tea.Cmd) {
	if m.menu != nil && m.menu.page == page {
		return m.closeMenu(), nil
	}
	m.menu = &menuState{selected: map[menuPage]int{menuModel: 2, menuResume: 0, menuTasks: 3, menuTracing: 4, menuInfo: 6, menuFiles: 13}[page]}
	m.sel, m.inputDrag = nil, false
	next, cmd := m.menuActivate()
	result := next.(model)
	if result.menu != nil {
		result.menu.direct = true
	}
	return result, cmd
}

// Session switches stop background tasks; confirmation defaults to Cancel.
func (m model) switchSession(id string) (tea.Model, tea.Cmd) {
	if m.ctl.Status().Tasks > 0 {
		if m.menu == nil {
			m.menu = &menuState{page: menuMain}
		}
		m.menu.parent, m.menu.page, m.menu.selected = m.menu.page, menuConfirm, 0
		m.menu.confirm, m.menu.target = "Stop running background tasks and switch session", id
		m.menu.query, m.menu.search = "", nil
		return m, nil
	}
	return m.performSwitch(id)
}

func (m model) performSwitch(id string) (tea.Model, tea.Cmd) {
	m = m.closeMenu()
	m.resetCompletions()
	if id == "" {
		return m.submit(m.ctl.NewSession())
	}
	return m.submit(m.ctl.Resume(id))
}
