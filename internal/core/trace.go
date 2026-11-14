package core

import (
	"os"
	"slices"
	"strings"
	"time"

	"zel/internal/store"
)

// TraceEntry is one step of a session trace: the system prompt, a message, the
// tool traffic or a notice, in the order the session produced it.

// traceKinds names the record kinds that the chat hides and the Tracing menu shows.
var traceKinds = map[string]string{"code": "lua", "reasoning": "thinking"}

func traceKind(kind string) string {
	if name, ok := traceKinds[kind]; ok {
		return name
	}
	return kind
}

// Trace merges history, background tasks (at their start time), and logged
// notices/errors chronologically. Stable ties preserve source order; undated
// legacy history and the system prompt stay before timestamped entries.
func (a *Runtime) Trace() []TraceEntry {
	history, slow := a.traceSources()
	return mergeTrace(history, slow())
}

// traceSources splits the trace by cost: the history is copied from memory now,
// while the returned function reads the task rows and the events log. It
// captures what it needs, so callers can run it without holding any lock.
func (a *Runtime) traceSources() ([]TraceEntry, func() []TraceEntry) {
	history := a.historyTrace()
	db, id, home := a.db, a.id, a.home
	return history, func() []TraceEntry {
		return append(taskTrace(db, id), eventTrace(home, id)...)
	}
}

func mergeTrace(history, rest []TraceEntry) []TraceEntry {
	entries := append(history, rest...)
	slices.SortStableFunc(entries, func(a, b TraceEntry) int { return a.At.Compare(b.At) })
	return entries
}

func (a *Runtime) historyTrace() (entries []TraceEntry) {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	for seq, item := range a.History {
		var at time.Time
		if meta := a.itemMeta[seq]; meta != nil {
			at = meta.At
		}
		if text, ok := instructionSnapshot(item); ok {
			entries = append(entries, TraceEntry{Kind: "system", Text: text, At: at})
			continue
		}
		for _, r := range RecordsForItem(item) {
			if r.Kind == turnEndType {
				continue // the TUI logs the error or stop separately
			}
			entries = append(entries, TraceEntry{Kind: traceKind(r.Kind), Text: r.Text, At: at})
		}
	}
	return entries
}

func taskTrace(db *store.Store, session string) (entries []TraceEntry) {
	tasks, err := sessionTasks(db, session)
	if err != nil {
		return nil
	}
	for _, t := range tasks {
		entries = append(entries, TraceEntry{Kind: "task", Text: t.ID + " $ " + t.Command + " · " + t.Status, At: t.Started})
	}
	return entries
}

// eventTrace parses the lines LogEvent appends: a timestamp, the kind and one or
// more lines of text.
func eventTrace(home, session string) (entries []TraceEntry) {
	if home == "" || session == "" {
		return nil
	}
	data, err := os.ReadFile(EventsLog(home, session))
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
