package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m model) menuView() string {
	if m.menu.page == menuForm {
		return m.formView()
	}
	pad, w := strings.Repeat(" ", m.menuLeft()), m.menuWidth()
	title, shortcut := m.menuHead()
	lines := make([]string, 0, m.height)
	lines = append(lines, make([]string, m.menuTop())...)
	header := pad + lipgloss.NewStyle().Foreground(agentColor).Render(title) +
		strings.Repeat(" ", max(1, w-ansi.StringWidth(title)-ansi.StringWidth(shortcut))) +
		lipgloss.NewStyle().Foreground(grayColor).Render(shortcut)
	if m.traceEntryView() {
		header = m.traceHeader(pad, w)
	}
	lines = append(lines, header, "")
	if searchableMenu(m.menu.page) {
		// The search input sits one blank line below the header.
		lines = append(lines, pad+renderRail(m.menuInput(max(1, w-2)).View(), w), "")
	}
	lines = append(lines, m.pageLines(pad, w)...)
	hint := ""
	if m.menu.page == menuProviders {
		hint = "Ctrl+A: Add provider"
	}
	if hint != "" {
		bodyRows := max(0, m.height-1)
		if len(lines) > bodyRows {
			lines = lines[:bodyRows]
		}
		for len(lines) < bodyRows {
			lines = append(lines, "")
		}
		lines = append(lines, pad+lipgloss.NewStyle().Foreground(grayColor).Render(hint))
	}
	if m.height <= 0 {
		return ""
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return frame(lines[:m.height])
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

func (m model) menuBodyRoom() int {
	bottom := 0
	if m.menu.page == menuProviders {
		bottom = 1
	}
	room := max(1, m.height-bottom-m.menuBodyStart())
	if m.traceEntryView() {
		return min(10, room)
	}
	return room
}

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
		body = len(m.textBody(m.menuWidth()))
		if m.traceEntryView() {
			body = min(10, body)
		}
	}
	height := m.height
	if m.menu.page == menuProviders {
		height--
	}
	return max(0, (height-header-body)/2)
}

func (m model) menuWidth() int { return max(10, min(menuWidth, m.width-4)) }

// menuHead is the header of the current page: its path on the left, the shortcut
// that opens it on the right.
func (m model) menuHead() (title, shortcut string) {
	title, shortcut = menuTitles[m.menu.page], map[menuPage]string{menuMain: "Ctrl+K", menuModel: "Ctrl+M", menuResume: "Ctrl+R", menuInfo: "Ctrl+I", menuTracing: "Ctrl+T"}[m.menu.page]
	if m.menu.page == menuMain {
		title = "zel " + strings.TrimPrefix(Version, "v")
	}
	if m.menu.page == menuInfo && len(m.menu.info) > 0 {
		title = m.menu.info[0].label
	}
	if m.menu.page == menuText {
		title = m.menu.title
		if m.menu.parent == menuTracing && m.menu.traceIndex >= 0 && m.menu.traceIndex < len(m.menu.trace) {
			title = traceElapsed(m.menu.trace, m.menu.traceIndex)
			shortcut = fmt.Sprintf("%d/%d", m.menu.traceIndex+1, len(m.menu.trace))
		}
	}
	return ansi.Truncate(title, max(1, m.menuWidth()-ansi.StringWidth(shortcut)-1), "…"), shortcut
}

// pageLines draws the body of the current page: text pages scroll, the others
// list their rows with the selected one in view.
func (m model) pageLines(pad string, w int) []string {
	if m.menu.page == menuText || m.menu.page == menuInfo {
		if m.traceEntryView() {
			body := m.textBody(w)
			height := m.menuBodyRoom()
			start := min(max(0, m.menu.offset), max(0, len(body)-height))
			lines := make([]string, 0, height)
			for i := start; i < min(len(body), start+height); i++ {
				lines = append(lines, pad+body[i])
			}
			return lines
		}
		body := m.textBody(w)
		height := m.menuBodyRoom()
		start := min(max(0, m.menu.offset), max(0, len(body)-1))
		lines := make([]string, 0, height)
		gray := 0
		for i := start; i < min(len(body), start+height); i++ {
			color := agentColor
			if i < gray {
				color = grayColor
			}
			if m.menu.page == menuInfo {
				lines = append(lines, pad+body[i])
				continue
			}
			lines = append(lines, pad+lipgloss.NewStyle().Foreground(color).Render(body[i]))
		}
		return lines
	}
	rows := m.menuRows()
	width := w
	fg := lipgloss.NewStyle().Foreground(agentColor)
	dim, accent := lipgloss.NewStyle().Foreground(grayColor), lipgloss.NewStyle().Foreground(accentColor)
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
		labelStyle, detailStyle := fg, dim
		if m.menu.page == menuTracing && i < len(indices) && indices[i] < len(m.menu.trace) {
			color := traceColor(m.menu.trace[indices[i]].Kind)
			labelStyle, detailStyle = lipgloss.NewStyle().Foreground(color), lipgloss.NewStyle().Foreground(color)
		}
		if m.menu.page == menuAccent && i < len(accentColors) {
			labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(accentColors[i].hex))
		}
		rendered := labelStyle.Render(label)
		lines = append(lines, pad+prefix+rendered+accent.Render(marker)+gap+detailStyle.Render(detail))
	}
	if m.menu.page == menuRename {
		lines = append(lines, "")
	}
	if remaining := len(rows) - first - visible; remaining > 0 {
		lines = append(lines, pad+dim.Render(fmt.Sprintf("  + %d more items", remaining)))
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

func (m model) traceEntryView() bool {
	return m.menu != nil && m.menu.page == menuText && m.menu.parent == menuTracing && m.menu.traceIndex >= 0 && m.menu.traceIndex < len(m.menu.trace)
}

func (m model) traceHeader(pad string, width int) string {
	e := m.menu.trace[m.menu.traceIndex]
	third := max(1, width/3)
	left := ansi.Truncate(traceElapsed(m.menu.trace, m.menu.traceIndex), third, "…")
	kind := ansi.Truncate(traceKindLabel(e.Kind), third, "…")
	right := ansi.Truncate(fmt.Sprintf("%d/%d", m.menu.traceIndex+1, len(m.menu.trace)), width-third, "…")
	leftWidth, kindWidth, rightWidth := ansi.StringWidth(left), ansi.StringWidth(kind), ansi.StringWidth(right)
	centerAt, rightAt := (width-kindWidth)/2, width-rightWidth
	if centerAt < leftWidth || centerAt+kindWidth >= rightAt {
		centerAt = max(leftWidth+1, centerAt)
		kind = ansi.Truncate(kind, max(1, rightAt-centerAt-1), "…")
		kindWidth = ansi.StringWidth(kind)
	}
	return pad + lipgloss.NewStyle().Foreground(grayColor).Render(left) +
		strings.Repeat(" ", max(1, centerAt-leftWidth)) +
		lipgloss.NewStyle().Foreground(traceColor(e.Kind)).Render(kind) +
		strings.Repeat(" ", max(1, rightAt-centerAt-kindWidth)) +
		lipgloss.NewStyle().Foreground(grayColor).Render(right)
}

// textLayout is the laid-out body of the open text page. It is rebuilt only when
// the text, the width, the entry kind or the theme change, never per key or tick.
type textLayout struct {
	text, kind, theme string
	width             int
	lines             []string
}

// textBody returns the wrapped (for trace entries also colored and Markdown
// rendered) lines of the open text page. The result is shared: do not modify it.
func (m model) textBody(w int) []string {
	mu := m.menu
	text, kind := m.menuText(), ""
	if m.traceEntryView() {
		e := mu.trace[mu.traceIndex]
		text, kind = traceText(e), e.Kind
	}
	if l := mu.layout; l != nil && l.width == w && l.kind == kind && l.theme == m.themeKey && l.text == text {
		return l.lines
	}
	var lines []string
	if mu.page == menuInfo {
		lines = infoBody(mu.info, w)
	} else if kind != "" {
		lines = m.traceViewLines(text, kind, w)
	} else {
		lines = m.wrapText(clampText(text, maxLayoutBytes), w)
	}
	mu.layout = &textLayout{text: text, kind: kind, theme: m.themeKey, width: w, lines: lines}
	return lines
}

func (m model) traceViewLines(text, kind string, width int) []string {
	color := traceColor(kind)
	if (kind == "bash" || kind == "code") && len(text) <= maxMarkdownBytes {
		if lines, ok := highlightBash(text, max(1, width-2)); ok {
			return lines
		}
	}
	if (kind == "system" || kind == "user" || kind == "assistant" || kind == "agent") && len(text) <= maxMarkdownBytes {
		if rendered, ok := renderTraceMarkdown(text, max(1, width-2), color); ok {
			return strings.Split(strings.TrimRight(rendered, "\n"), "\n")
		}
	}
	lines := m.wrapText(clampText(text, maxLayoutBytes), width)
	style := lipgloss.NewStyle().Foreground(color)
	for i := range lines {
		lines[i] = style.Render(lines[i])
	}
	return lines
}

func (m model) menuText() string {
	if m.menu.page != menuInfo {
		return m.menu.text
	}
	// Only a cache key for textBody: the Info rows are drawn by infoBody.
	var parts []string
	for _, row := range m.menu.info {
		parts = append(parts, row.label+row.detail)
	}
	return strings.Join(parts, "\n")
}

// scrollText scrolls the body of a text page by delta rows.
func (m *model) scrollText(delta int) {
	body := m.textBody(m.menuWidth())
	if m.traceEntryView() {
		room := m.menuBodyRoom()
		m.menu.offset = min(max(0, m.menu.offset+delta), max(0, len(body)-room))
		return
	}
	m.menu.offset = min(max(0, m.menu.offset+delta), max(0, len(body)-1))
}
