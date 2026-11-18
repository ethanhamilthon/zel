package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"zel/internal/core"
)

// traceFilters are the views Tab cycles in Tracing.
var traceFilters = []string{"All", "Lua", "Agent", "Errors"}

func traceShown(t core.TraceEntry, filter int, hooks bool) bool {
	if t.Kind == "hook" && !hooks {
		return false
	}
	switch traceFilters[filter] {
	case "Lua":
		return t.Kind == "lua"
	case "Agent":
		return t.Kind == "agent"
	case "Errors":
		return t.Status != "" || t.Kind == "error"
	}
	return true
}

// traceHooksShown is the Ctrl+H toggle of Tracing; until it is pressed, the
// trace_hooks setting decides.
func (m *app) traceHooksShown() bool {
	if m.traceHooks != nil {
		return *m.traceHooks
	}
	return m.ctl.Settings().TraceHooks
}

func (m *app) traceTitle() string {
	title := "Tracing · " + traceFilters[m.traceFilter]
	if !m.traceHooksShown() {
		title += " · hooks hidden"
	}
	return title
}

// traceItems lists the entries the filter shows under a header row per turn:
// the turn number, its first message and how long it took. The id of an entry
// row is its index in trace.
func traceItems(trace []core.TraceEntry, filter int, hooks bool) []item {
	// A turn is titled by its first message, or by a notification that started it.
	titles, typed := map[int]string{}, map[int]bool{}
	first, last := map[int]time.Time{}, map[int]time.Time{}
	for _, t := range trace {
		switch {
		case t.Kind == "user" && !typed[t.Turn]:
			titles[t.Turn], typed[t.Turn] = firstLine(t.Text), true
		case t.Kind == "notice" && titles[t.Turn] == "":
			titles[t.Turn] = firstLine(t.Text)
		}
		// A turn lasts from its first entry to its last model response or Lua
		// call; later events (an idle hook, a resume) do not stretch it.
		if !t.At.IsZero() {
			if first[t.Turn].IsZero() {
				first[t.Turn] = t.At
			}
			if t.Request != nil || t.Kind == "lua" {
				last[t.Turn] = t.At
			}
		}
	}
	var items []item
	turn := -1
	for i, t := range trace {
		if !traceShown(t, filter, hooks) {
			continue
		}
		if t.Turn != turn {
			turn = t.Turn
			label, detail := "Start", ""
			if turn > 0 {
				label = fmt.Sprintf("Turn %d", turn)
				if title := titles[turn]; title != "" {
					label += " · " + title
				}
				if !last[turn].IsZero() {
					detail = traceDuration(last[turn].Sub(first[turn]))
				}
			}
			items = append(items, item{label, detail, headerID})
		}
		text := t.Text
		if t.Kind == "hook" { // the exit and the time are in the detail column
			text, _, _ = strings.Cut(text, " -> exit ")
		}
		items = append(items, item{fmt.Sprintf("%-8s %s", t.Kind, firstLine(text)), traceDetail(t), fmt.Sprint(i)})
	}
	return items
}

// traceDuration is short: milliseconds under a second, a tenth of a second
// under ten.
func traceDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return formatDuration(d)
}

// traceDuration of an entry: its own run for Lua and hooks, the model request
// on the first entry of a response.
func entryDuration(t core.TraceEntry) time.Duration {
	if t.Kind == "lua" || t.Kind == "hook" {
		return t.Duration
	}
	if t.First && t.Request != nil {
		return t.Request.Duration
	}
	return 0
}

// traceDetail is the right column of an entry row: a failure, the duration and the time.
func traceDetail(t core.TraceEntry) string {
	var parts []string
	if t.Status != "" && t.Kind != "error" {
		parts = append(parts, t.Status)
	}
	if d := traceDuration(entryDuration(t)); d != "" {
		parts = append(parts, d)
	}
	at := ""
	if !t.At.IsZero() {
		at = t.At.Format("15:04:05")
	}
	if len(parts) == 0 {
		return at
	}
	return strings.Join(parts, " · ") + "  " + at
}

// traceHeader is the top of an entry page: when it happened, how it ended and
// how long it took, then the model request it came from with its tokens.
func traceHeader(t core.TraceEntry) string {
	var parts []string
	if !t.At.IsZero() {
		parts = append(parts, t.At.Format("15:04:05"))
	}
	switch {
	case t.Status != "":
		parts = append(parts, t.Status)
	case strings.HasPrefix(t.Output, "[skipped: "), strings.HasPrefix(t.Output, "[moved to task "):
	case t.Kind == "lua" && t.Duration > 0:
		parts = append(parts, "exit 0")
	}
	if t.Kind == "lua" || t.Kind == "hook" {
		if d := traceDuration(t.Duration); d != "" {
			parts = append(parts, d)
		}
	}
	lines := []string{strings.Join(parts, " · ")}
	if r := t.Request; r != nil {
		req := []string{fmt.Sprintf("request %d", r.N)}
		if d := traceDuration(r.Duration); d != "" {
			req = append(req, d)
		}
		if u := r.Usage; u != nil {
			in := formatTokens(u.Input) + " in"
			if u.Cached >= 0 {
				in += " (" + formatTokens(u.Cached) + " cached)"
			}
			req = append(req, in, formatTokens(u.Output)+" out")
		}
		lines = append(lines, strings.Join(req, " · "))
	}
	return strings.Join(lines, "\n")
}

// showTrace rebuilds the Tracing list for the filter and the hook toggle. The
// selection stays on its entry, or moves to the next one the filter shows; a
// list opened anew starts at the latest entry.
func (m *app) showTrace() {
	mu := m.menu
	at := len(mu.trace)
	if it, ok := mu.list.current(); ok {
		at, _ = strconv.Atoi(it.id)
	}
	mu.list.items = traceItems(mu.trace, m.traceFilter, m.traceHooksShown())
	mu.title = m.traceTitle()
	v := mu.list.visible()
	mu.list.selected = len(v) - 1
	for i, it := range v {
		if n, err := strconv.Atoi(it.id); err == nil && !it.header() && n >= at {
			mu.list.selected = i
			break
		}
	}
	mu.list.clamp()
}

// traceEntries are the entry rows the Tracing list behind an open entry shows.
func (mu *menu) traceEntries() (ids []int) {
	for _, it := range mu.traceList.visible() {
		if n, err := strconv.Atoi(it.id); err == nil && !it.header() {
			ids = append(ids, n)
		}
	}
	return ids
}

func (m *app) openTraceEntry(i int) {
	mu := m.menu
	if i < 0 || i >= len(mu.trace) {
		return
	}
	if mu.page == "trace" {
		mu.traceList = mu.list
	}
	t := mu.trace[i]
	mu.page, mu.traceIndex, mu.list, mu.offset = "entry", i, list{}, 0
	mu.body = cmp.Or(t.Text+t.Output, " ") // a body scrolls with Up and Down
	ids := mu.traceEntries()
	pos := slices.Index(ids, i)
	mu.title = fmt.Sprintf("Tracing / %s · %d/%d", t.Kind, pos+1, len(ids))
	if t.Turn > 0 {
		mu.title += fmt.Sprintf(" · turn %d", t.Turn)
	}
	// Back in the list, the selection is on this entry.
	for n, it := range mu.traceList.visible() {
		if it.id == strconv.Itoa(i) {
			mu.traceList.selected = n
		}
	}
}

// stepTraceEntry opens the previous or next entry the list shows: the arrows
// go through the filter's entries only.
func (m *app) stepTraceEntry(delta int) {
	mu := m.menu
	ids := mu.traceEntries()
	pos := slices.Index(ids, mu.traceIndex)
	if pos < 0 || pos+delta < 0 || pos+delta >= len(ids) {
		return
	}
	m.openTraceEntry(ids[pos+delta])
}

func (m *app) closeTraceEntry() {
	mu := m.menu
	mu.page, mu.list, mu.body, mu.offset = "trace", mu.traceList, "", 0
	mu.title = m.traceTitle()
	m.loadPage()
}
