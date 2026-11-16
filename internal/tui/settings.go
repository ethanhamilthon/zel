package tui

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"zel/internal/config"
	"zel/internal/core"
)

type form struct {
	kind, original string
	fields, values []string
	step, setting  int
	input          editor
	err            string
}

func (m *app) startForm(f *form) {
	m.menu = &menu{page: "form", title: "Menu / " + f.kind, form: f}
	f.input.set(f.values[0])
	m.dirty = true
}
func (m *app) providerForm(name string) {
	p := m.ctl.Settings().Providers[name]
	m.startForm(&form{kind: "provider", original: name, fields: []string{"API type (responses / chat)", "Base URL", "API key", "Name"}, values: []string{cmp.Or(p.API, core.APIResponses), cmp.Or(p.BaseURL, "https://api.openai.com/v1"), p.APIKey, name}})
}
func (m *app) manualModel() {
	s := m.ctl.Settings()
	name := s.Provider
	if name == "" {
		names := slices.Sorted(maps.Keys(s.Providers))
		if len(names) > 0 {
			name = names[0]
		}
	}
	m.startForm(&form{kind: "model", fields: []string{"Provider", "Model ID"}, values: []string{name, ""}})
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
	m.operationTo("", func() error {
		if id == "" {
			return m.ctl.NewSession()
		}
		return m.ctl.Resume(id)
	})
}
func (m *app) activate(it item) {
	mu := m.menu
	switch mu.page {
	case "main":
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
			m.startForm(&form{kind: "rename", fields: []string{"Session title"}, values: []string{s.Title}})
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
		i, _ := strconv.Atoi(it.id)
		if i >= len(mu.trace) {
			return
		}
		mu.page = "entry"
		mu.traceIndex = i
		mu.title = "Tracing / " + mu.trace[i].Kind
		mu.body = mu.trace[i].Text
		mu.list = list{}
		mu.offset = 0
	case "providers":
		if it.id == "add" {
			m.providerForm("")
			return
		}
		m.menu = &menu{page: "provider", title: "Provider / " + it.id, provider: it.id, list: list{items: []item{{label: "Edit", id: "edit"}, {label: "Remove", id: "remove"}}}}
	case "provider":
		if it.id == "edit" {
			m.providerForm(mu.provider)
			return
		}
		name := mu.provider
		m.menu = &menu{page: "confirm", title: "Remove provider " + name + "?", provider: name, list: list{items: []item{{label: "Cancel", id: "no"}, {label: "Remove", id: "yes"}}}}
	case "confirm":
		if it.id != "yes" {
			m.openMenu("providers")
			return
		}
		s := m.ctl.Settings()
		delete(s.Providers, mu.provider)
		if s.Provider == mu.provider {
			s.Provider = ""
			s.Model = ""
			s.Effort = ""
		}
		m.operationTo("providers", func() error { return m.ctl.UpdateSettings(s) })
		m.models = nil
		m.modelsAt = time.Time{}
	case "settings":
		m.changeSetting(it.id)
	case "accent":
		s := m.ctl.Settings()
		s.Colors.Set("accent", it.id)
		m.operationTo("settings", func() error { return m.ctl.UpdateSettings(s) })
	}
}
func (m *app) settingItems() {
	s := m.ctl.Settings()
	m.menu.list.items = []item{
		{"Auto compaction", strconv.Itoa(s.CompactionThreshold), "compaction"},
		{"Reasoning summary", strconv.FormatBool(s.ReasoningSummary), "reasoning"},
		{"Slash opens menu", strconv.FormatBool(s.UI.SlashMenu), "slash"},
		{"Accent", accentName(s.Colors.Get("accent")), "accent"},
		{"Bash timeout", fmt.Sprintf("%d s", s.BashTimeout), "timeout"},
		{"Bash output", strconv.Itoa(s.BashOutput), "output"},
		{"Show bash", strconv.FormatBool(s.UI.ShowBash), "show_bash"},
	}
}
func (m *app) changeSetting(id string) {
	s := m.ctl.Settings()
	switch id {
	case "accent":
		m.menu = &menu{page: "accent", title: "Settings / Accent"}
		for _, a := range accentColors {
			m.menu.list.items = append(m.menu.list.items, item{a.name, a.hex, a.hex})
		}
		return
	case "reasoning":
		s.ReasoningSummary = !s.ReasoningSummary
	case "slash":
		s.UI.SlashMenu = !s.UI.SlashMenu
	case "show_bash":
		s.UI.ShowBash = !s.UI.ShowBash
	default:
		value, field, index := s.CompactionThreshold, "Auto compaction tokens (0 = off)", 0
		if id == "timeout" {
			value, field, index = s.BashTimeout, "Bash timeout in seconds", 1
		}
		if id == "output" {
			value, field, index = s.BashOutput, "Bash output bytes (0 = off)", 2
		}
		m.startForm(&form{kind: "setting", setting: index, fields: []string{field}, values: []string{strconv.Itoa(value)}})
		return
	}
	m.operationTo("settings", func() error { return m.ctl.UpdateSettings(s) })
}
func (m *app) formKey(k *tcell.EventKey) {
	f := m.menu.form
	width, _ := m.screen.Size()
	switch k.Key() {
	case tcell.KeyEscape:
		switch f.kind {
		case "provider":
			m.openMenu("providers")
		case "model":
			m.openMenu("models")
		case "setting":
			m.openMenu("settings")
		default:
			m.openMenu("main")
		}
		return
	case tcell.KeyTab, tcell.KeyBacktab, tcell.KeyEnter:
		f.values[f.step] = strings.TrimSpace(f.input.text)
		if k.Key() == tcell.KeyEnter {
			if err := validateField(f, f.step); err != nil {
				f.err = err.Error()
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
		if err := validateField(f, i); err != nil {
			f.step = i
			f.input.set(f.values[i])
			f.err = err.Error()
			return
		}
	}
	s := m.ctl.Settings()
	switch f.kind {
	case "rename":
		title := f.values[0]
		m.operationTo("", func() error { return m.ctl.Rename(title) })
		return
	case "model":
		if _, ok := s.Providers[f.values[0]]; !ok {
			f.err = "Provider not found"
			return
		}
		provider, model := f.values[0], f.values[1]
		m.openMenu("effort")
		m.menu.provider = provider
		m.menu.model = model
		return
	case "provider":
		name := f.values[3]
		if _, exists := s.Providers[name]; exists && name != f.original {
			f.step = 3
			f.input.set(name)
			f.err = "A provider with this name already exists"
			return
		}
		if f.original != "" && f.original != name {
			delete(s.Providers, f.original)
			if s.Provider == f.original {
				s.Provider = name
			}
		}
		s.Providers[name] = core.Provider{API: f.values[0], BaseURL: f.values[1], APIKey: f.values[2]}
		if err := config.Validate(s); err != nil {
			f.err = err.Error()
			return
		}
		m.models = nil
		m.modelsAt = time.Time{}
		m.operationTo("models", func() error { return m.ctl.UpdateSettings(s) })
		return
	case "setting":
		raw := f.values[0]
		if strings.EqualFold(raw, "off") {
			raw = "0"
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			f.err = "Enter a whole number"
			return
		}
		switch f.setting {
		case 0:
			s.CompactionThreshold = n
		case 1:
			s.BashTimeout = n
		case 2:
			s.BashOutput = n
		}
		if err := config.Validate(s); err != nil {
			f.err = err.Error()
			return
		}
		m.operationTo("settings", func() error { return m.ctl.UpdateSettings(s) })
	}
}

func validateField(f *form, i int) error {
	value := f.values[i]
	if f.kind == "provider" {
		switch i {
		case 0:
			if value != core.APIResponses && value != core.APIChat {
				return fmt.Errorf("Choose responses or chat")
			}
		case 1:
			u, err := url.Parse(value)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("Enter an HTTP(S) base URL")
			}
		case 3:
			if value == "" {
				return fmt.Errorf("Provider name is required")
			}
		}
	}
	if f.kind == "model" && value == "" {
		return fmt.Errorf("%s is required", f.fields[i])
	}
	return nil
}
