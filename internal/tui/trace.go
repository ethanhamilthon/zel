package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"zel/internal/core"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// traceLabel is the first column of the Tracing page: the kind of the entry.
func traceLabel(e core.TraceEntry) string { return traceKindLabel(e.Kind) }

func traceKindLabel(kind string) string {
	switch kind {
	case "assistant":
		return "agent"
	case "code":
		return "bash"
	case "reasoning":
		return "thinking"
	}
	return kind
}

func traceKindMatches(kind, filter string) bool {
	return strings.EqualFold(traceKindLabel(kind), traceKindLabel(filter))
}

// traceContext returns the full searchable text for a traced entry,
// including its kind label, detail summary, and complete body text.
func traceContext(e core.TraceEntry) string {
	parts := []string{traceLabel(e), e.Kind, traceDetail(e), e.Text}
	if stripped := ansi.Strip(e.Text); stripped != e.Text {
		parts = append(parts, stripped)
	}
	return strings.Join(parts, " ")
}

func traceColor(kind string) lipgloss.Color {
	switch kind {
	case "system":
		return darkGray
	case "user":
		return inputBlue
	case "assistant", "agent":
		return lipgloss.Color("#ffffff")
	case "bash":
		return codeGray
	case "notice", "task":
		return lipgloss.Color("#808080")
	case "output":
		return lipgloss.Color("#404040")
	case "error":
		return errorColor
	default:
		return muted
	}
}

// traceDetail is the second column: when it happened and its first line, or the
// number of lines for a long body.
func traceDetail(e core.TraceEntry) string {
	first, rest, _ := strings.Cut(strings.TrimSpace(ansi.Strip(e.Text)), "\n")
	first = strings.Join(strings.Fields(first), " ")
	if rest != "" {
		first = fmt.Sprintf("%s · %d lines", first, len(strings.Split(e.Text, "\n")))
	}
	if e.At.IsZero() {
		return first
	}
	return first + " · " + e.At.Format("15:04:05")
}

// traceText is the body of one traced entry, keeping the raw text.
func traceText(e core.TraceEntry) string {
	if strings.TrimSpace(e.Text) == "" {
		return "(empty)"
	}
	return e.Text
}

func traceElapsed(trace []core.TraceEntry, index int) string {
	if index < 0 || index >= len(trace) || trace[index].At.IsZero() {
		return "0s"
	}
	start := trace[index].At
	for _, entry := range trace {
		if !entry.At.IsZero() && entry.At.Before(start) {
			start = entry.At
		}
	}
	return formatDuration(trace[index].At.Sub(start))
}

func (m *model) openTrace(index int) {
	e := m.menu.trace[index]
	m.menu.traceIndex = index
	m.menu.parent, m.menu.page, m.menu.offset = menuTracing, menuText, 0
	m.menu.title, m.menu.text = "Menu/Tracing/"+traceKindLabel(e.Kind), traceText(e)
}

// History is committed at the end of a model response; include the streaming
// records the TUI has already received while that response is still in flight.
func (m model) sessionTrace() []core.TraceEntry {
	trace := m.ctl.Trace()
	var live []core.TraceEntry
	for _, record := range m.entries {
		if !record.Preview {
			continue
		}
		kind := record.Kind
		switch kind {
		case "code":
			kind = "bash"
		case "reasoning":
			kind = "thinking"
		}
		at := time.Now()
		if record.Meta != nil && !record.Meta.At.IsZero() {
			at = record.Meta.At
		}
		live = append(live, core.TraceEntry{Kind: kind, Text: record.Text, At: at})
	}
	trace = append(trace, live...)
	slices.SortStableFunc(trace, func(a, b core.TraceEntry) int { return a.At.Compare(b.At) })
	return trace
}

// moveTrace navigates the filtered list without losing its search or selection.
func (m *model) moveTrace(delta int) {
	mu := m.menu
	mu.page = menuTracing
	indices := m.menuIndices(m.rawMenuRows())
	selected := mu.selected + delta
	mu.page = menuText
	if selected < 0 || selected >= len(indices) {
		return
	}
	mu.selected = selected
	m.openTrace(indices[selected])
}
