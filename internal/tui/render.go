package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"
)

var (
	cyan             = lipgloss.Color("#d97757")
	foreground       = lipgloss.Color("#d6d6d6")
	muted            = lipgloss.Color("#929292")
	codeGray         = lipgloss.Color("#a0a0a0")
	darkGray         = lipgloss.Color("#606060")
	composerCurrent  = lipgloss.Color("#8c8c8c")
	inputBlue        = lipgloss.Color("#d97757")
	inputBackground  = lipgloss.Color("#202020")
	selectBackground = lipgloss.Color("#303030")
	assistantColor   = lipgloss.Color("#d97757")
	errorColor       = lipgloss.Color("#e27878")
	warnColor        = lipgloss.Color("#d97757")
	queueColor       = lipgloss.Color("#606060")
	infoColor        = lipgloss.Color("#d97757")
	markdownStyle    = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, muted,
		infoColor, errorColor, inputBlue, inputBlue)
)

type paintKey struct {
	row    string
	width  int
	bg, fg lipgloss.TerminalColor
	color  any
}

var painted = map[paintKey]string{}

func paint(text string, width int, bg, fg lipgloss.TerminalColor) string {
	rows := strings.Split(text, "\n")
	style := lipgloss.NewStyle().Background(bg).Foreground(fg)
	for i, row := range rows {
		key := paintKey{row, width, bg, fg, lipgloss.ColorProfile()}
		if out, ok := painted[key]; ok {
			rows[i] = out
			continue
		}
		if len(painted) >= 8192 {
			clear(painted)
		}
		rows[i] = style.Render(ansi.Truncate(row, max(1, width), ""))
		painted[key] = rows[i]
	}
	return strings.Join(rows, "\n")
}

// renderEntryMode draws the chat: user messages and answers only. The trace
// of the turn (system prompt, tools, thoughts, notices) lives in the Tracing menu.
func renderEntryMode(e entry, width, limit int, markdown bool) string {
	if limit == 0 {
		return ""
	}
	width = max(1, width)
	text := ansi.Strip(clampText(e.Text, maxLayoutBytes))
	switch e.Kind {
	case "user":
		return paint(ansi.Wrap(text, width, ""), width, lipgloss.NoColor{}, darkGray)
	case "compaction":
		label := " context compacted "
		side := max(0, width-lipgloss.Width(label)) / 2
		return paint(strings.Repeat("─", side)+label+strings.Repeat("─", side), width, lipgloss.NoColor{}, darkGray)
	case "assistant":
	default:
		return ""
	}
	body, ok := "", false
	if markdown && len(text) <= maxMarkdownBytes {
		body, ok = renderMarkdown(text, width, e.Preview)
	}
	if !ok {
		body = ansi.Wrap(text, width, "")
	}
	if body == "" {
		body = "(no output)"
	}
	lines := linkedRows(body, width)
	out := paint(strings.Join(lines, "\n"), width, lipgloss.NoColor{}, foreground)
	if !showTurnStats {
		return out
	}
	header := "*"
	if metadata := answerMetadata(e); metadata != "" {
		header += " " + metadata
	}
	return paint(ansi.Truncate(header, width, "…"), width, lipgloss.NoColor{}, assistantColor) + "\n" + out
}

func answerMetadata(e entry) string {
	if e.Kind != "assistant" || e.Preview || e.Meta == nil {
		return ""
	}
	return formatDuration(e.Meta.Duration) + " · " + formatSteps(e.Meta.Steps)
}

// renderRail draws the composer rail, smoothly pulsing while its draft is queued.
func renderRail(text string, width int, queued bool) string {
	lines := strings.Split(text, "\n")
	color := inputBlue
	if queued {
		from, e1 := colorful.Hex(string(inputBlue))
		to, e2 := colorful.Hex(string(queueColor))
		if e1 == nil && e2 == nil {
			phase := (1 - math.Cos(float64(time.Now().UnixMilli())*2*math.Pi/1600)) / 2
			color = lipgloss.Color(from.BlendRgb(to, phase).Hex())
		}
	}
	rail := lipgloss.NewStyle().Foreground(color).Render("▌")
	for i, line := range lines {
		body := paint(" "+line, max(1, width-1), lipgloss.NoColor{}, foreground)
		lines[i] = ansi.Truncate(rail+body, max(1, width), "")
	}
	return strings.Join(lines, "\n")
}

const composerCursorMarker = "\x1b]1337;ZelCursor\x07"

func (m model) inputBlock() string {
	lines := strings.Split(m.input.View(), "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, m.width), "")
	}
	return strings.Join(lines, "\n")
}

type screenRows []string

func (r *screenRows) add(chunk string) { r.addLines(strings.Split(chunk, "\n")) }

func (r *screenRows) addLines(lines []string) {
	if len(*r) == 0 {
		*r = append(*r, lines...)
		return
	}
	(*r)[len(*r)-1] += lines[0]
	*r = append(*r, lines[1:]...)
}

// timelineRows is the whole timeline as one slice: the chat history, the working
// and agent indicators, the composer and the completions.
func (m *model) timelineRows(budget time.Duration) []string {
	head, tail := m.timelineSegments(budget)
	return append(slices.Clone(head), tail...)
}

// blankRow reports whether a row holds nothing but whitespace.
func blankRow(row string) bool { return strings.TrimSpace(ansi.Strip(row)) == "" }

// trimBlankTail drops the trailing blank rows of rows without copying them.
func trimBlankTail(rows []string) []string {
	for len(rows) > 0 && blankRow(rows[len(rows)-1]) {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// timelineSegments builds the timeline as a stable head, the chat history, and a
// tail that is cheap to rebuild: the indicators, the composer and the
// completions. The history is concatenated only when an entry, the width or the
// theme changed, so a loader tick or a keystroke costs nothing in its length.
func (m *model) timelineSegments(budget time.Duration) (head, tail []string) {
	if m.cache == nil {
		m.historyStale = true
	}
	if len(m.cache) > len(m.entries) {
		m.cache = m.cache[:len(m.entries)]
		m.historyStale = true
	} else if diff := len(m.entries) - len(m.cache); diff > 0 {
		m.cache = append(m.cache, make([]renderedEntry, diff)...)
		m.historyStale = true
	}
	started, rendered := time.Now(), 0
	m.renderPending = false
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		limit := 0
		if e.Kind == "assistant" || e.Kind == "user" || e.Kind == "compaction" {
			limit = -1
		}
		cached := &m.cache[i]
		if cached.width == m.width && cached.source == e && cached.maxLines == limit && !cached.draft {
			continue
		}
		draft := budget > 0 && e.Kind == "assistant" && rendered > 0 && time.Since(started) >= budget
		if draft {
			m.renderPending = true
			if cached.draft && cached.width == m.width && cached.source == e && cached.maxLines == limit {
				continue
			}
		}
		cached.source, cached.width, cached.maxLines, cached.draft = e, m.width, limit, draft
		cached.Text = renderEntryMode(e, m.width, limit, !draft)
		cached.rows = strings.Split(cached.Text, "\n")
		m.historyStale = true
		if e.Kind == "assistant" && !draft {
			rendered++
		}
	}
	if m.historyStale || m.history == nil {
		var history screenRows
		for _, cached := range m.cache {
			if cached.Text != "" {
				history.addLines(cached.rows)
				history.add("\n\n")
			}
		}
		m.history, m.historyStale = history, false
	}
	history := m.history

	// Every chat entry ends with an empty row; the indicators start on it.
	working, subagents := m.workingBlock(), m.subagentBlock()
	var mid screenRows
	if working == "" && subagents == "" {
		head = trimBlankTail(history)
	} else {
		head = history
		if len(history) > 0 {
			head, mid = history[:len(history)-1], screenRows{history[len(history)-1]}
		}
		mid.add(working)
		mid.add(subagents)
		for len(mid) > 0 && blankRow(mid[len(mid)-1]) {
			mid = mid[:len(mid)-1]
		}
		if len(mid) == 0 {
			head = trimBlankTail(head)
		}
	}
	tail = mid
	// The composer follows after one empty row; its first chunk line merges into
	// the last row of the screen, so only the rest of it is appended.
	nonEmpty := len(head)+len(tail) > 0
	m.inputTop = len(head) + len(tail)
	block := strings.Split(m.inputBlock(), "\n")
	if nonEmpty {
		m.inputTop++
		tail = append(tail, "")
	}
	tail = append(tail, block...)
	tail = append(tail, "", "")
	start, rows := 0, min(8, max(1, m.height-m.input.Height()-5))
	if count := len(m.fileCompletions); m.hasFileCompletions() {
		start = max(0, m.fileSelected%count-rows+1)
		for i := start; i < min(count, start+rows); i++ {
			selected := i == m.fileSelected%count
			marker, fg := "  ", muted
			if selected {
				marker, fg = " <", foreground
				if m.width < ansi.StringWidth(marker) {
					marker = "<"
				}
			}
			label := ansi.Truncate(sanitizeLabel(m.fileCompletions[i]), max(0, m.width-ansi.StringWidth(marker)), "…")
			row := lipgloss.NewStyle().Foreground(fg).Render(label) + lipgloss.NewStyle().Foreground(inputBlue).Render(marker)
			screen := screenRows(tail)
			screen.add(row + "\n")
			tail = screen
		}
		if remaining := count - min(count, start+rows); remaining > 0 {
			screen := screenRows(tail)
			screen.add(lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("  + %d more items", remaining)) + "\n")
			tail = screen
		}
	}
	return head, tail
}
