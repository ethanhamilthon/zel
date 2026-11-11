package tui

import (
	"fmt"
	"strings"
	"time"

	"zel/internal/agent"

	"github.com/charmbracelet/x/ansi"
)

// traceLabel is the first column of the Tracing page: the kind of the entry.
func traceLabel(e agent.TraceEntry) string { return e.Kind }

// traceDetail is the second column: when it happened and its first line, or the
// number of lines for a long body.
func traceDetail(e agent.TraceEntry) string {
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

// traceTitle is the header of one traced entry.
func traceTitle(e agent.TraceEntry) string {
	if e.At.IsZero() {
		return e.Kind
	}
	return e.Kind + " · " + e.At.Format(time.DateTime)
}

// traceText is the body of one traced entry, keeping the raw text.
func traceText(e agent.TraceEntry) string {
	if strings.TrimSpace(e.Text) == "" {
		return "(empty)"
	}
	return e.Text
}

// History is committed at the end of a model response; include the streaming
// records the TUI has already received while that response is still in flight.
func (m model) sessionTrace() []agent.TraceEntry {
	trace := m.ctl.Trace()
	var live []agent.TraceEntry
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
		live = append(live, agent.TraceEntry{Kind: kind, Text: record.Text})
	}
	at := len(trace)
	for i, e := range trace {
		if e.Kind == "task" || e.Kind == "event" {
			at = i
			break
		}
	}
	result := append([]agent.TraceEntry(nil), trace[:at]...)
	result = append(result, live...)
	return append(result, trace[at:]...)
}
