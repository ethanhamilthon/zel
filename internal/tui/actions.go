package tui

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// form is a page with one or two text fields: rename and a model ID.
type form struct {
	kind           string
	fields, values []string
	step           int
	input          editor
	err            string
}

func (m *app) startForm(title string, f *form) {
	m.menu = &menu{page: "form", title: title, form: f}
	f.input.set(f.values[0])
	m.dirty = true
}
func (m *app) manualModel() {
	s := m.ctl.Settings()
	name := s.Provider
	if names := slices.Sorted(maps.Keys(s.Providers)); name == "" && len(names) > 0 {
		name = names[0]
	}
	m.startForm("Model ID", &form{kind: "model", fields: []string{"Provider", "Model ID"}, values: []string{name, ""}})
}
func (m *app) operationTo(page string, fn func() error) {
	if m.operation {
		return
	}
	m.operation = true
	m.work("operation", func() (any, error) { return page, fn() })
}
func (m *app) switchSession(id string) {
	m.cancelPrompt()
	page := ""
	if id == "" {
		page = "models"
	}
	m.operationTo(page, func() error {
		if id == "" {
			return m.ctl.NewSession()
		}
		return m.ctl.Resume(id)
	})
}
func (m *app) runCommand(name string) {
	if err := m.ctl.RunCommand(name); err != nil {
		m.notice(err)
	}
}

func (m *app) activate(it item) {
	mu := m.menu
	switch mu.page {
	case "main":
		if name, ok := strings.CutPrefix(it.id, "cmd:"); ok {
			m.menu = nil
			m.updateCompletions()
			m.runCommand(name)
			return
		}
		switch it.id {
		case "new":
			m.switchSession("")
		case "quit":
			closed, err := m.ctl.Quit()
			m.notice(err)
			if closed {
				m.cancel()
			}
		case "rename":
			s, _ := m.ctl.Session()
			m.startForm("Rename", &form{kind: "rename", fields: []string{"Session title"}, values: []string{s.Title}})
		default:
			m.openMenu(it.id)
		}
	case "sessions":
		m.switchSession(it.id)
	case "models":
		if it.id == "manual" {
			m.manualModel()
			return
		}
		provider, model, _ := strings.Cut(it.id, "\n")
		m.openMenu("effort")
		m.menu.provider = provider
		m.menu.model = model
	case "effort":
		provider, model := mu.provider, mu.model
		m.operationTo("", func() error { return m.ctl.SelectModel(provider, model, it.id) })
	case "trace":
		if i, err := strconv.Atoi(it.id); err == nil && !it.header() {
			m.openTraceEntry(i)
		}
	}
}
func (m *app) formKey(k *tcell.EventKey) {
	f := m.menu.form
	width, _ := m.screen.Size()
	switch k.Key() {
	case tcell.KeyEscape:
		if f.kind == "model" {
			m.openMenu("models")
		} else {
			m.openMenu("main")
		}
	case tcell.KeyTab, tcell.KeyBacktab, tcell.KeyEnter:
		f.values[f.step] = strings.TrimSpace(f.input.text)
		if k.Key() == tcell.KeyEnter {
			if err := m.checkField(f, f.step); err != "" {
				f.err = err
				return
			}
			if f.step == len(f.fields)-1 {
				m.saveForm()
				return
			}
		}
		delta := 1
		if k.Key() == tcell.KeyBacktab {
			delta = -1
		}
		f.step = (f.step + delta + len(f.fields)) % len(f.fields)
		f.input.set(f.values[f.step])
		f.err = ""
	default:
		f.input.key(k, max(1, width-4), false)
		f.err = ""
	}
}
func (m *app) saveForm() {
	f := m.menu.form
	for i := range f.fields {
		if err := m.checkField(f, i); err != "" {
			f.step = i
			f.input.set(f.values[i])
			f.err = err
			return
		}
	}
	switch f.kind {
	case "rename":
		title := f.values[0]
		m.operationTo("", func() error { return m.ctl.Rename(title) })
	case "model":
		provider, model := f.values[0], f.values[1]
		m.openMenu("effort")
		m.menu.provider = provider
		m.menu.model = model
	}
}

// checkField returns why the value of field i cannot be used, or "".
func (m *app) checkField(f *form, i int) string {
	switch {
	case f.kind == "model" && f.values[i] == "":
		return f.fields[i] + " is required"
	case f.kind == "model" && i == 0:
		if _, ok := m.ctl.Settings().Providers[f.values[0]]; !ok {
			return "Provider not found"
		}
	}
	return ""
}
