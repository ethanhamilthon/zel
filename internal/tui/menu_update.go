package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"zel/internal/core"
)

func (m *model) openMenu() tea.Cmd {
	m.menu = &menuState{}
	return m.fetchModels()
}

func (m model) closeMenu() model {
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
	case menuAccent:
		mu.page, mu.selected = menuSettings, 3
		return m, nil
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
		case "up", "down":
			step := max(1, m.menuBodyRoom()/2)
			if m.traceEntryView() {
				step = 1
			}
			if v.String() == "up" {
				step = -step
			}
			m.scrollText(step)
		case "pgup":
			m.scrollText(-max(1, m.menuBodyRoom()/2))
		case "pgdown":
			m.scrollText(max(1, m.menuBodyRoom()/2))
		case "home":
			m.menu.offset = 0
		case "end":
			m.scrollText(1 << 30)
		}
		return m, nil
	}
	switch v.String() {
	case "ctrl+a":
		switch mu.page {
		case menuProviders:
			m.openProviderForm("", false)
			return m, nil
		}
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
			mu.trace = m.sessionTrace()
			mu.listCache = nil
			mu.parent = menuMain
			open(menuTracing)
			mu.selected = max(0, len(mu.trace)-1)
		case 4:
			mu.name = ""
			open(menuRename)
		case 5:
			info, err := m.ctl.Info()
			if err != nil {
				return m.closeMenu().reject(err)
			}
			mu.info = infoRows(info)
			open(menuInfo)
		case 6:
			m = m.closeMenu()
			m.ctl.Reload()
			m.refresh(true)
		case 7:
			m.openText("Menu/Help", help())
			return m, nil
		case 9:
			m.openProviders()
		case 10:
			open(menuSettings)
		case 11:
			return m.closeMenu().copyAnswer()
		case 8:
			m = m.closeMenu()
			return m.quit()
		}
	case menuProviders:
		if index < 0 || index >= len(mu.providerNames) {
			return m, nil
		}
		mu.provider = mu.providerNames[index]
		open(menuProvider)
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
			open(menuAccent)
			mu.selected = accentIndex(m.ctl.Settings().UI.Accent)
		} else if index == 1 || index == 2 {
			settings := m.ctl.Settings()
			if index == 1 {
				settings.ReasoningSummary = !settings.ReasoningSummary
			} else {
				settings.UI.SlashMenu = !settings.UI.SlashMenu
			}
			if err := m.ctl.UpdateSettings(settings); err != nil {
				return m.reject(err)
			}
		} else {
			m.openSettingForm(index)
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
			mu.selected = slices.Index(core.Efforts, "medium")
		}
	case menuAccent:
		if index >= 0 && index < len(accentColors) {
			settings := m.ctl.Settings()
			settings.UI.Accent = accentColors[index].hex
			if index == 0 {
				settings.UI.Accent = ""
			}
			if err := m.ctl.UpdateSettings(settings); err != nil {
				return m.reject(err)
			}
			mu.page, mu.selected = menuSettings, 3
			m.refresh(false)
		}
	case menuEffort:
		m = m.closeMenu()
		if index >= 0 && index < len(core.Efforts) {
			return m.submit(m.ctl.SelectModel(mu.provider, mu.model, core.Efforts[index]))
		}
		return m, nil
	case menuTracing:
		if index < len(mu.trace) {
			m.openTrace(index)
			return m, tea.EnableMouseCellMotion
		}
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
	m.menu = &menuState{selected: map[menuPage]int{menuModel: 2, menuResume: 0, menuTracing: 3, menuInfo: 5}[page]}
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
