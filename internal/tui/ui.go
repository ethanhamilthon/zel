package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"zel/internal/core"

	"github.com/gdamore/tcell/v2"
)

var Version = "dev"

type result struct {
	kind       string
	value      any
	err        error
	generation int
}
type cachedRecord struct {
	source core.Record
	width  int
	theme  core.Colors
	rows   []row
	plain  bool
	full   bool // ShowPrompt when the rows were built
}
type app struct {
	screen  tcell.Screen
	ctl     *core.Controller
	runtime *core.Runtime

	ctx         context.Context
	cancel      context.CancelFunc
	results     chan result
	records     []core.Record
	cache       []cachedRecord
	input       editor
	menu        *menu
	choice      *core.Choice
	answer      editor
	choiceIndex int
	writing     bool
	cwd         string
	generation  int
	queued      uint64
	serial      uint64
	queuedText  string
	pending     string
	turnIndex   int

	scroll                         int
	history, historyBase           []row
	chatHeight                     int
	dirty                          bool
	failure                        error
	busy, pasting, operation       bool
	promptCancel                   context.CancelFunc
	promptSerial                   int
	paste                          strings.Builder
	pasteTarget                    *editor
	pasteGeneration                int
	models                         []core.ModelChoice
	modelProviders                 map[string]core.Provider
	modelsAt                       time.Time
	modelsLoading                  bool
	completions                    list
	traceFilter                    int   // the Tracing view: an index of traceFilters
	traceHooks                     *bool // the Ctrl+H toggle of Tracing; nil until pressed
	completionStart, completionEnd int
	completionKind                 byte
	completionHidden               string
	fileRoot                       string
	fileIndex                      []string
	fileAt                         time.Time
	fileLoading                    bool
	tasks                          []core.ActiveTask
	editing                        bool // Ctrl+E: an editor owns the terminal
}

// Run owns the screen, not the runtime. Only this goroutine touches UI state.
func Run(ctx context.Context, a *core.Runtime, cwd string) error {
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err = screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()
	screen.EnablePaste()
	screen.SetCursorStyle(tcell.CursorStyleSteadyBar)
	return runScreen(ctx, a, cwd, screen)
}
func runScreen(ctx context.Context, a *core.Runtime, cwd string, screen tcell.Screen) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := newEventQueue()
	defer q.Close()
	m := &app{screen: screen, runtime: a, ctx: ctx, cancel: cancel, cwd: cwd, results: make(chan result, 32), dirty: true}
	m.ctl = core.NewController(a, q.Push)
	defer func() { cancel(); m.cancelPrompt(); m.ctl.Close() }()
	if s, err := m.ctl.Session(); err == nil {
		m.records = s.Records
	}
	m.turnIndex = len(m.records)
	a.OnChoiceChanged(func() { m.post(result{kind: "choice"}) })
	defer a.OnChoiceChanged(nil)
	a.OnSubagentsChanged(func() { m.post(result{kind: "tasks"}) })
	defer a.OnSubagentsChanged(nil)
	m.syncChoice()
	m.tasks = m.ctl.ActiveTasks()
	keyboard := make(chan tcell.Event, 64)
	quit := make(chan struct{})
	defer close(quit)
	go screen.ChannelEvents(keyboard, quit)
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	m.fetchModels()
	if len(m.records) == 0 {
		m.openMenu("models")
	}
	m.draw()
	for {
		select {
		case <-ctx.Done():
			return m.failure
		case <-m.ctl.Closed():
			return m.failure
		case <-q.wake:
			for range 256 {
				e, ok := q.TryPop()
				if !ok {
					break
				}
				m.control(e)
			}
			// A bounded drain keeps keyboard input responsive during large streams.
			q.mu.Lock()
			more := len(q.items) > 0 || q.read < q.written
			q.mu.Unlock()
			if more {
				select {
				case q.wake <- struct{}{}:
				default:
				}
			}
		case r := <-m.results:
			m.accept(r)
		case e, ok := <-keyboard:
			if !ok {
				return m.failure
			}
			switch e := e.(type) {
			case *tcell.EventKey:
				m.key(e)
			case *tcell.EventResize:
				screen.Sync()
				m.dirty = true
			case *tcell.EventPaste:
				if e.Start() {
					m.pasting = true
					m.paste.Reset()
					m.pasteTarget = m.activeEditor()
					m.pasteGeneration = m.generation
				}
				if e.End() {
					m.pasting = false
					if m.pasteTarget != nil && m.pasteGeneration == m.generation {
						m.pasteTarget.insert(m.paste.String())
						if m.pasteTarget == &m.input {
							m.composerChanged()
							m.updateCompletions()
						}
					}
					m.pasteTarget = nil
					m.dirty = true
				}
			}
		case <-ticker.C:
			if m.menu != nil && !m.menu.loading && time.Since(m.menu.refreshed) > time.Second {
				switch m.menu.page {
				case "sessions", "trace", "info":
					m.loadPage()
				}
			}
			status := m.ctl.Status()
			if status.Busy || len(m.tasks) > 0 {
				m.dirty = true
			}
			if status.Busy != m.busy {
				m.busy = status.Busy
				m.dirty = true
			}
			if m.dirty {
				m.draw()
			}
		}
	}
}
func (m *app) post(r result) {
	select {
	case m.results <- r:
	case <-m.ctx.Done():
	}
}
func (m *app) work(kind string, fn func() (any, error)) {
	generation := m.generation
	go func() { value, err := fn(); m.post(result{kind: kind, value: value, err: err, generation: generation}) }()
}
func (m *app) notice(err error) {
	if err != nil {
		m.records = append(m.records, core.Record{Kind: "error", Text: err.Error()})
		m.dirty = true
	}
}
func (m *app) activeEditor() *editor {
	if m.menu != nil {
		if m.menu.form != nil {
			return &m.menu.form.input
		}
		return &m.menu.search
	}
	if m.choice != nil && m.writing {
		return &m.answer
	}
	return &m.input
}
func (m *app) syncChoice() {
	next := m.ctl.PendingChoice()
	if next == nil || m.choice == nil || next.ID != m.choice.ID {
		m.choiceIndex = 0
		m.writing = false
		m.answer = editor{limit: 4096}
	}
	m.choice = next
	m.dirty = true
}
func (m *app) control(e core.Event) {
	m.dirty = true
	switch e.Kind {
	case "started":
		m.turnIndex = len(m.records)
	case "agent":
		if e.Agent == nil || e.Agent.Kind == "error" {
			return
		}
		if e.Agent.Kind == "user" {
			m.syncQueued()
			m.pending = ""
		}
		m.records = core.ApplyEvent(m.records, *e.Agent)
	case "notice", "error":
		m.ctl.LogEvent(e.Kind, e.Text)
	case "fatal":
		m.failure = errors.New(e.Text)
		m.cancel()
	case "session":
		m.generation++
		m.operation = false
		m.modelsLoading = false
		m.records = slices.Clone(e.Session.Records)
		m.cache = nil
		m.history, m.historyBase = nil, nil
		m.scroll = 0
		m.turnIndex = len(m.records)
		m.input = editor{}
		m.pending = ""
		m.queued = 0
		m.fileRoot = ""
		m.fileIndex = nil
		m.fileLoading = false
		m.resetCompletions()
		clear(pathTargets)
		mdStable.key = nil
	case "done":
		m.pending = ""
		if e.Error != "" && !slices.ContainsFunc(m.records[min(m.turnIndex, len(m.records)):], func(r core.Record) bool { return r.Kind == "turn_end" }) {
			m.notice(errors.New(e.Error))
		}
		if len(e.Unsent) > 0 {
			var parts []string
			for _, in := range e.Unsent {
				if in.QueueID == m.queued && m.input.text == m.queuedText {
					m.input.set("")
				}
				parts = append(parts, in.Text)
			}
			if m.input.text != "" {
				parts = append(parts, m.input.text)
			}
			m.input.set(strings.Join(parts, "\n\n"))
			m.queued = 0
		}
		m.pruneRecords()
	}
	m.tasks = m.ctl.ActiveTasks()
}
func (m *app) pruneRecords() {
	kept := m.records[:0]
	cached := m.cache[:0]
	for i, r := range m.records {
		switch r.Kind {
		case "user", "assistant", "choice", "compaction", "turn_end", "error":
			kept = append(kept, r)
			if i < len(m.cache) {
				cached = append(cached, m.cache[i])
			} else {
				cached = append(cached, cachedRecord{})
			}
		}
	}
	clear(m.records[len(kept):])
	m.records = kept
	if len(cached) < len(m.cache) {
		clear(m.cache[len(cached):])
	}
	m.cache = cached
}
