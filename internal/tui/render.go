package tui

import (
	"fmt"
	"math"
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
	style := lipgloss.NewStyle().Width(width).Background(bg).Foreground(fg)
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
	text := ansi.Strip(e.Text)
	switch e.Kind {
	case "user":
		return renderRail(ansi.Wrap(text, max(1, width-2), ""), width, false)
	case "assistant":
	default:
		return ""
	}
	body, ok := "", false
	if markdown {
		body, ok = renderMarkdown(text, max(1, width-4), e.Preview)
	}
	if !ok {
		body = ansi.Wrap(text, max(1, width-4), "")
	}
	if body == "" {
		body = "(no output)"
	}
	lines := linkedRows(body, max(1, width-2))
	for i, line := range lines {
		lines[i] = ansi.Truncate(strings.Repeat(" ", min(2, max(0, width-1)))+line, width, "…")
	}
	out := paint(strings.Join(lines, "\n"), width, lipgloss.NoColor{}, foreground)
	if !showTurnStats {
		return out
	}
	header := "*"
	if metadata := answerMetadata(e); metadata != "" {
		header += " " + metadata
	}
	return paint(ansi.Truncate("  "+header, width, "…"), width, lipgloss.NoColor{}, assistantColor) + "\n" + out
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
	return renderRail(m.input.View(), m.width, m.pendingSteer != 0)
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

func (m *model) timelineRows(budget time.Duration) []string {
	var screen screenRows
	if len(m.cache) > len(m.entries) {
		m.cache = m.cache[:len(m.entries)]
	}
	for len(m.cache) < len(m.entries) {
		m.cache = append(m.cache, renderedEntry{})
	}
	started, rendered := time.Now(), 0
	m.renderPending = false
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		limit := 0
		if e.Kind == "assistant" || e.Kind == "user" {
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
		if e.Kind == "assistant" && !draft {
			rendered++
		}
	}
	for _, cached := range m.cache {
		if cached.Text != "" {
			screen.addLines(cached.rows)
			screen.add("\n\n")
		}
	}
	screen.add(m.workingBlock())
	// Composer spacing is independent of timeline and working-indicator rows.
	for len(screen) > 0 && strings.TrimSpace(ansi.Strip(screen[len(screen)-1])) == "" {
		screen = screen[:len(screen)-1]
	}
	if len(screen) == 0 {
		screen = screenRows{"", ""}
	} else {
		screen = append(screen, "", "")
	}
	m.inputTop = len(screen) - 1
	screen.add(m.inputBlock())
	screen.add("\n\n")
	start, rows := 0, min(8, max(1, m.height-m.input.Height()-5))
	if count := len(m.fileCompletions); m.hasFileCompletions() {
		start = max(0, m.fileSelected%count-rows+1)
		for i := start; i < min(count, start+rows); i++ {
			var bg lipgloss.TerminalColor = lipgloss.NoColor{}
			fg := muted
			if i == m.fileSelected%count {
				bg, fg = selectBackground, foreground
			}
			screen.add(paint(ansi.Truncate("  "+sanitizeLabel(m.fileCompletions[i]), m.width, "…"), m.width, bg, fg) + "\n")
		}
		if remaining := count - min(count, start+rows); remaining > 0 {
			screen.add(lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("  %d more items", remaining)) + "\n")
		}
	}
	return screen
}
