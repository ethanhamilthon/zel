package tui

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"time"

	"zel/internal/core"

	"github.com/gdamore/tcell/v2"
)

type item struct{ label, detail, id string }

// headerID marks the turn rows of Tracing: they are shown, never selected.
const headerID = "turn"

func (it item) header() bool { return it.id == headerID }

type list struct {
	items    []item
	selected int
	query    string
}

func (l *list) visible() []item {
	if l.query == "" {
		return l.items
	}
	// A search shows matching entries only, without the turn rows.
	// Items whose label matches come first, then those that match only with
	// the detail, so a typed name is not shadowed by another item's text.
	var out, byDetail []item
	for _, it := range l.items {
		if it.header() {
			continue
		}
		if fuzzyMatches(it.label, l.query) {
			out = append(out, it)
		} else if fuzzyMatches(it.label+" "+it.detail, l.query) {
			byDetail = append(byDetail, it)
		}
	}
	return append(out, byDetail...)
}

// clamp keeps the selection in the list and off header rows: on the next
// row below a header, or above it at the end.
func (l *list) clamp() {
	v := l.visible()
	l.selected = max(0, min(l.selected, len(v)-1))
	for dir := 1; dir >= -1; dir -= 2 {
		for i := l.selected; i >= 0 && i < len(v); i += dir {
			if !v[i].header() {
				l.selected = i
				return
			}
		}
	}
}
func (l *list) move(delta int) {
	v := l.visible()
	n := len(v)
	for range n {
		l.selected = (l.selected + delta + n) % n
		if !v[l.selected].header() {
			return
		}
	}
}

func (l *list) current() (item, bool) {
	l.clamp()
	v := l.visible()
	if len(v) == 0 {
		return item{}, false
	}
	return v[l.selected], true
}

type menu struct {
	page, title               string
	list                      list
	search                    editor
	body                      string
	offset                    int
	form                      *form
	provider, model           string
	trace                     []core.TraceEntry
	traceIndex                int
	traceList                 list // the Tracing list behind an open entry
	loading                   bool
	refreshed                 time.Time
	x, y, width, top, visible int
}
type pageResult struct {
	target *menu
	items  []item
	body   string
	trace  []core.TraceEntry
}
type modelResult struct {
	models    []core.ModelChoice
	failures  map[string]string
	providers map[string]core.Provider
}

const keyboardHelp = `Ctrl+K: menu · Ctrl+M: models · Ctrl+R: sessions
Ctrl+N: new session · Ctrl+I: info · Ctrl+T: tracing
Ctrl+E: edit the input in your editor ([settings] editor, default nvim)
Enter: send · Shift+Enter / Alt+Enter / Ctrl+J: newline
Ctrl+D: cancel · Ctrl+Q: save and quit · Ctrl+L: redraw
Up on empty input: recall a queued message
Editing a queued message withdraws it
@files and #prompts: arrows select, Enter makes a token
Tab completes and enters directories
PgUp/PgDn: scroll the chat; Up/Down scroll it when the input is one line
Esc: go back or dismiss
Tracing: Tab: all, Lua, agent or errors · Ctrl+H: show the hooks
  Left/Right in an entry: the previous or next of the view

Ctrl shortcuts that overlap Tab/Enter need an enhanced keyboard protocol.
Config: ~/.config/zel/init.lua and ./zel.lua (Ctrl+R in Config reloads).
Commands run with your user permissions.`

var pageTitles = map[string]string{"sessions": "Sessions", "models": "Models", "effort": "Effort", "trace": "Tracing", "config": "Config", "info": "Info", "help": "Help"}

func (m *app) openMenu(page string) {
	m.resetCompletions()
	m.menu = &menu{page: page, title: pageTitles[page]}
	mu := m.menu
	s := m.ctl.Settings()
	switch page {
	case "main":
		mu.title = "Zel " + Version
		mu.list.items = []item{
			{"Resume", "Open a past session", "sessions"},
			{"New session", "Start a clean session", "new"},
			{"Models", "Switch model and effort", "models"},
			{"Tracing", "Inspect every session step", "trace"},
			{"Config", "Files, entries, commands and errors", "config"},
			{"Rename", "Rename this session", "rename"},
			{"Info", "Model, context and spend", "info"},
			{"Help", "Keys and tips", "help"},
			{"Quit", "Save and exit", "quit"},
		}
		if cfg := m.ctl.Config(); cfg != nil {
			for _, cmd := range cfg.Commands {
				mu.list.items = append(mu.list.items, item{label: cmd.Name, detail: commandDetail(cmd), id: "cmd:" + cmd.Name})
			}
		}
	case "models":
		m.modelItems()
		m.fetchModels()
	case "effort":
		for _, effort := range core.Efforts {
			mu.list.items = append(mu.list.items, item{label: effort, id: effort})
			if effort == s.Effort {
				mu.list.selected = len(mu.list.items) - 1
			}
		}
	case "help":
		pairs := []string{}
		for action, key := range core.DefaultKeys {
			binding := key
			if s.Keys != nil {
				binding = s.Keys[action]
			}
			if binding == "" {
				binding = "Disabled"
			}
			pairs = append(pairs, "Ctrl+"+strings.ToUpper(strings.TrimPrefix(key, "ctrl+")), binding)
		}
		mu.body = strings.NewReplacer(pairs...).Replace(keyboardHelp)
	case "trace":
		mu.title = m.traceTitle()
		m.loadPage()
	case "config":
		m.configStatus = ""
		mu.body = configText(m.ctl.Config(), "")
	case "sessions", "info":
		m.loadPage()
	}
	m.dirty = true
}
func (m *app) modelItems() {
	if m.menu == nil || m.menu.page != "models" {
		return
	}
	s := m.ctl.Settings()
	// The current model comes first, so the menu opens on it.
	choices := slices.DeleteFunc(slices.Clone(m.models), func(c core.ModelChoice) bool { return c.Provider == s.Provider && c.Model == s.Model })
	if s.Model != "" {
		choices = append([]core.ModelChoice{{Provider: s.Provider, Model: s.Model}}, choices...)
	}
	items := make([]item, 0, len(choices)+1)
	for _, c := range choices {
		items = append(items, item{c.Model, c.Provider, c.Provider + "\n" + c.Model})
	}
	items = append(items, item{label: "Enter model ID…", id: "manual"})
	m.menu.list.items = items
	m.menu.list.clamp()
}
func (m *app) fetchModels() {
	providers := m.ctl.Settings().Providers
	if !reflect.DeepEqual(providers, m.modelProviders) {
		m.modelProviders = providers
		m.models = nil
		m.modelsAt = time.Time{}
		m.modelsLoading = false
		m.modelItems()
	}
	if m.modelsLoading || len(providers) == 0 || time.Since(m.modelsAt) < 10*time.Minute {
		return
	}
	m.modelsLoading = true
	m.work("models", func() (any, error) {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		models, errs, err := m.ctl.Models(ctx)
		return modelResult{models, errs, providers}, err
	})
}
func (m *app) loadPage() {
	mu := m.menu
	if mu == nil || mu.loading {
		return
	}
	mu.loading = true
	mu.refreshed = time.Now()
	page := mu.page
	m.work("page", func() (any, error) {
		out := pageResult{target: mu}
		switch page {
		case "sessions":
			sessions, err := m.ctl.Sessions()
			if err != nil {
				return out, err
			}
			for _, s := range sessions {
				out.items = append(out.items, item{cmp.Or(s.Title, "(no message)"), s.Updated.Format("Jan 02 15:04"), s.ID})
			}
		case "info":
			info, err := m.ctl.Info()
			if err != nil {
				return out, err
			}
			out.body = infoText(info)
		case "trace":
			out.trace = m.ctl.Trace()
		}
		return out, nil
	})
}
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	r := []rune(cleanInput(line))
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return string(r)
}
func (m *app) acceptMenu(r result) {
	switch r.kind {
	case "models":
		loaded := r.value.(modelResult)
		if !reflect.DeepEqual(loaded.providers, m.modelProviders) {
			return
		}
		m.modelsLoading = false
		m.modelsAt = time.Now()
		if r.err != nil {
			m.notice(r.err)
			return
		}
		m.models = loaded.models
		m.modelItems()
		if len(loaded.failures) > 0 && m.menu != nil && m.menu.page == "models" {
			for name, why := range loaded.failures {
				m.menu.body += name + ": " + why + "\n"
			}
		}
	case "page":
		out := r.value.(pageResult)
		if m.menu != out.target {
			return
		}
		m.menu.loading = false
		if r.err != nil {
			m.menu.body = r.err.Error()
			return
		}
		if m.menu.page == "trace" || m.menu.page == "entry" {
			if m.menu.page == "trace" { // an open entry keeps the trace it was opened from
				m.menu.trace = out.trace
				m.showTrace()
			}
			return
		}
		m.menu.list.items = out.items
		m.menu.list.clamp()
		m.menu.body = out.body
		m.menu.trace = out.trace
	case "operation":
		m.operation = false
		if r.err != nil {
			if m.menu != nil && m.menu.form != nil {
				m.menu.form.err = r.err.Error()
			} else {
				m.notice(r.err)
			}
			return
		}
		page, _ := r.value.(string)
		if page == "" {
			m.menu = nil
		} else {
			m.openMenu(page)
		}
		if page == "models" {
			m.modelsAt = time.Time{}
			m.fetchModels()
		}
	case "reload":
		if m.menu != nil && m.menu.page == "config" {
			if m.configStatus == "Reloading..." {
				cfg := m.ctl.Config()
				switch {
				case m.ctl.Status().Busy:
					m.configStatus = "Reload queued until turn ends"
				case cfg != nil && len(cfg.Errors) > 0:
					m.configStatus = "Error: " + strings.Join(cfg.Errors, "; ")
				default:
					m.configStatus = "Reloaded"
				}
			}
			m.menu.body = configText(m.ctl.Config(), m.configStatus)
		}
	}
}

func commandDetail(cmd core.Command) string {
	switch {
	case cmd.Description != "" && cmd.Key != "":
		return cmd.Description + " (" + cmd.Key + ")"
	case cmd.Description != "":
		return cmd.Description
	default:
		return cmd.Key
	}
}

func (m *app) reloadConfig() {
	if m.menu == nil || m.menu.page != "config" {
		return
	}
	m.configStatus = "Reloading..."
	m.menu.body = configText(m.ctl.Config(), m.configStatus)
	m.work("reload", func() (any, error) {
		m.ctl.Reload()
		return nil, nil
	})
}

func (m *app) menuKey(k *tcell.EventKey) {
	mu := m.menu
	if mu.form != nil {
		m.formKey(k)
		return
	}
	if shortcut(k, m.ctl.Settings().Keys) == "menu" {
		m.menu = nil
		m.updateCompletions()
		return
	}
	switch k.Key() {
	case tcell.KeyEscape:
		if mu.page == "main" {
			m.menu = nil
			m.updateCompletions()
		} else {
			parent := "main"
			switch mu.page {
			case "entry":
				m.closeTraceEntry()
				return
			case "effort":
				parent = "models"
			}
			m.openMenu(parent)
		}
		return
	case tcell.KeyCtrlR:
		if mu.page == "config" {
			m.reloadConfig()
			return
		}
	case tcell.KeyUp:
		if mu.body != "" && len(mu.list.items) == 0 {
			mu.offset = max(0, mu.offset-1)
		} else {
			mu.list.move(-1)
		}
		return
	case tcell.KeyDown:
		if mu.body != "" && len(mu.list.items) == 0 {
			mu.offset++
		} else {
			mu.list.move(1)
		}
		return
	case tcell.KeyPgUp:
		mu.offset = max(0, mu.offset-10)
		mu.list.selected = max(0, mu.list.selected-10)
		return
	case tcell.KeyPgDn:
		mu.offset += 10
		mu.list.selected += 10
		mu.list.clamp()
		return
	case tcell.KeyLeft, tcell.KeyRight:
		if mu.page == "entry" {
			delta := 1
			if k.Key() == tcell.KeyLeft {
				delta = -1
			}
			m.stepTraceEntry(delta)
			return
		}
	case tcell.KeyTab, tcell.KeyBacktab:
		if mu.page == "trace" {
			delta := 1
			if k.Key() == tcell.KeyBacktab {
				delta = len(traceFilters) - 1
			}
			m.traceFilter = (m.traceFilter + delta) % len(traceFilters)
			m.showTrace()
			return
		}
	case tcell.KeyCtrlH:
		if mu.page == "trace" {
			shown := !m.traceHooksShown()
			m.traceHooks = &shown
			m.showTrace()
			return
		}
	case tcell.KeyEnter:
		if it, ok := mu.list.current(); ok {
			m.activate(it)
		}
		return
	}
	if len(mu.list.items) > 0 {
		before := mu.search.text
		mu.search.key(k, max(1, mu.width-2), false)
		if before != mu.search.text {
			mu.list.query = mu.search.text
			mu.list.selected = 0
		}
	}
}
