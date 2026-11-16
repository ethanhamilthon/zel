package core

import (
	"os"
	"slices"
	"strings"
	"time"
)

// TraceEntry is one step of a session trace: the system prompt, a message, the
// tool traffic or a notice, in the order the session produced it.

// traceKinds names the record kinds that the chat hides and the Tracing menu shows.
var traceKinds = map[string]string{"code": "bash", "reasoning": "thinking"}

func traceKind(kind string) string {
	if name, ok := traceKinds[kind]; ok {
		return name
	}
	return kind
}

// Trace merges history and logged notices/errors chronologically. Stable ties preserve source order; undated
// legacy history and the system prompt stay before timestamped entries.
func (a *Runtime) Trace() []TraceEntry { return a.session().Trace() }

func (s *session) Trace() []TraceEntry {
	history, slow := s.traceSources()
	return mergeTrace(history, slow())
}

func (a *Runtime) traceSources() ([]TraceEntry, func() []TraceEntry) {
	return a.session().traceSources()
}

// traceSources splits the trace by cost: the history is copied from memory now,
// while the returned function reads the events log. It
// captures what it needs, so callers can run it without holding any lock.
func (s *session) traceSources() ([]TraceEntry, func() []TraceEntry) {
	history := s.historyTrace()
	path := ""
	if s.id != "" && s.db != nil {
		path = s.eventsLog()
	}
	return history, func() []TraceEntry { return eventTrace(path) }
}

func mergeTrace(history, rest []TraceEntry) []TraceEntry {
	entries := append(history, rest...)
	slices.SortStableFunc(entries, func(a, b TraceEntry) int { return a.At.Compare(b.At) })
	return entries
}

func (s *session) historyTrace() (entries []TraceEntry) {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	for seq, item := range s.History {
		var at time.Time
		if meta := s.itemMeta[seq]; meta != nil {
			at = meta.At
		}
		p := s.projection(seq)
		if !p.sent {
			if text, ok := instructionSnapshot(item); ok {
				entries = append(entries, TraceEntry{Kind: "system", Text: text, At: at})
				continue
			}
		}
		for _, r := range p.records {
			if r.Kind == turnEndType {
				continue // the TUI logs the error or stop separately
			}
			entries = append(entries, TraceEntry{Kind: traceKind(r.Kind), Text: r.Text, At: at})
		}
	}
	return entries
}

// eventTrace parses the lines LogEvent appends: a timestamp, the kind and one or
// more lines of text.
func eventTrace(path string) (entries []TraceEntry) {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		// LogEvent indents every further line of a message by four spaces.
		if len(entries) > 0 && strings.HasPrefix(line, "    ") {
			entries[len(entries)-1].Text += "\n" + strings.TrimRight(line[4:], " ")
			continue
		}
		stamp, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			// Legacy logs used local wall time, without an offset or subseconds.
			if len(line) < 21 {
				continue
			}
			at, err = time.ParseInLocation(time.DateTime, line[:19], time.Local)
			if err != nil {
				continue
			}
			rest = line[20:]
		}
		kind, text, _ := strings.Cut(rest, " ")
		entries = append(entries, TraceEntry{Kind: kind, Text: text, At: at})
	}
	return entries
}
