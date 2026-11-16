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
	menuTracing
	menuRename
	menuInfo
	menuHelp
	menuText
	menuProviders
	menuProvider
	menuForm
	menuSettings
	menuConfirm
	menuAccent
)

var menuTitles = map[menuPage]string{
	menuMain: "Menu", menuResume: "Menu/Resume", menuModel: "Menu/Models", menuEffort: "Menu/Models/Effort",
	menuTracing:   "Menu/Tracing",
	menuProviders: "Menu/Providers", menuProvider: "Menu/Providers/Provider", menuForm: "Menu/Settings/Edit", menuSettings: "Menu/Settings", menuAccent: "Menu/Settings/Accent", menuRename: "Menu/Rename", menuInfo: "Menu/Info", menuHelp: "Menu/Help",
}

type menuRow struct{ label, detail string }

var mainRows = []menuRow{
	{"Resume", "View all sessions"}, {"New", "Or just Ctrl+N"}, {"Models", "And Efforts, Ctrl+M"},
	{"Tracing", "Session Details, Ctrl+T"}, {"Rename", "Rename this session"},
	{"Info", "Tokens, Session, Ctrl+I"}, {"Help", "Keyboard shortcuts"},
	{"Quit", "Exit, Ctrl+Q"},
	{"Providers", "Add, edit, remove"}, {"Settings", "Bash timeout and output, compaction, accent"},
}

type menuListCache struct {
	page     menuPage
	count    int
	rev      uint64
	rows     []menuRow
	query    string
	indices  []int
	filtered []menuRow
}

type menuState struct {
	listCache       *menuListCache
	layout          *textLayout // the laid-out body of the open text page
	form            *formState
	providerNames   []string
	direct          bool // opened by a shortcut, not through Menu
	traceIndex      int
	page            menuPage
	selected        int
	offset          int      // first row of a text page
	parent          menuPage // where Esc returns from a text page
	title, text     string   // the title and body of a text page
	provider, model string   // the model chosen on the Model page, waiting for an effort
	name            string   // the Rename text
	query           string   // the search text of the current page
	search          *textarea.Model
	status          string
	confirm         string
	target          string
	sessions        []core.SessionSummary
	info            []menuRow
	trace           []core.TraceEntry
	rev             uint64   // changes whenever a snapshot replaced the data above
	ctx             []string // searchable text of the trace entries
	ctxRev          uint64
	loading, loaded bool // a background snapshot is in flight / one was applied
	wasActive       bool // the session was active at the last snapshot
}

// File and trace sources change only when refreshed, not on cursor movement.
