package tui

import (
	"fmt"
	"slices"
	"strings"

	"zel/internal/core"
)

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
		for _, effort := range core.Efforts {
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
