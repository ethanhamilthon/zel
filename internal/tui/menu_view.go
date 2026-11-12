package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"zel/internal/core"
)

func (m model) menuView() string {
	if m.menu.page == menuForm {
		return m.formView()
	}
	pad, w := strings.Repeat(" ", m.menuLeft()), m.menuWidth()
	title, shortcut := m.menuHead()
	lines := make([]string, 0, m.height)
	lines = append(lines, make([]string, m.menuTop())...)
	lines = append(lines, pad+lipgloss.NewStyle().Foreground(foreground).Render(title)+
		strings.Repeat(" ", max(1, w-ansi.StringWidth(title)-ansi.StringWidth(shortcut)))+
		lipgloss.NewStyle().Foreground(muted).Render(shortcut), "")
	if searchableMenu(m.menu.page) {
		// The search input sits one blank line below the header.
		lines = append(lines, pad+renderRail(m.menuInput(max(1, w-2)).View(), w, false), "")
	}
	lines = append(lines, m.pageLines(pad, w)...)
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	row, column := 0, 0
	for i, line := range lines[:m.height] {
		if at := strings.Index(line, composerCursorMarker); at >= 0 {
			row, column = i+1, ansi.StringWidth(line[:at])+1
		}
		lines[i] = strings.ReplaceAll(line, composerCursorMarker, "")
	}
	return strings.Join(lines[:m.height], "\n") + fmt.Sprintf("\x1b]1337;ZelCaret;%d;%d\x07", row, column)
}

// menuBodyStart is the screen row of the first body line and menuBodyRoom the
// number of rows the body may use.
func (m model) menuBodyStart() int {
	start := m.menuTop() + 2 // the header and the blank line below it
	if searchableMenu(m.menu.page) {
		start += 2 // the search input and the blank line below it
	}
	return start
}

func (m model) menuBodyRoom() int { return max(1, m.height-m.menuBodyStart()) }

// menuLeft, menuTop and menuWidth place the menu block on the screen; the
// prepared entries of a text page scroll inside it.
func (m model) menuLeft() int { return max(0, (m.width-m.menuWidth())/2) }

func (m model) menuTop() int {
	header := 2
	if searchableMenu(m.menu.page) {
		header += 2
	}
	body := len(m.menuRows())
	if body > maxListItems {
		body = maxListItems + 1
	}
	if m.menu.page == menuRename {
		body++
	}
	if m.menu.page == menuText || m.menu.page == menuInfo {
		body = len(m.wrapText(m.menuText(), m.menuWidth()))
	}
	return max(0, (m.height-header-body)/2)
}

func (m model) menuWidth() int { return max(10, min(menuWidth, m.width-4)) }

// menuHead is the header of the current page: its path on the left, the shortcut
// that opens it on the right.
func (m model) menuHead() (title, shortcut string) {
	title, shortcut = menuTitles[m.menu.page], map[menuPage]string{menuMain: "Ctrl+K", menuModel: "Ctrl+M", menuResume: "Ctrl+R", menuInfo: "Ctrl+I", menuTracing: "Ctrl+T", menuFiles: "Ctrl+F"}[m.menu.page]
	if m.menu.page == menuMain {
		name := m.ctl.Settings().Name
		if name == "" {
			name = "Menu"
		}
		title = "zel " + strings.TrimPrefix(Version, "v") + "/" + name
	}
	if m.menu.page == menuFiles {
		title = "Menu/Files " + core.DisplayPath(m.menu.directory)
	}
	if m.menu.page == menuText {
		title = m.menu.title
		if m.menu.parent == menuTracing && m.menu.traceIndex >= 0 && m.menu.traceIndex < len(m.menu.trace) {
			shortcut = fmt.Sprintf("%d/%d", m.menu.traceIndex+1, len(m.menu.trace))
		}
	}
	return ansi.Truncate(title, max(1, m.menuWidth()-ansi.StringWidth(shortcut)-1), "…"), shortcut
}

// pageLines draws the body of the current page: text pages scroll, the others
// list their rows with the selected one in view.
func (m model) pageLines(pad string, w int) []string {
	if m.menu.page == menuText || m.menu.page == menuInfo {
		body := m.wrapText(m.menuText(), w)
		height := m.menuBodyRoom()
		start := min(max(0, m.menu.offset), max(0, len(body)-1))
		lines := make([]string, 0, height)
		for i := start; i < min(len(body), start+height); i++ {
			lines = append(lines, pad+lipgloss.NewStyle().Foreground(foreground).Render(body[i]))
		}
		return lines
	}
	rows := m.menuRows()
	width := w
	fg := lipgloss.NewStyle().Foreground(foreground)
	dim, accent := lipgloss.NewStyle().Foreground(muted), lipgloss.NewStyle().Foreground(inputBlue)
	first, visible := m.listWindow(len(rows))
	indices := m.menuIndices(m.rawMenuRows())
	var lines []string
	for i := first; i < min(len(rows), first+visible); i++ {
		detailMax := width / 2
		detail := ansi.Truncate(rows[i].detail, detailMax, "…")
		dw := ansi.StringWidth(detail)
		label := ansi.Truncate(rows[i].label, max(1, width-3-dw), "…")
		marker := "  "
		if i == m.menu.selected {
			marker = " <"
		}
		gap := strings.Repeat(" ", max(0, width-ansi.StringWidth(label)-2-dw))
		prefix := ""
		if m.menu.page == menuTracing && i < len(indices) {
			number := indices[i] + 1
			prefix = lipgloss.NewStyle().Foreground(darkGray).Render(fmt.Sprintf("%*d ", len(fmt.Sprint(len(m.menu.trace))), number))
			label = ansi.Truncate(label, max(1, width-3-dw-ansi.StringWidth(prefix)), "…")
			gap = strings.Repeat(" ", max(0, width-ansi.StringWidth(prefix)-ansi.StringWidth(label)-2-dw))
		}
		lines = append(lines, pad+prefix+fg.Render(label)+accent.Render(marker)+gap+dim.Render(detail))
	}
	if m.menu.page == menuRename {
		lines = append(lines, "")
	}
	if remaining := len(rows) - first - visible; remaining > 0 {
		lines = append(lines, pad+dim.Render(fmt.Sprintf("  %d more items", remaining)))
	}
	return lines
}

// listWindow is the range of rows a list page shows with the selected row in view.
func (m model) listWindow(rows int) (first, visible int) {
	room := m.menuBodyRoom()
	if rows > min(maxListItems, room) {
		room--
	} // reserve the remaining-items hint
	visible = max(1, min(rows, maxListItems, room))
	first = min(max(0, m.menu.selected-visible+1), max(0, rows-visible))
	return first, visible
}

// wrapText wraps a text page body to the menu width.
func (m model) wrapText(text string, w int) []string {
	var lines []string
	for _, line := range strings.Split(ansi.Strip(text), "\n") {
		if line == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, strings.Split(ansi.Wrap(line, max(1, w-2), ""), "\n")...)
	}
	return lines
}

func (m model) menuText() string {
	if m.menu.page != menuInfo {
		return m.menu.text
	}
	var lines []string
	for _, row := range m.menu.info {
		lines = append(lines, row.label)
	}
	return strings.Join(lines, "\n")
}

// scrollText scrolls the body of a text page by delta rows.
func (m *model) scrollText(delta int) {
	body := m.wrapText(m.menuText(), m.menuWidth())
	m.menu.offset = min(max(0, m.menu.offset+delta), max(0, len(body)-1))
}
