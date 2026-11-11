package agent

import (
	"os"
	"strings"
	"time"
)

// TraceEntry is one step of a session trace: the system prompt, a message, the
// tool traffic or a notice, in the order the session produced it.
type TraceEntry struct {
	Kind string // system, user, assistant, thinking, bash, output, error, notice, task, event
	Text string
	At   time.Time
}

// traceKinds names the record kinds that the chat hides and the Tracing menu shows.
var traceKinds = map[string]string{"code": "bash", "reasoning": "thinking"}

func traceKind(kind string) string {
	if name, ok := traceKinds[kind]; ok {
		return name
	}
	return kind
}

// Trace lists everything the session has done: the history, which holds the system
// prompt and the tool traffic the chat hides, then its background tasks and the
// notices and errors clients wrote to the session events log.
func (a *Agent) Trace() []TraceEntry {
	return append(append(a.historyTrace(), a.taskTrace()...), a.eventTrace()...)
}

func (a *Agent) historyTrace() (entries []TraceEntry) {
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
			entries = append(entries, TraceEntry{Kind: traceKind(r.Kind), Text: r.Text, At: at})
		}
	}
	return entries
}

func (a *Agent) taskTrace() (entries []TraceEntry) {
	tasks, err := a.Tasks()
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
func (a *Agent) eventTrace() (entries []TraceEntry) {
	if a.home == "" || a.id == "" {
		return nil
	}
	data, err := os.ReadFile(EventsLog(a.home, a.id))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		// LogEvent indents every further line of a message by four spaces.
		if len(entries) > 0 && strings.HasPrefix(line, "    ") {
			entries[len(entries)-1].Text += "\n" + strings.TrimRight(line[4:], " ")
			continue
		}
		if len(line) < 21 {
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05", line[:19])
		if err != nil {
			continue
		}
		kind, text, _ := strings.Cut(line[20:], " ")
		entries = append(entries, TraceEntry{Kind: kind, Text: text, At: at})
	}
	return entries
}
