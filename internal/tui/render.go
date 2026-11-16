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

	"zel/internal/config"
)

var (
	// The theme, set by applyTheme from [colors]; see config.ColorKeys for where each goes.
	bgColor, accentColor, agentColor, userColor, userHighlight, grayColor, darkGray, errorColor lipgloss.Color
	traceColors                                                                                 map[string]lipgloss.Color
	markdownStyle                                                                               string
)

func init() { applyTheme(config.DefaultColors()) }

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
		return paint(ansi.Wrap(text, width, ""), width, lipgloss.NoColor{}, userColor)
	case "prompt_error":
		return paint(ansi.Wrap(text, width, ""), width, lipgloss.NoColor{}, errorColor)
	case "compaction":
		label := " context compacted "
		side := max(0, width-lipgloss.Width(label)) / 2
		return paint(strings.Repeat("─", side)+label+strings.Repeat("─", side), width, lipgloss.NoColor{}, darkGray)
	case "turn_end":
		// A turn that failed or was stopped: its time and steps in red, then why.
		header := "*"
		if e.Meta != nil {
			header += " " + formatDuration(e.Meta.Duration) + " · " + formatSteps(e.Meta.Steps)
		}
		return paint(ansi.Truncate(header, width, "…"), width, lipgloss.NoColor{}, errorColor) + "\n" +
			paint(ansi.Wrap(text, width, ""), width, lipgloss.NoColor{}, grayColor)
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
	out := paint(strings.Join(lines, "\n"), width, lipgloss.NoColor{}, agentColor)
	if !showTurnStats {
		return out
	}
	header := "*"
	if metadata := answerMetadata(e); metadata != "" {
		header += " " + metadata
	}
	return paint(ansi.Truncate(header, width, "…"), width, lipgloss.NoColor{}, accentColor) + "\n" + out
}

func answerMetadata(e entry) string {
	if e.Kind != "assistant" || e.Preview || e.Meta == nil {
		return ""
	}
	return formatDuration(e.Meta.Duration) + " · " + formatSteps(e.Meta.Steps)
}

// renderRail draws the rail used by menu and settings inputs.
func renderRail(text string, width int) string {
	lines := strings.Split(text, "\n")
	rail := lipgloss.NewStyle().Foreground(accentColor).Render("▌")
	for i, line := range lines {
		body := paint(" "+line, max(1, width-1), lipgloss.NoColor{}, agentColor)
		lines[i] = ansi.Truncate(rail+body, max(1, width), "")
	}
	return strings.Join(lines, "\n")
}

const composerCursorMarker = "\x1b]1337;ZelCursor\x07"

// queuedColor fades to a darker shade and back over a 1.6-second cycle.
func queuedColor(base lipgloss.Color, now time.Time) lipgloss.Color {
	from, err := colorful.Hex(string(base))
	if err != nil {
		return base
	}
	phase := (1 - math.Cos(float64(now.UnixMilli()%1600)*2*math.Pi/1600)) / 2
	return lipgloss.Color(from.BlendRgb(colorful.Color{}, 0.6*phase).Hex())
}

func (m model) inputBlock() string {
	return m.inputBlockAt(time.Now())
}

func (m model) inputBlockAt(now time.Time) string {
	// Rebind the textarea's style pointer on this copy, not the live composer.
	if m.pendingSteer != 0 {
		style := composerStyle()
		style.Base = style.Base.Foreground(queuedColor(userColor, now))
		style.Text = style.Text.Foreground(queuedColor(userColor, now))
		style.CursorLine = style.CursorLine.Foreground(queuedColor(userHighlight, now))
		m.input.FocusedStyle, m.input.BlurredStyle = style, style
		if m.input.Focused() {
			m.input.Focus()
		} else {
			m.input.Blur()
		}
		m.input.Cursor.Style = lipgloss.NewStyle().Transform(func(s string) string {
			return "\x1b[0m" + composerCursorMarker + lipgloss.NewStyle().Foreground(queuedColor(userHighlight, now)).Render(s)
		})
	}
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
	changedFrom := len(m.entries)
	if m.cache == nil || m.history == nil || m.historyStale {
		changedFrom = 0
		m.historyStale = true
	}
	if len(m.cache) > len(m.entries) {
		clear(m.cache[len(m.entries):])
		m.cache = m.cache[:len(m.entries)]
		changedFrom = 0
		m.historyStale = true
	} else if diff := len(m.entries) - len(m.cache); diff > 0 {
		changedFrom = min(changedFrom, len(m.cache))
		oldCount := len(m.cache)
		m.cache = append(m.cache, make([]renderedEntry, diff)...)
		m.cache[oldCount].startRow = max(0, len(m.history)-1)
	}
	started, rendered := time.Now(), 0
	m.renderPending = false
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		limit := 0
		if e.Kind == "assistant" || e.Kind == "user" || e.Kind == "compaction" || e.Kind == "turn_end" {
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
		changedFrom = min(changedFrom, i)
		if e.Kind == "assistant" && !draft {
			rendered++
		}
	}
	if changedFrom < len(m.cache) || m.historyStale || m.history == nil {
		var history screenRows
		if changedFrom > 0 && changedFrom < len(m.cache) && m.cache[changedFrom].startRow < len(m.history) {
			history = m.history[:m.cache[changedFrom].startRow+1]
			history[len(history)-1] = ""
		} else {
			changedFrom = 0
		}
		for i := changedFrom; i < len(m.cache); i++ {
			cached := &m.cache[i]
			cached.startRow = max(0, len(history)-1)
			if cached.Text != "" {
				history.addLines(cached.rows)
				history.add("\n\n")
			}
		}
		m.history, m.historyStale = history, false
	}
	history := m.history

	// Every chat entry ends with an empty row; the indicators start on it.
	working, subagents := m.workingBlock(), m.taskBlock()
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
	inputBlock := m.inputBlock
	if m.choice != nil {
		inputBlock = m.choiceBlock
	}
	block := strings.Split(inputBlock(), "\n")
	if nonEmpty {
		m.inputTop++
		tail = append(tail, "")
	}
	tail = append(tail, block...)
	tail = append(tail, "", "")
	start, rows := 0, min(8, max(1, m.height-m.input.Height()-5))
	items, selectedIndex := m.completionList()
	if count := len(items); count > 0 {
		start = max(0, selectedIndex-rows+1)
		for i := start; i < min(count, start+rows); i++ {
			selected := i == selectedIndex
			marker, fg := "  ", grayColor
			if selected {
				marker, fg = " <", agentColor
				if m.width < ansi.StringWidth(marker) {
					marker = "<"
				}
			}
			label := ansi.Truncate(sanitizeLabel(items[i]), max(0, m.width-ansi.StringWidth(marker)), "…")
			row := lipgloss.NewStyle().Foreground(fg).Render(label) + lipgloss.NewStyle().Foreground(accentColor).Render(marker)
			screen := screenRows(tail)
			screen.add(row + "\n")
			tail = screen
		}
		if remaining := count - min(count, start+rows); remaining > 0 {
			screen := screenRows(tail)
			screen.add(lipgloss.NewStyle().Foreground(grayColor).Render(fmt.Sprintf("  + %d more items", remaining)) + "\n")
			tail = screen
		}
	}
	return head, tail
}
