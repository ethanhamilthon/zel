package tui

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"zel/internal/core"
)

type formState struct {
	kind           string
	original       string
	onboarding     bool
	step           int
	fields, values []string
	input          textarea.Model
	err            string
	setting        int
}

func (m *model) startForm(f *formState) {
	f.input = newComposer()
	f.input.KeyMap.InsertNewline.SetEnabled(false)
	f.input.Placeholder = ""
	f.input.SetValue(f.values[0])
	m.menu.page, m.menu.form = menuForm, f
}

func (m *model) openProviderForm(name string, onboarding bool) {
	p := m.ctl.Settings().Providers[name]
	p.BaseURL = cmp.Or(p.BaseURL, "https://api.openai.com/v1")
	p.API = cmp.Or(p.API, core.APIResponses)
	m.startForm(&formState{kind: "provider", original: name, onboarding: onboarding, fields: []string{"Name", "Base URL", "API key", "API type (responses / chat)"}, values: []string{name, p.BaseURL, p.APIKey, p.API}})
}
func (m *model) openProviders() {
	m.menu.page, m.menu.selected, m.menu.query, m.menu.search = menuProviders, 0, "", nil
	m.menu.providerNames = slices.Sorted(maps.Keys(m.ctl.Settings().Providers))
}
func (m *model) openSettingForm(index int) {
	s := m.ctl.Settings()
	labels := []string{"Editor", "Output limit in bytes (0 = unlimited)", "Bash timeout in seconds (1–600)"}
	values := []string{s.UI.Editor, strconv.Itoa(s.ShellOutput), strconv.Itoa(s.ShellTimeout)}
	m.startForm(&formState{kind: "setting", setting: index, fields: []string{labels[index]}, values: []string{values[index]}})
}
func (m *model) openManualModel() {
	names := slices.Sorted(maps.Keys(m.ctl.Settings().Providers))
	provider := m.ctl.Settings().Provider
	if provider == "" && len(names) > 0 {
		provider = names[0]
	}
	m.startForm(&formState{kind: "model", fields: []string{"Provider", "Model ID"}, values: []string{provider, ""}})
}

func (m model) formKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.menu.form
	switch key.String() {
	case "esc":
		if f.onboarding {
			return m, nil
		}
		switch f.kind {
		case "provider":
			m.openProviders()
		case "model":
			m.menu.page = menuModel
		default:
			m.menu.page = menuSettings
		}
		m.menu.form = nil
		return m, nil
	case "tab", "shift+tab", "enter":
		f.values[f.step] = strings.TrimSpace(f.input.Value())
		if key.String() == "enter" {
			if err := validateFormField(f); err != nil {
				f.err = err.Error()
				return m, nil
			}
			if f.step == len(f.fields)-1 {
				return m.saveForm()
			}
		}
		delta := 1
		if key.String() == "shift+tab" {
			delta = -1
		}
		f.step = (f.step + delta + len(f.fields)) % len(f.fields)
		f.input.SetValue(f.values[f.step])
		f.input.CursorEnd()
		f.err = ""
		return m, nil
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(key)
	f.err = ""
	return m, cmd
}

func validateFormField(f *formState) error {
	value := f.values[f.step]
	if f.kind == "provider" {
		switch f.step {
		case 0:
			if value == "" {
				return fmt.Errorf("Provider name is required")
			}
		case 1:
			u, err := url.Parse(value)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("Enter an HTTP(S) base URL")
			}
		case 3:
			if value != core.APIResponses && value != core.APIChat {
				return fmt.Errorf("Choose responses or chat")
			}
		}
	} else if f.kind == "model" && value == "" {
		return fmt.Errorf("%s is required", f.fields[f.step])
	}
	return nil
}

func (m model) saveForm() (tea.Model, tea.Cmd) {
	f := m.menu.form
	// Tab navigation can skip fields; validate all of them at save time.
	for i := range f.fields {
		f.step = i
		if err := validateFormField(f); err != nil {
			f.err = err.Error()
			f.input.SetValue(f.values[i])
			return m, nil
		}
	}
	s := m.ctl.Settings()
	switch f.kind {
	case "provider":
		name := f.values[0]
		if _, exists := s.Providers[name]; exists && name != f.original {
			f.step = 0
			f.input.SetValue(name)
			f.err = "A provider with this name already exists"
			return m, nil
		}
		if f.original != "" && f.original != name {
			delete(s.Providers, f.original)
			if s.Provider == f.original {
				s.Provider = name
			}
		}
		s.Providers[name] = core.Provider{BaseURL: f.values[1], APIKey: f.values[2], API: f.values[3]}
	case "setting":
		if f.setting == 0 {
			s.UI.Editor = f.values[0]
		} else {
			n, err := strconv.Atoi(f.values[0])
			if err != nil {
				f.err = "Enter a whole number"
				return m, nil
			}
			if f.setting == 1 {
				s.ShellOutput = n
			} else {
				s.ShellTimeout = n
			}
		}
	case "model":
		if _, ok := s.Providers[f.values[0]]; !ok {
			f.step = 0
			f.input.SetValue(f.values[0])
			f.err = "Provider not found"
			return m, nil
		}
		m.menu.provider, m.menu.model = f.values[0], f.values[1]
		m.menu.page, m.menu.form, m.menu.query, m.menu.search, m.menu.selected = menuEffort, nil, "", nil, slices.Index(core.Efforts, "medium")
		return m, nil
	}
	if err := m.ctl.UpdateSettings(s); err != nil {
		f.err = err.Error()
		return m, nil
	}
	m.menu.form = nil
	if f.kind == "provider" {
		m.models = nil
		m.menu.page, m.menu.query, m.menu.search, m.menu.selected = menuModel, "", nil, 0
		return m, m.fetchModels()
	}
	m.menu.page = menuSettings
	return m, nil
}

func (m model) formView() string {
	f := m.menu.form
	w := m.menuWidth()
	pad := strings.Repeat(" ", m.menuLeft())
	title := "Add provider"
	subtitle := "Configure a provider"
	if f.original != "" {
		title = "Edit provider"
	}
	if f.onboarding {
		title = "Welcome to Zel " + strings.TrimPrefix(Version, "v")
		subtitle = "Let's add your first provider to get started"
	}
	if f.kind == "setting" {
		title, subtitle = "Settings", "Changes are saved in the database"
	}
	if f.kind == "model" {
		title, subtitle = "Choose a model", "Enter a provider and model ID"
	}
	f.input.SetWidth(max(1, w-2))
	f.input.SetHeight(1)
	view := f.input.View()
	if f.kind == "provider" && f.step == 2 {
		// Mask the key while retaining the native cursor marker and its position.
		raw := ansi.Strip(view)
		marker := strings.Index(raw, composerCursorMarker)
		if marker >= 0 {
			before, after := raw[:marker], raw[marker+len(composerCursorMarker):]
			view = strings.Repeat("•", len([]rune(strings.TrimRight(before, " ")))) + composerCursorMarker + strings.Repeat("•", len([]rune(strings.TrimRight(after, " "))))
		} else {
			view = strings.Repeat("•", len([]rune(f.input.Value())))
		}
	}
	lines := make([]string, max(0, (m.height-9)/2))
	center := func(text string) string {
		text = ansi.Truncate(text, w, "…")
		return pad + strings.Repeat(" ", max(0, (w-ansi.StringWidth(text))/2)) + text
	}
	lines = append(lines, center(lipgloss.NewStyle().Foreground(inputBlue).Render(title)), center(subtitle), "", pad+fmt.Sprintf("%-*s %d/%d", max(1, w-5), f.fields[f.step], f.step+1, len(f.fields)), "", pad+renderRail(view, w, false), "")
	if f.err != "" {
		lines = append(lines, pad+lipgloss.NewStyle().Foreground(errorColor).Render(ansi.Truncate(f.err, w, "…")))
	}
	for len(lines) < m.height-2 {
		lines = append(lines, "")
	}
	lines = append(lines, center("Tab / Shift+Tab: step · Enter: next"), center("← / →: cursor · Ctrl+Q: quit"))
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	row, col := 0, 0
	for i := range lines {
		if at := strings.Index(lines[i], composerCursorMarker); at >= 0 {
			row, col = i+1, ansi.StringWidth(lines[i][:at])+1
		}
		lines[i] = strings.ReplaceAll(lines[i], composerCursorMarker, "")
	}
	return strings.Join(lines[:m.height], "\n") + fmt.Sprintf("\x1b]1337;ZelCaret;%d;%d\x07", row, col)
}
