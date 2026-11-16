package tui

import (
	"cmp"
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
	return cmp.Or(map[string]string{"assistant": "agent", "code": "bash", "reasoning": "thinking"}[kind], kind)
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
	return cmp.Or(traceColors[kind], grayColor)
}

// traceDetail is the second column: when it happened and its first line, or the
// number of lines for a long body.
func traceDetail(e core.TraceEntry) string {
	head := e.Text
	if len(head) > 4096 {
		head = head[:4096] // the first line is all the list shows
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(ansi.Strip(head)), "\n")
	first = strings.Join(strings.Fields(first), " ")
	if rest != "" {
		first = fmt.Sprintf("%s · %d lines", first, strings.Count(e.Text, "\n")+1)
	}
	if !e.At.IsZero() {
		first += " · " + e.At.Format("15:04:05")
	}
	return first
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

// History is committed at the end of a model response; liveTrace is the
// streaming records the TUI has already received while that response is still
// in flight. The snapshot merges them into the session trace.
func (m model) liveTrace() []core.TraceEntry {
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
	return live
}

// sessionTrace is the full trace of the session, read synchronously.
func (m model) sessionTrace() []core.TraceEntry {
	trace := append(m.ctl.Trace(), m.liveTrace()...)
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
