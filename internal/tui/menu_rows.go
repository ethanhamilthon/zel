package tui

import (
	"fmt"
	"slices"
	"strconv"
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
	case menuProviders:
		for _, name := range mu.providerNames {
			rows = append(rows, menuRow{name, m.ctl.Settings().Providers[name].BaseURL})
		}
	case menuConfirm:
		rows = []menuRow{{"Cancel", "Keep everything as it is"}, {"Confirm", mu.confirm}}
	case menuProvider:
		rows = []menuRow{{"Edit provider", mu.provider}, {"Delete provider", ""}}
	case menuSettings:
		s := m.ctl.Settings()
		rows = []menuRow{{"Editor", s.UI.Editor}, {"Output limit", fmt.Sprintf("%d bytes", s.ShellOutput)}, {"Bash timeout", fmt.Sprintf("%d seconds", s.ShellTimeout)}, {"Reasoning summary", fmt.Sprint(s.ReasoningSummary)}, {"Slash opens menu", fmt.Sprint(s.UI.SlashMenu)}}
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
	case menuCreate:
		detail := "end with / for directory"
		if mu.status != "" {
			detail = mu.status
		}
		rows = []menuRow{{mu.name + "▏", detail}}
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
	types, lower, upper, textQuery := parseTraceFilters(query)
	if m.menu.page == menuTracing {
		query = textQuery
	}
	for i, row := range rows {
		if m.menu.page == menuTracing {
			if i >= len(m.menu.trace) {
				if len(types) > 0 || lower > 0 || upper > 0 {
					continue
				}
			} else {
				e := m.menu.trace[i]
				position := i + 1
				if (len(types) > 0 && !traceTypeIn(e.Kind, types)) || (lower > 0 && position < lower) || (upper > 0 && position > upper) {
					continue
				}
			}
		}
		matches := true
		switch {
		case !searchableMenu(m.menu.page), m.menu.page == menuModel:
			matches = true
		default:
			matches = menuLabelMatches(row.label, query)
		}
		if matches {
			indices = append(indices, i)
		}
	}
	return indices
}

func parseTraceFilters(query string) (types []string, lower, upper int, text string) {
	var terms []string
	for _, term := range strings.Fields(query) {
		if term[0] == ':' && len(term) > 1 {
			types = append(types, strings.Split(strings.ToLower(term[1:]), "+")...)
			continue
		}
		if len(term) > 1 && (term[0] == '>' || term[0] == '<') {
			if n, err := strconv.Atoi(term[1:]); err == nil {
				if term[0] == '>' {
					lower = max(lower, n)
				} else if upper == 0 || n < upper {
					upper = n
				}
				continue
			}
		}
		terms = append(terms, term)
	}
	return types, lower, upper, strings.Join(terms, " ")
}

func traceTypeIn(kind string, filters []string) bool {
	return slices.ContainsFunc(filters, func(filter string) bool { return traceKindMatches(kind, filter) })
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
