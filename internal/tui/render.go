package tui

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"

	"zel/internal/core"

	"github.com/rivo/uniseg"
)

func (m *app) draw() {
	width, height := m.screen.Size()
	if width < 1 || height < 1 || m.editing {
		return
	}
	m.dirty = false
	applyTheme(m.ctl.Appearance())
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
	for _, ind := range m.indicators {
		indicators = append(indicators, indicatorRow(ind, width))
	}
	if status.Busy {
		if len(indicators) > 0 {
			indicators = append(indicators, row{}) // one line between the indicators and the working line
		}
		indicators = append(indicators, workingRow(status, m.callStatus, m.turnSteps(), width))
	}
	if m.promptCancel != nil {
		indicators = append(indicators, plainRows("Expanding prompt…", width, grayColor)...)
	}
	e := &m.input
	inputRows := e.rows(width)
	completionRows := min(6, min(len(m.completions.items), max(0, height/3)))
	// The input grows with its text up to the whole screen, less one line above
	// it and the completions panel under it.
	inputHeight := min(len(inputRows), max(1, height-1-min(1, completionRows)-completionRows))
	choiceRows := 0
	if m.choice != nil {
		completionRows = 0
		if m.writing {
			e = &m.answer
			inputRows = e.rows(width)
			inputHeight = min(len(inputRows), max(1, height-5))
			choiceRows = 1
		} else {
			inputHeight = 0
			choiceRows = min(len(m.choice.Options)+2, max(1, height/2))
		}
	}
	// The composer is the tail of the same scrollable timeline, not a footer.
	// One empty line separates it from what is above; a chat record already ends with one.
	tailStart := len(m.history) + len(indicators)
	gap := 1
	switch {
	case tailStart == 0:
		gap = 0
	case len(indicators) > 0:
	case len(m.history) > 0 && len(m.history[len(m.history)-1]) == 0:
		gap = 0
	}
	panelGap := min(1, completionRows)
	total := tailStart + gap + choiceRows + inputHeight + panelGap + completionRows
	m.scroll = max(0, min(m.scroll, max(0, total-height)))
	start := max(0, total-height-m.scroll)
	end := min(len(m.history), start+height)
	first := min(start, len(m.history))
	m.chatHeight = max(0, end-first)
	for i, r := range m.history[first:end] {
		drawRow(m.screen, i, r, width)
	}
	y := len(m.history) - start
	for _, r := range indicators {
		drawRow(m.screen, y, r, width)
		y++
	}
	y += gap
	if m.choice != nil {
		m.line(y, m.choice.Title, width, accentColor)
		y++
		if !m.writing {
			options := append(slices.Clone(m.choice.Options), "Write your own answer…")
			shown := max(0, choiceRows-1)
			first := max(0, m.choiceIndex-shown+1)
			items := make([]item, len(options))
			for i, o := range options {
				items[i].label = o
			}
			m.drawList(0, y, width, items[first:min(len(options), first+shown)], m.choiceIndex-first, false)
			y += min(len(options), first+shown) - first
		}
	}
	if inputHeight > 0 {
		if m.choice != nil && m.writing && e.text == "" {
			m.at(0, y, "Your answer", width, darkGray)
		}
		m.drawEditor(e, 0, y, width, inputHeight, false)
		y += inputHeight
	}
	if completionRows > 0 {
		first := max(0, m.completions.selected-completionRows+1)
		last := min(len(m.completions.items), first+completionRows)
		m.drawList(0, y+panelGap, width, m.completions.items[first:last], m.completions.selected-first, false)
	}
	m.screen.Show()
}
func (m *app) line(y int, text string, width int, color lipgloss.Color) {
	if rows := plainRows(text, width, color); len(rows) > 0 {
		drawRow(m.screen, y, rows[0], width)
	}
}
func (m *app) historyRows(width int) {
	colors := m.ctl.Appearance()
	full := m.ctl.ShowPrompt()
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
		if c.source == r && c.width == width && c.theme == colors && c.full == full && !c.plain {
			continue
		}
		changed = true
		c.source, c.width, c.theme, c.full = r, width, colors, full
		c.rows = nil
		c.plain = false
		switch r.Kind {
		case "user":
			text := r.Text
			if !full {
				text = collapsePrompts(text)
			}
			c.rows = plainRows(text, width, userColor)
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
			question, answer, _ := strings.Cut(r.Text, core.ChoiceSeparator)
			c.rows = styledRows(lipgloss.NewStyle().Foreground(accentColor).Render(question+": ")+lipgloss.NewStyle().Foreground(userColor).Render(answer), width, style(userColor))
		case "compaction", "print":
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

// turnSteps counts the tool calls of the running turn, not reasoning. The
// chat hides those entries, but they still arrive and keep the count live.
// Completed answers carry authoritative totals, including steps missing from live
// events. Continue counting subsequent work from that total without resetting it.
func (m *app) turnSteps() int {
	count := 0
	for _, r := range m.records[min(m.turnIndex, len(m.records)):] {
		if r.Kind == "code" {
			count++
		}
		if r.Meta != nil {
			count = max(count, r.Meta.Steps)
		}
	}
	return count
}

// workingRow shows elapsed time, the last `zel status` text, call count, and
// queued input, in that order. It never shows the code of a call.
func workingRow(status core.Status, callStatus string, steps int, width int) row {
	stars := []string{"✶", "✸", "✹", "✺", "✹", "✸"}
	elapsed := time.Since(status.Started)
	var r row
	add := func(text string, st tcell.Style) {
		if width <= 0 {
			return
		}
		if len(r) > 0 {
			text = " · " + text
		}
		truncated := ansi.Truncate(text, width, "...")
		n := textWidth(truncated)
		r = append(r, span{truncated, n, st})
		width -= n
		if truncated != text {
			width = 0
		}
	}
	accent := style(accentColor)
	add(stars[int(max(0, elapsed)/(80*time.Millisecond))%len(stars)]+" "+formatDuration(elapsed), accent)
	if status.Cancelling {
		add("cancelling", accent)
	} else {
		if line, _, _ := strings.Cut(callStatus, "\n"); line != "" {
			add(cleanInput(strings.ReplaceAll(line, "\t", " ")), style(darkGray))
		}
		add(formatSteps(steps), accent)
	}
	switch queued := len(status.Queued); {
	case queued == 1:
		add("queued", accent)
	case queued > 1:
		add(fmt.Sprintf("%d queued", queued), accent)
	}
	return r
}

// indicatorRow is one active `zel status new` line, drawn above the working
// line: the spinner and elapsed time in the accent color, then the text in dark gray.
func indicatorRow(ind core.Indicator, width int) row {
	frames := []string{"◐", "◓", "◑", "◒"}
	since := time.Since(ind.Started)
	head := ansi.Truncate(frames[int(since/(120*time.Millisecond))%len(frames)]+" "+formatDuration(since), width, "...")
	r := row{{head, textWidth(head), style(accentColor)}}
	if room := width - textWidth(head) - 3; room > 0 {
		name := ansi.Truncate(" · "+cleanInput(cmp.Or(ind.Text, ind.ID)), room+3, "...")
		r = append(r, span{name, textWidth(name), style(darkGray)})
	}
	return r
}

var promptBlock = regexp.MustCompile(`(?s)<prompt name="([A-Za-z0-9_-]+)">\n.*?\n</prompt>`)

// collapsePrompts shows each <prompt name="x"> block of a user message as #x.
func collapsePrompts(text string) string {
	return promptBlock.ReplaceAllString(text, "#$1")
}
