package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rivo/uniseg"
	"zel/internal/core"
	"zel/internal/entities"
)

func (m *app) draw() {
	width, height := m.screen.Size()
	if width < 1 || height < 1 {
		return
	}
	m.dirty = false
	colors, _ := m.ctl.Appearance()
	applyTheme(colors)
	linkBase = m.cwd
	m.screen.SetStyle(style(agentColor))
	m.screen.Fill(' ', style(agentColor))
	m.screen.HideCursor()

	if m.menu != nil {
		m.drawMenu(width, height)
		m.screen.Show()
		return
	}
	m.syncQueued()
	m.historyRows(width)
	status := m.ctl.Status()
	var indicators []row
	for _, task := range m.tasks {
		indicators = append(indicators, plainRows("● "+cmp.Or(task.Name, task.ID)+" · "+formatDuration(time.Since(task.Started)), width, grayColor)...)
	}
	if status.Busy {
		_, ui := m.ctl.Appearance()
		stars := []string{"✶", "✸", "✹", "✺", "✹", "✸"}
		icon := stars[int(time.Since(status.Started)/(80*time.Millisecond))%len(stars)]
		indicators = append(indicators, plainRows(icon+" "+workingText(status, m.turnSteps(), ui.ShowBash), width, accentColor)...)
	}
	if m.promptCancel != nil {
		indicators = append(indicators, plainRows("Expanding prompt…", width, grayColor)...)
	}
	e := &m.input
	inputRows := e.rows(max(1, width-2))
	inputHeight := min(len(inputRows), max(1, height/3))
	completionRows := min(6, min(len(m.completions.items), max(0, height/3)))
	choiceRows := 0
	if m.choice != nil {
		completionRows = 0
		if m.writing {
			e = &m.answer
			inputRows = e.rows(max(1, width-2))
			inputHeight = min(len(inputRows), max(1, height-5))
			choiceRows = 1
		} else {
			inputHeight = 0
			choiceRows = min(len(m.choice.Options)+2, max(1, height/2))
		}
	}
	// The composer is the tail of the same scrollable timeline, not a footer.
	tailStart := len(m.history) + len(indicators)
	gap := 0
	if tailStart > 0 {
		gap = 1
	}
	total := tailStart + gap + choiceRows + inputHeight + completionRows
	m.scroll = max(0, min(m.scroll, max(0, total-height)))
	start := max(0, total-height-m.scroll)
	end := min(len(m.history), start+height)
	m.visibleStart = min(start, len(m.history))
	m.chatHeight = max(0, end-m.visibleStart)
	for i, r := range m.history[m.visibleStart:end] {
		drawRow(m.screen, i, r, width)
	}
	y := len(m.history) - start
	for _, r := range indicators {
		drawRow(m.screen, y, r, width)
		y++
	}
	y += gap
	m.inputY = y
	m.inputHeight = inputHeight
	if m.choice != nil {
		m.line(y, m.choice.Title, width, accentColor)
		y++
		if !m.writing {
			options := append(slices.Clone(m.choice.Options), "Write your own answer…")
			shown := max(0, choiceRows-1)
			first := max(0, m.choiceIndex-shown+1)
			m.choiceFirst, m.choiceVisible = first, shown
			for i := first; i < min(len(options), first+shown); i++ {
				prefix := "  "
				color := grayColor
				if i == m.choiceIndex {
					prefix = "› "
					color = accentColor
				}
				m.line(y, prefix+options[i], width, color)
				y++
			}
		}
	}
	if inputHeight > 0 {
		if m.choice != nil && m.writing && e.text == "" {
			m.at(1, y, "Your answer", max(1, width-2), darkGray)
		}
		m.inputY = y
		m.drawEditor(e, 1, y, max(1, width-2), inputHeight, false)
		y += inputHeight
	}
	if completionRows > 0 {
		first := max(0, m.completions.selected-completionRows+1)
		for i := first; i < min(len(m.completions.items), first+completionRows); i++ {
			color := grayColor
			prefix := "  "
			if i == m.completions.selected {
				color = accentColor
				prefix = "› "
			}
			m.line(y, prefix+m.completions.items[i].label, width, color)
			y++
		}
	}

	m.drawSelection()
	m.screen.Show()
}
func (m *app) line(y int, text string, width int, color lipgloss.Color) {
	if rows := plainRows(text, width, color); len(rows) > 0 {
		drawRow(m.screen, y, rows[0], width)
	}
}
func (m *app) historyRows(width int) {
	colors, _ := m.ctl.Appearance()
	changed := len(m.cache) != len(m.records)
	if len(m.cache) > len(m.records) {
		m.cache = m.cache[:len(m.records)]
	}
	for len(m.cache) < len(m.records) {
		m.cache = append(m.cache, cachedRecord{})
	}
	started, rendered := time.Now(), false
	for i := len(m.records) - 1; i >= 0; i-- {
		r := m.records[i]
		c := &m.cache[i]
		if c.source == r && c.width == width && c.theme == colors && !c.plain {
			continue
		}
		changed = true
		c.source, c.width, c.theme = r, width, colors
		c.rows = nil
		c.plain = false
		switch r.Kind {
		case "user":
			c.rows = plainRows(r.Text, width, userColor)
		case "assistant":
			text, ok := "", false
			if len(r.Text) <= 64<<10 {
				if !rendered || time.Since(started) < 4*time.Millisecond {
					text, ok = renderMarkdown(r.Text, width, r.Preview || m.ctl.Status().Busy)
					rendered = true
				} else {
					c.plain = true
					m.dirty = true
				}
			}
			if !ok {
				text = cleanInput(displayText(r.Text))
			}
			c.rows = plainRows(answerHeader(r), width, accentColor)
			c.rows = append(c.rows, styledRows(text, width, style(agentColor))...)
		case "choice":
			question, answer, _ := strings.Cut(r.Text, entities.ChoiceSeparator)
			c.rows = styledRows(lipgloss.NewStyle().Foreground(accentColor).Render(question+": ")+lipgloss.NewStyle().Foreground(userColor).Render(answer), width, style(userColor))
		case "compaction":
			c.rows = plainRows(r.Text, width, darkGray)
		case "turn_end":
			c.rows = plainRows(answerHeader(r), width, errorColor)
			c.rows = append(c.rows, plainRows(r.Text, width, grayColor)...)
		case "error", "prompt_error":
			c.rows = plainRows(r.Text, width, errorColor)
		}
		if len(c.rows) > 0 {
			c.rows = append(c.rows, row{})
		}
	}
	if changed {
		m.historyBase = nil
		for _, c := range m.cache {
			m.historyBase = append(m.historyBase, c.rows...)
		}
	}
	m.history = m.historyBase
	if m.pending != "" {
		// The optimistic message stays until the runtime emits the accepted input.
		m.history = append(slices.Clone(m.history), plainRows(m.pending, width, userColor)...)
	}
}
func answerHeader(r core.Record) string {
	if r.Meta == nil || r.Preview {
		return "*"
	}
	return "* " + formatDuration(r.Meta.Duration) + " · " + formatSteps(r.Meta.Steps)
}
func (m *app) drawEditor(e *editor, x, y, width, height int, secret bool) {
	rows := e.rows(width)
	cx, cy := e.location(rows)
	e.scroll = max(0, min(e.scroll, max(0, len(rows)-height)))
	if cy < e.scroll {
		e.scroll = cy
	}
	if cy >= e.scroll+height {
		e.scroll = cy - height + 1
	}
	tokenRanges := [][2]int{}
	for _, token := range e.tokens {
		for at := 0; at < len(e.text); {
			i := strings.Index(e.text[at:], token)
			if i < 0 {
				break
			}
			i += at
			tokenRanges = append(tokenRanges, [2]int{i, i + len(token)})
			at = i + len(token)
		}
	}
	for n, r := range rows[e.scroll:min(len(rows), e.scroll+height)] {
		at := x
		g := uniseg.NewGraphemes(e.text[r.start:r.end])
		for g.Next() {
			start, _ := g.Positions()
			text := g.Str()
			w := g.Width()
			color := userHighlight
			for _, span := range tokenRanges {
				if r.start+start >= span[0] && r.start+start < span[1] {
					color = accentColor
					break
				}
			}
			if text == "\t" {
				w = 4 - (at-x)%4
				text = strings.Repeat(" ", w)
			}
			if secret {
				text = strings.Repeat("•", max(1, w))
			}
			if at+w <= x+width {
				for len(text) > 0 {
					rest, used := m.screen.Put(at, y+n, text, style(color))
					if used < 1 {
						break
					}
					at += used
					text = rest
				}
			}
		}
	}
	m.screen.ShowCursor(x+min(cx, max(0, width-1)), y+cy-e.scroll)
}
func (m *app) turnSteps() int {
	count := 0
	for _, r := range m.records[min(m.turnIndex, len(m.records)):] {
		if r.Kind == "bash" || r.Kind == "reasoning" {
			count++
		}
		if r.Meta != nil {
			count = max(count, r.Meta.Steps)
		}
	}
	return count
}
func workingText(status core.Status, steps int, showBash bool) string {
	parts := []string{formatDuration(time.Since(status.Started))}
	if status.Cancelling {
		parts = append(parts, "cancelling")
	} else {
		parts = append(parts, formatSteps(steps))
	}
	switch queued := len(status.Queued); {
	case queued == 1:
		parts = append(parts, "queued")
	case queued > 1:
		parts = append(parts, fmt.Sprintf("%d queued", queued))
	}
	if !status.Cancelling && showBash {
		parts = append(parts, cmp.Or(activityLine(status, time.Now()), "thinking..."))
	}
	return strings.Join(parts, " · ")
}

// activityStep is how long each line of a multi-line command stays in the indicator.
const activityStep = 150 * time.Millisecond

// activityLine is the row of the activity to show now: a running multi-line
// command cycles through its lines, anything else shows its first line.
func activityLine(status core.Status, now time.Time) string {
	lines := strings.Split(status.Activity, "\n")
	if !status.Live || len(lines) < 2 {
		return lines[0]
	}
	return lines[int(max(0, now.Sub(status.LiveSince))/activityStep)%len(lines)]
}

// turnSteps counts the tool calls and thinking blocks of the running turn. The
// chat hides those entries, but they still arrive and keep the count live.
// Completed answers carry authoritative totals, including steps missing from live
// events. Continue counting subsequent work from that total without resetting it.
