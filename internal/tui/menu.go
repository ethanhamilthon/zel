package tui

import (
	"github.com/charmbracelet/bubbles/textarea"

	"zel/internal/core"
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
	sessions        []core.SessionSummary
	info            []menuRow
	tasks           []core.Task
	trace           []core.TraceEntry
}

// File and trace sources change only when refreshed, not on cursor movement.
