package tui

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"zel/internal/core"
)

type item struct{ label, detail, id string }
type list struct {
	items    []item
	selected int
	query    string
}

func (l *list) visible() []item {
	if l.query == "" {
		return l.items
	}
	var out []item
	for _, it := range l.items {
		if fuzzyMatches(it.label+" "+it.detail, l.query) {
			out = append(out, it)
		}
	}
	return out
}
func (l *list) clamp() { l.selected = max(0, min(l.selected, len(l.visible())-1)) }
func (l *list) move(delta int) {
	n := len(l.visible())
	if n > 0 {
		l.selected = (l.selected + delta + n) % n
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

type menuRow struct{ label, detail string }
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
Enter: send · Shift+Enter / Alt+Enter / Ctrl+J: newline
Ctrl+D: cancel · Ctrl+Q: save and quit · Ctrl+L: redraw
Ctrl+V: paste text or an image
Up on empty input: recall a queued message
Editing a queued message withdraws it
@files and #prompts: arrows select, Tab or Enter completes
PgUp/PgDn or mouse wheel: scroll
Drag chat text to select; Ctrl+C copies
Shift+drag: native terminal selection
Ctrl+click: open a link
Esc: go back or dismiss

Ctrl shortcuts that overlap Tab/Enter need an enhanced keyboard protocol.
Configuration reloads automatically. Commands run with your user permissions.`

func (m *app) openMenu(page string) {
	m.resetCompletions()
	m.menu = &menu{page: page, title: "Menu / " + page}
	mu := m.menu
	s := m.ctl.Settings()
	switch page {
	case "main":
		mu.title = "Zel " + Version
		mu.list.items = []item{{"Resume", "Ctrl+R", "sessions"}, {"New session", "Ctrl+N", "new"}, {"Models", "Ctrl+M", "models"}, {"Tracing", "Ctrl+T", "trace"}, {"Rename", "", "rename"}, {"Info", "Ctrl+I", "info"}, {"Help", "", "help"}, {"Providers", "Add, edit, remove", "providers"}, {"Settings", "", "settings"}, {"Quit", "Ctrl+Q", "quit"}}
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
	case "providers":
		for _, name := range slices.Sorted(maps.Keys(s.Providers)) {
			mu.list.items = append(mu.list.items, item{name, s.Providers[name].BaseURL, name})
		}
		mu.list.items = append(mu.list.items, item{label: "Add provider…", id: "add"})
	case "settings":
		m.settingItems()
	case "help":
		mu.body = keyboardHelp
	case "sessions", "trace", "info":
		m.loadPage()
	}
	m.dirty = true
}
func (m *app) modelItems() {
	if m.menu == nil || m.menu.page != "models" {
		return
	}
	s := m.ctl.Settings()
	choices := slices.Clone(m.models)
	if s.Model != "" && !slices.ContainsFunc(choices, func(c core.ModelChoice) bool { return c.Provider == s.Provider && c.Model == s.Model }) {
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
	if !maps.Equal(providers, m.modelProviders) {
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
			for _, r := range infoRows(info) {
				out.body += r.label + " " + r.detail + "\n"
			}
		case "trace":
			out.trace = m.ctl.Trace()
			for i, t := range out.trace {
				out.items = append(out.items, item{t.Kind + "  " + firstLine(t.Text), t.At.Format("15:04:05"), fmt.Sprint(i)})
			}
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
		if !maps.Equal(loaded.providers, m.modelProviders) {
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
	}
}
func (m *app) menuKey(k *tcell.EventKey) {
	mu := m.menu
	if mu.form != nil {
		m.formKey(k)
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
				parent = "trace"
			case "provider", "confirm":
				parent = "providers"
			case "accent":
				parent = "settings"
			case "effort":
				parent = "models"
			}
			m.openMenu(parent)
		}
		return
	case tcell.KeyCtrlK:
		m.menu = nil
		return
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
		if mu.page == "entry" && len(mu.trace) > 0 {
			delta := 1
			if k.Key() == tcell.KeyLeft {
				delta = -1
			}
			mu.traceIndex = max(0, min(mu.traceIndex+delta, len(mu.trace)-1))
			mu.body = mu.trace[mu.traceIndex].Text
			mu.title = "Tracing / " + mu.trace[mu.traceIndex].Kind
			mu.offset = 0
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
