package core

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TraceEntry is one step of a session trace: the system prompt, a message, a
// bash call with its output or a notice, in the order the session produced it.

// traceKinds names the record kinds that the chat hides and the Tracing menu shows.
var traceKinds = map[string]string{"code": "lua", "reasoning": "thinking", "assistant": "agent"}

func traceKind(kind string) string {
	if name, ok := traceKinds[kind]; ok {
		return name
	}
	return kind
}

// Trace merges history and logged notices/errors chronologically. Stable ties preserve source order; undated
// history items and the system prompt stay before timestamped entries.
func (a *Runtime) Trace() []TraceEntry { return a.session().Trace() }

func (s *session) Trace() []TraceEntry {
	history, slow := s.traceSources()
	return mergeTrace(history, slow())
}

// traceSources splits the trace by cost: the history is copied from memory now,
// while the returned function reads the events log. It
// captures what it needs, so callers can run it without holding any lock.
func (s *session) traceSources() ([]TraceEntry, func() []TraceEntry) {
	history := s.historyTrace()
	path := ""
	if s.id != "" && s.store != nil {
		path = s.eventsLog()
	}
	return history, func() []TraceEntry { return s.cachedEventTrace(path) }
}

// mergeTrace sorts by time; an entry of the events log (Turn < 0) joins the
// turn of the history entry before it.
func mergeTrace(history, rest []TraceEntry) []TraceEntry {
	entries := append(history, rest...)
	slices.SortStableFunc(entries, func(a, b TraceEntry) int { return a.At.Compare(b.At) })
	turn := 0
	for i := range entries {
		if entries[i].Turn < 0 {
			entries[i].Turn = turn
		} else {
			turn = entries[i].Turn
		}
	}
	return entries
}

// historyTrace lists the history items. A bash call and its output are one
// entry; the records of a model response share its request. A turn starts at
// the item its responses name (TurnSeq), or at a user message outside a known
// turn: one without an answer yet, or any in sessions saved before turns were
// recorded. A message typed during a turn steers it and starts none.
func (s *session) historyTrace() (entries []TraceEntry) {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	ends := map[int]int{} // turn start -> its last response item
	for seq, m := range s.itemMeta {
		if m != nil && m.RequestSeq != 0 {
			ends[m.TurnSeq] = max(ends[m.TurnSeq], seq)
		}
	}
	turn, until, calls := 0, -1, map[string]int{} // call ID -> index in entries
	requests := map[int]*TraceRequest{}
	for seq, item := range s.History {
		meta := s.itemMeta[seq]
		var at time.Time
		if meta != nil {
			at = meta.At
		}
		p := s.projection(seq)
		if !p.sent {
			if text, ok := instructionSnapshot(item); ok {
				entries = append(entries, TraceEntry{Kind: "system", Text: text, At: at})
				continue
			}
		}
		if end, ok := ends[seq]; ok {
			turn, until = turn+1, end
		} else if seq > until && slices.ContainsFunc(p.records, func(r Record) bool { return r.Kind == "user" }) {
			turn++
		}
		var request *TraceRequest
		first := false
		if meta != nil && meta.RequestSeq != 0 {
			if request = requests[meta.RequestSeq]; request == nil {
				request = &TraceRequest{N: len(requests) + 1, Duration: meta.Duration, Usage: meta.Usage}
				requests[meta.RequestSeq] = request
				first = true
			}
		}
		var call struct {
			Type   string
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(item, &call)
		for _, r := range p.records {
			if r.Kind == turnEndType {
				continue // the TUI logs the error or stop separately
			}
			if i, ok := calls[call.CallID]; ok && call.Type == "function_call_output" && (r.Kind == "output" || r.Kind == "error") {
				entries[i].Output, entries[i].Status = r.Text, bashStatus(r.Text)
				if meta != nil {
					entries[i].Duration = meta.Duration
				}
				continue
			}
			e := TraceEntry{Kind: traceKind(r.Kind), Text: r.Text, At: at, Turn: turn, Request: request, First: first}
			if r.Kind == "error" {
				e.Status = "error"
			}
			first = false
			if call.Type == "function_call" && call.CallID != "" {
				calls[call.CallID] = len(entries)
			}
			entries = append(entries, e)
		}
	}
	return entries
}

// bashStatus reads the failure of a bash call from the lines zel appends to its
// output (bash_after hooks may print after them).
func bashStatus(out string) string {
	if strings.HasPrefix(out, "error: ") {
		return "error"
	}
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= max(0, len(lines)-20); i-- {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "[exit ") && strings.HasSuffix(line, "]"):
			if _, err := strconv.Atoi(line[6 : len(line)-1]); err == nil {
				return line[1 : len(line)-1]
			}
		case strings.HasPrefix(line, "[timeout after "):
			return "timeout"
		case line == "[cancelled]":
			return "cancelled"
		}
	}
	return ""
}

func (s *session) cachedEventTrace(path string) []TraceEntry {
	if path == "" {
		return nil
	}
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if s.traceCache == nil || info.Size() != s.traceSize || !info.ModTime().Equal(s.traceMod) {
		s.traceCache = eventTrace(path)
		s.traceSize, s.traceMod = info.Size(), info.ModTime()
	}
	return slices.Clone(s.traceCache)
}

// eventTrace parses the lines LogEvent appends: a timestamp, the kind and one or
// more lines of text. Their turn is set when they are merged with the history.
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
			continue
		}
		kind, text, _ := strings.Cut(rest, " ")
		e := TraceEntry{Kind: kind, Text: text, At: at, Turn: -1}
		switch kind {
		case "hook":
			e.Status, e.Duration = hookResult(text)
		case "error":
			e.Status = "error"
		}
		entries = append(entries, e)
	}
	return entries
}

// hookResult reads the end of a hook line, "-> exit N, Mms": a failure is an
// exit other than 0 and 2 (2 blocks on purpose).
func hookResult(line string) (status string, took time.Duration) {
	i := strings.LastIndex(line, " -> exit ")
	if i < 0 {
		return "", 0
	}
	code, ms, _ := strings.Cut(line[i+len(" -> exit "):], ", ")
	if n, err := strconv.Atoi(code); err == nil && n != 0 && n != 2 {
		status = "exit " + code
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(ms, "ms")); err == nil {
		took = time.Duration(n) * time.Millisecond
	}
	return status, took
}
