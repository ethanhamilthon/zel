package tui

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"zel/internal/agent"
	"zel/internal/control"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

// TestMockupFrames renders real TUI states into docs/mockup/index.html:
// ZEL_MOCKUP=$PWD/docs/mockup go test ./internal/tui -run TestMockupFrames
func TestMockupFrames(t *testing.T) {
	dir := os.Getenv("ZEL_MOCKUP")
	if dir == "" {
		t.Skip("ZEL_MOCKUP not set")
	}
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	var frames []mockFrame
	const cwd = "~/Projects/zel"
	save := func(name string, m model) {
		m.refresh(true)
		for m.renderPending {
			m.refresh(true)
		}
		frames = append(frames, mockFrame{name: name, ansi: m.View()})
	}
	sized := func(m model) model {
		next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
		return next.(model)
	}
	// typeText feeds keys and resolves the commands they return, skipping slow ones.
	resolve := func(m model, pending ...tea.Cmd) model {
		for steps := 0; len(pending) > 0 && steps < 40; steps++ {
			cmd := pending[0]
			pending = pending[1:]
			if cmd == nil {
				continue
			}
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			select {
			case msg := <-done:
				if batch, ok := msg.(tea.BatchMsg); ok {
					pending = append(pending, batch...)
					continue
				}
				next, more := m.Update(msg)
				m = next.(model)
				pending = append(pending, more)
			case <-time.After(200 * time.Millisecond):
			}
		}
		return m
	}
	typeText := func(m model, text string) model {
		m.input.SetValue("")
		var pending []tea.Cmd
		for _, r := range text {
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = next.(model)
			pending = append(pending, cmd)
		}
		return resolve(m, pending...)
	}
	ms := func(d time.Duration, steps ...int) *agent.Metadata {
		ctx := int64(18_400)
		s := 0
		if len(steps) > 0 {
			s = steps[0]
		}
		return &agent.Metadata{Duration: d, Steps: s, Usage: &agent.Usage{Input: 18_400, Output: 912, Cached: 12_288}, ContextInput: &ctx, At: time.Date(2026, 11, 7, 18, 38, 0, 0, time.Local)}
	}
	conversation := func(m *model) {
		n := m.applyAgent
		n(agent.Event{Kind: "user", ID: "u1", Text: "What does internal/control do? Keep it short."})
		n(agent.Event{Kind: "reasoning", ID: "r1", Text: "The user asks about the control package. I should read controller.go and ops.go\nbefore answering, then summarise the turn lifecycle."})
		n(agent.Event{Kind: "code", ID: "c1", Meta: ms(2100 * time.Millisecond), Text: "rg '^func' internal/control/controller.go | head -20"})
		n(agent.Event{Kind: "output", ID: "o1", Text: "internal/control/controller.go:49:func New(a *agent.Agent, emit func(Event)) *Controller {\ninternal/control/controller.go:98:func (c *Controller) Note(e agent.Event) {\ninternal/control/controller.go:113:func (c *Controller) History() (records []agent.Record) {\ninternal/control/controller.go:134:func (c *Controller) Close() {\ninternal/control/controller.go:172:func (c *Controller) beginLocked() *operation {\ninternal/control/controller.go:201:func (c *Controller) finishTurn(op *operation, err error) {\ninternal/control/controller.go:268:func (c *Controller) deliverLocked() {"})
		n(agent.Event{Kind: "assistant", ID: "a1", Meta: ms(14200*time.Millisecond, 2), Text: "## internal/control\n\n`Controller` sits between the **TUI** and the agent:\n\n- runs **one turn at a time**; input that arrives meanwhile becomes *steering*\n- keeps the display journal and session title\n- turns agent errors into `control.Error` with a code\n\n```go\nc := control.New(a, emit)\nc.Input(agent.Input{Text: \"hi\"})\n```\n\n| method | purpose |\n|---|---|\n| `Input` | start or steer a turn |\n| `Stop` | cancel the active turn |\n\nSee [controller.go](internal/control/controller.go)."})
	}

	onboarding, _ := freshModel(t)
	save("01-onboarding", sized(onboarding))

	// realistic renames the fixture provider; /models gets a catalog, turns reach the fixture server.
	realistic := func(f *fixture) {
		upstream, _ := url.Parse(f.a.Settings().Providers["p"].BaseURL)
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/models") {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[{"id":"gpt-5.1"},{"id":"gpt-5.1-codex"},{"id":"gpt-5-mini"},{"id":"o4-mini"}]}`)
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"id":"qwen3-coder:30b"},{"id":"llama3.3:70b"}]}`)
		}))
		t.Cleanup(local.Close)
		setConfig(t, f.m.ctl, func(s *agent.Settings) {
			s.Providers = map[string]agent.Provider{"openai": {BaseURL: server.URL + "/v1"}, "ollama": {BaseURL: local.URL + "/v1"}}
			s.Provider, s.Model, s.Effort = "openai", "gpt-5.1", "medium"
		})
		if err := f.a.SelectSession(context.Background(), "openai", "gpt-5.1", "medium"); err != nil {
			t.Fatal(err)
		}
	}
	chat := func() *fixture {
		f := newFixture(t, true)
		realistic(f)
		f.m = sized(f.m)
		f.m.cwd = cwd
		conversation(&f.m)
		return f
	}
	empty := newFixture(t, true)
	realistic(empty)
	empty.m = sized(empty.m)
	empty.m.cwd = cwd
	save("02-empty", empty.m)

	save("03-chat", chat().m)

	f := chat()
	f.m.add("error", "provider p: 429 Too Many Requests; retrying in 4s")
	save("04-flash-error", f.m)
	f = chat()
	f.m.add("notice", "Task 3 finished: go test ./...")
	save("05-flash-notice", f.m)

	// menu opens the Ctrl+K menu and presses the keys, resolving what they start.
	menu := func(m model, keys ...tea.KeyType) model {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
		m = resolve(next.(model), cmd)
		for _, key := range keys {
			next, cmd = m.Update(tea.KeyMsg{Type: key})
			m = resolve(next.(model), cmd)
		}
		return m
	}
	down := func(n int) []tea.KeyType { return slices.Repeat([]tea.KeyType{tea.KeyDown}, n) }
	save("06-menu", menu(chat().m))
	save("10-info", menu(chat().m, append(down(6), tea.KeyEnter)...))
	save("10-providers", menu(chat().m, append(down(11), tea.KeyEnter)...))
	save("10-settings", menu(chat().m, append(down(12), tea.KeyEnter)...))
	save("10-files", menu(chat().m, append(down(13), tea.KeyEnter)...))
	f = chat()
	for _, p := range []string{"README.md", "go.mod", "main.go", "Makefile", "docs/PLAN.md", "internal/control/controller.go", "internal/tui/ui.go", "internal/agent/agent.go"} {
		path := filepath.Join(f.project, p)
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		_ = os.WriteFile(path, nil, 0644)
	}
	f.m.cwd = f.project
	save("08-file-mention", typeText(f.m, "look at @"))
	tempProject := f.project
	save("09-multiline-input", typeText(chat().m, "First line of a longer prompt that wraps across the composer width so you can see how it grows and keeps the background."))

	save("10-help", menu(chat().m, append(down(8), tea.KeyEnter)...))
	models := menu(chat().m, append(down(2), tea.KeyEnter)...)
	save("10-model", models)
	save("10-model-effort", menu(chat().m, append(down(2), tea.KeyEnter, tea.KeyEnter)...))

	f = chat()
	f.fail.Store(true)
	f.run(t, "Summarise the open pull requests")
	save("15-turn-error", f.m)

	f = chat()
	for _, title := range []string{"Picker refactor", "Fix flicker in file completion", ""} {
		other, err := agent.New(f.project, agent.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := other.Run(context.Background(), "Make the model picker remember the last effort", func(agent.Event) {}); err != nil {
			t.Fatal(err)
		}
		if title != "" {
			_ = other.Rename(title)
		}
		other.Close()
	}
	for _, command := range []string{"sleep 600", "make test"} {
		if _, err := agent.RunTask(f.project, f.a.SessionID(), command); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	f.m.add("notice", "Settings reloaded from ~/.zel")
	f.m.add("error", "provider p: 429 Too Many Requests; retrying in 4s")
	f.m.add("notice", "Background task t2 finished: exit 2")
	save("10-resume", menu(f.m, tea.KeyEnter))
	save("10-tasks", menu(f.m, append(down(3), tea.KeyEnter)...))
	save("10-tasks-action", menu(f.m, append(down(3), tea.KeyEnter, tea.KeyEnter)...))
	save("10-tracing", menu(f.m, append(down(4), tea.KeyEnter)...))
	save("10-tracing-entry", menu(f.m, append(down(4), tea.KeyEnter, tea.KeyEnter)...))

	h, release := heldFixture(t)
	realistic(h)
	h.m = sized(h.m)
	h.m.cwd = cwd
	conversation(&h.m)
	next, _ := h.m.sendInput(agent.Input{Text: "Now remove the cached fields from Controller"})
	h.m = next.(model)
	<-h.reached
	h.m = settle(h.m)
	h.m.applyAgent(agent.Event{Kind: "reasoning", ID: "r2", Text: "I'll start by finding every read of c.journal, c.custom and c.first."})
	h.m.applyAgent(agent.Event{Kind: "code", ID: "c2", Text: "rg 'c\\.journal|c\\.custom|c\\.first' internal/control"})
	save("13-working", h.m)
	save("14-working-steering", typeText(h.m, "also keep the tests green"))
	release()

	if err := writeMockup(filepath.Join(dir, "index.html"), frames, tempProject, cwd); err != nil {
		t.Fatal(err)
	}
}

func probeLayout(value string, width int) []composerRow {
	if value == "" {
		return nil
	}
	probe := textarea.New()
	probe.CharLimit = 0
	probe.MaxWidth = 10000
	probe.Prompt = ""
	probe.ShowLineNumbers = false
	probe.SetWidth(width)
	var rows []composerRow
	base := 0
	for _, line := range strings.Split(value, "\n") {
		runes := []rune(line)
		probe.SetValue(line)
		col := 0
		for {
			probe.SetCursor(col)
			info := probe.LineInfo()
			end := min(info.StartColumn+info.Width, len(runes))
			rows = append(rows, composerRow{base + info.StartColumn, base + end})
			if info.RowOffset+1 >= info.Height || info.Width <= 0 {
				break
			}
			col = info.StartColumn + info.Width
		}
		base += len(runes) + 1
	}
	return rows
}

// Bubble Tea requires the full terminal file interface, not just Write and Fd.
var _ term.File = (*cursorOutput)(nil)

type cursorBuffer struct{ strings.Builder }

func (*cursorBuffer) Read([]byte) (int, error) { return 0, io.EOF }

func (*cursorBuffer) Close() error { return nil }

type fixture struct {
	m       model
	a       *agent.Agent
	fail    atomic.Bool
	hold    atomic.Pointer[chan struct{}]
	reached chan struct{}
	project string
}

func newFixture(t *testing.T, providers bool) *fixture {
	t.Helper()
	f := &fixture{project: t.TempDir(), reached: make(chan struct{}, 64)}
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		if f.fail.Load() {
			http.Error(w, `{"error":{"message":"rejected"}}`, http.StatusBadRequest)
			return
		}
		stream := bytes.Contains(body, []byte(`"stream":true`))
		if stream {
			select {
			case f.reached <- struct{}{}:
			default:
			}
		}
		if hold := f.hold.Load(); hold != nil {
			select {
			case <-*hold:
			case <-r.Context().Done():
				return
			}
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	t.Cleanup(server.Close)
	s := agent.DefaultSettings()
	if providers {
		s.Provider, s.Model = "p", "m"
		s.Providers["p"] = agent.Provider{BaseURL: server.URL}
	}
	if err := agent.WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(f.project, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.a, f.m = a, newModel(a, f.project)
	requestSignals.Store(f.m.ctl, f.reached)
	t.Cleanup(func() {
		f.m.ctl.Close()
		a.Close()
	})
	return f
}

func (f *fixture) pump() {
	for {
		e, ok := f.m.events.TryPop()
		if !ok {
			f.m = flushFrame(f.m)
			return
		}
		next, _ := f.m.Update(e)
		f.m = next.(model)
	}
}

func (f *fixture) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.pump()
		if !f.m.ctl.Status().Busy {
			f.pump()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) run(t *testing.T, text string) {
	t.Helper()
	next, _ := f.m.sendInput(agent.Input{Text: text})
	f.m = next.(model)
	f.settle(t)
}

func flushFrame(m model) model {
	if m.dirty {
		next, _ := m.Update(redraw{})
		m = next.(model)
	}
	return m
}

var requestSignals sync.Map

func heldFixture(t *testing.T) (*fixture, func()) {
	t.Helper()
	f := newFixture(t, true)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	return f, release
}

func heldModel(t *testing.T) (model, *agent.Agent, func()) {
	t.Helper()
	f, release := heldFixture(t)
	return f.m, f.a, release
}

func awaitRequest(t *testing.T, m model) {
	t.Helper()
	signals, ok := requestSignals.Load(m.ctl)
	if !ok {
		t.Fatal("model has no provider")
	}
	select {
	case <-signals.(chan struct{}):
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not receive the request")
	}
}

func busyModel(t *testing.T) (model, func()) {
	t.Helper()
	m, _, release := heldModel(t)
	m = startBusy(t, m)
	return m, release
}

func startBusy(t *testing.T, m model) model {
	t.Helper()
	if err := m.ctl.Input(agent.Input{Text: "busy"}); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, m)
	return settle(m)
}

func awaitIdle(t *testing.T, m model) model {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m = settle(m)
		if !m.ctl.Status().Busy {
			return settle(m)
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func settle(m model) model {
	for {
		event, ok := m.events.TryPop()
		if !ok {
			return flushFrame(m)
		}
		next, _ := m.Update(event)
		m = next.(model)
	}
}

func key(m model, k tea.KeyMsg) model {
	next, _ := m.Update(k)
	return settle(next.(model))
}

func agentEvent(e agent.Event) control.Event { return control.Event{Kind: "agent", Agent: &e} }

func freshAgent(t *testing.T) *agent.Agent {
	t.Helper()
	t.Setenv("ZEL_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func freshModel(t *testing.T) (model, *agent.Agent) {
	t.Helper()
	a := freshAgent(t)
	m := newModel(a, a.Shell.Dir)
	t.Cleanup(m.ctl.Close)
	return m, a
}

func bareModel(t *testing.T, cwd string) model {
	t.Helper()
	m, _ := freshModel(t)
	m.cwd = cwd
	m.menu = nil // Most unit tests exercise chat, not first-run onboarding.
	return m
}

func renderEntry(e entry, width, limit int) string { return renderEntryMode(e, width, limit, true) }

func (m *model) timeline() string { return strings.Join(m.timelineRows(0), "\n") }

// setConfig updates database preferences and reloads it.
func setConfig(t *testing.T, ctl *control.Controller, change func(*agent.Settings)) {
	t.Helper()
	home, err := agent.ZelHome("")
	if err != nil {
		t.Fatal(err)
	}
	s, err := agent.LoadConfig(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	change(&s)
	if err := agent.WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	ctl.Reload()
}

type mockFrame struct{ name, ansi string }

type mockInfo struct{ group, title, note string }

var mockGroups = []string{"Start", "Turn", "Composer", "Model & sessions", "Tasks & help", "Tracing", "Flash messages"}

var mockFrames = map[string]mockInfo{
	"01-onboarding":       {"Start", "Onboarding", "First provider onboarding inside Zel."},
	"02-empty":            {"Start", "Empty session", "The composer is empty."},
	"03-chat":             {"Start", "Conversation", "User messages and markdown answers with stats; the trace stays out of the chat."},
	"13-working":          {"Turn", "Working", "The working indicator sits above the input: elapsed time and steps."},
	"14-working-steering": {"Turn", "Steering while working", "Typing during a turn; Enter queues the message."},
	"15-turn-error":       {"Turn", "Failed turn", "Failures are recorded in Tracing, keeping the chat clean."},
	"06-menu":             {"Composer", "Menu", "Ctrl+K opens a centered menu; Esc goes back."},
	"08-file-mention":     {"Composer", "File mention", "@ lists files one directory at a time."},
	"09-multiline-input":  {"Composer", "Long input", "The composer grows with its text."},
	"10-providers":        {"Settings", "Menu: Providers", "Add, edit, and remove providers stored in SQLite."},
	"10-settings":         {"Settings", "Menu: Settings", "Application preferences stored in SQLite."},
	"10-files":            {"Files", "Menu: Files", "Browse directories, hidden entries, and edit files."},
	"10-info":             {"Model & sessions", "Menu: Info", "Live session, model, configuration layers and context usage."},
	"10-model":            {"Model & sessions", "Menu: Model", "Models across providers."},
	"10-model-effort":     {"Model & sessions", "Menu: Effort", "Reasoning effort for the chosen model."},
	"10-resume":           {"Model & sessions", "Menu: Resume", "Saved sessions of this project."},
	"10-tasks":            {"Tasks & help", "Menu: Tasks", "Background tasks with status and duration."},
	"10-tasks-action":     {"Tasks & help", "Menu: Task", "Show output or kill the task."},
	"10-help":             {"Tasks & help", "Help", "Keyboard shortcuts."},
	"04-flash-error":      {"Flash messages", "Error", "Errors go to Tracing, never to the chat."},
	"05-flash-notice":     {"Flash messages", "Notice", "Notices only go to the session events log, shown in Tracing."},
	"10-tracing":          {"Tracing", "Menu: Tracing", "Everything the session did, searchable: system, messages, tools, tasks."},
	"10-tracing-entry":    {"Tracing", "Menu: Tracing entry", "One traced entry, scrollable."},
}

var (
	csiPattern  = regexp.MustCompile(`\x1b\[([0-9;:?]*)([A-Za-z])`)
	oscPattern  = regexp.MustCompile(`\x1b\]8;[^;]*;([^\x07\x1b]*)(?:\x07|\x1b\\)`)
	hostPattern = regexp.MustCompile(`http://127\.0\.0\.1:\d+/v1`)
)

var ansiColors = []string{
	"#1d1f21", "#cc6666", "#b5bd68", "#f0c674", "#81a2be", "#b294bb", "#8abeb7", "#c5c8c6",
	"#666666", "#d54e53", "#b9ca4a", "#e7c547", "#7aa6da", "#c397d8", "#70c0b1", "#eaeaea",
}

type sgr struct {
	fg, bg                                      string
	bold, faint, italic, under, reverse, strike bool
}

func (s sgr) css() string {
	fg, bg := s.fg, s.bg
	if s.reverse {
		fg, bg = cmpOr(bg, "var(--bg)"), cmpOr(fg, "var(--fg)")
	}
	var b strings.Builder
	if fg != "" {
		fmt.Fprintf(&b, "color:%s;", fg)
	}
	if bg != "" {
		fmt.Fprintf(&b, "background:%s;", bg)
	}
	if s.bold {
		b.WriteString("font-weight:700;")
	}
	if s.faint {
		b.WriteString("opacity:.6;")
	}
	if s.italic {
		b.WriteString("font-style:italic;")
	}
	if s.under || s.strike {
		b.WriteString("text-decoration:")
		if s.under {
			b.WriteString(" underline")
		}
		if s.strike {
			b.WriteString(" line-through")
		}
		b.WriteString(";")
	}
	return b.String()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func color(params []string, i int) (string, int) {
	if i+1 >= len(params) {
		return "", i
	}
	switch params[i+1] {
	case "2":
		if i+4 < len(params) {
			r, _ := strconv.Atoi(params[i+2])
			g, _ := strconv.Atoi(params[i+3])
			b, _ := strconv.Atoi(params[i+4])
			return fmt.Sprintf("#%02x%02x%02x", r, g, b), i + 4
		}
	case "5":
		if i+2 < len(params) {
			n, _ := strconv.Atoi(params[i+2])
			return xterm(n), i + 2
		}
	}
	return "", i
}

func xterm(n int) string {
	switch {
	case n < 16:
		return ansiColors[n]
	case n < 232:
		n -= 16
		level := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", level(n/36), level(n/6%6), level(n%6))
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

func (s *sgr) apply(code string) {
	params := strings.FieldsFunc(code, func(r rune) bool { return r == ';' || r == ':' })
	if len(params) == 0 {
		params = []string{"0"}
	}
	for i := 0; i < len(params); i++ {
		n, _ := strconv.Atoi(params[i])
		switch {
		case n == 0:
			*s = sgr{}
		case n == 1:
			s.bold = true
		case n == 2:
			s.faint = true
		case n == 3:
			s.italic = true
		case n == 4:
			s.under = true
		case n == 7:
			s.reverse = true
		case n == 9:
			s.strike = true
		case n == 22:
			s.bold, s.faint = false, false
		case n == 23:
			s.italic = false
		case n == 24:
			s.under = false
		case n == 27:
			s.reverse = false
		case n == 29:
			s.strike = false
		case n >= 30 && n <= 37:
			s.fg = ansiColors[n-30]
		case n >= 90 && n <= 97:
			s.fg = ansiColors[n-90+8]
		case n >= 40 && n <= 47:
			s.bg = ansiColors[n-40]
		case n >= 100 && n <= 107:
			s.bg = ansiColors[n-100+8]
		case n == 39:
			s.fg = ""
		case n == 49:
			s.bg = ""
		case n == 38:
			s.fg, i = color(params, i)
		case n == 48:
			s.bg, i = color(params, i)
		}
	}
}

// ansiHTML converts one terminal frame to HTML spans.
func ansiHTML(frame string) string {
	var out strings.Builder
	var state sgr
	link := ""
	for _, line := range strings.Split(frame, "\n") {
		rest := caretPosition.ReplaceAllString(line, "")
		out.WriteString(`<div class="row">`)
		for rest != "" {
			csi := csiPattern.FindStringSubmatchIndex(rest)
			osc := oscPattern.FindStringSubmatchIndex(rest)
			next := len(rest)
			if csi != nil {
				next = csi[0]
			}
			if osc != nil && osc[0] < next {
				next = osc[0]
			}
			if text := rest[:next]; text != "" {
				style := state.css()
				open := ""
				if style != "" {
					open = fmt.Sprintf(`<span style="%s">`, style)
				}
				body := html.EscapeString(text)
				if link != "" {
					body = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(link), body)
				}
				if open != "" {
					out.WriteString(open + body + "</span>")
				} else {
					out.WriteString(body)
				}
			}
			if next == len(rest) {
				break
			}
			if osc != nil && osc[0] == next {
				link = rest[osc[2]:osc[3]]
				rest = rest[osc[1]:]
				continue
			}
			if rest[csi[5]-1] == 'm' {
				state.apply(rest[csi[2]:csi[3]])
			}
			rest = rest[csi[1]:]
		}
		out.WriteString("</div>")
	}
	return out.String()
}

// tidyFrame hides test-only details: temp directories and loopback provider URLs.
func tidyFrame(frame, tempProject, cwd string) string {
	if pad := len(tempProject) - len(cwd); pad >= 0 {
		frame = strings.ReplaceAll(frame, tempProject, strings.Repeat(" ", pad)+cwd)
	}
	lines := strings.Split(frame, "\n")
	for i, line := range lines {
		url := "https://api.openai.com/v1"
		if strings.Contains(line, "ollama") {
			url = "http://localhost:11434/v1"
		}
		lines[i] = hostPattern.ReplaceAllStringFunc(line, func(found string) string {
			return url + strings.Repeat(" ", max(0, len(found)-len(url)))
		})
	}
	return strings.Join(lines, "\n")
}

func writeMockup(path string, frames []mockFrame, tempProject, cwd string) error {
	byGroup := map[string][]mockFrame{}
	for _, f := range frames {
		info, ok := mockFrames[f.name]
		if !ok {
			return fmt.Errorf("frame %s has no mockup description", f.name)
		}
		byGroup[info.group] = append(byGroup[info.group], f)
	}
	var body strings.Builder
	for _, group := range mockGroups {
		sort.SliceStable(byGroup[group], func(i, j int) bool { return byGroup[group][i].name < byGroup[group][j].name })
		fmt.Fprintf(&body, "<section class=\"group\"><h2>%s</h2><div class=\"frames\">\n", html.EscapeString(group))
		for _, f := range byGroup[group] {
			info := mockFrames[f.name]
			fmt.Fprintf(&body, "<figure class=\"frame\" id=\"%s\"><div class=\"chrome\"><i></i><i></i><i></i><span>%s</span></div><div class=\"screen\">%s</div><figcaption><b>%s</b> %s</figcaption></figure>\n",
				f.name, html.EscapeString(info.title), ansiHTML(tidyFrame(f.ansi, tempProject, cwd)), html.EscapeString(info.title), html.EscapeString(info.note))
		}
		body.WriteString("</div></section>\n")
	}
	page := strings.Replace(mockupPage, "{{FRAMES}}", body.String(), 1)
	return os.WriteFile(path, []byte(page), 0644)
}

const mockupPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>zel TUI mockup</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root { --bg: #161616; --fg: #d4d4d4; --canvas: #0b0d10; --dot: #1f242b; }
* { box-sizing: border-box; }
html, body { margin: 0; height: 100%; overflow: hidden; background: var(--canvas); color: #c9d1d9;
  font: 14px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
#viewport { position: fixed; inset: 0; cursor: grab; touch-action: none;
  background-image: radial-gradient(var(--dot) 1px, transparent 1px); background-size: 24px 24px; }
#viewport.dragging { cursor: grabbing; }
#world { position: absolute; left: 0; top: 0; transform-origin: 0 0; padding: 80px; width: max-content; }
h1 { font-size: 40px; margin: 0 0 8px; color: #fff; }
.lead { margin: 0 0 56px; color: #8b949e; font-size: 18px; max-width: 900px; }
.group { margin-bottom: 96px; }
.group h2 { font-size: 26px; color: #d97757; margin: 0 0 20px; letter-spacing: .02em; }
.frames { display: flex; gap: 48px; align-items: flex-start; }
.frame { margin: 0; width: max-content; }
.chrome { display: flex; align-items: center; gap: 8px; height: 30px; padding: 0 12px; background: #2a2d33;
  border-radius: 10px 10px 0 0; color: #9aa4b2; font-size: 12px; }
.chrome i { width: 12px; height: 12px; border-radius: 50%; background: #ff5f57; }
.chrome i:nth-child(2) { background: #febc2e; } .chrome i:nth-child(3) { background: #28c840; }
.chrome span { margin-left: 8px; }
.screen { background: var(--bg); color: var(--fg); padding: 10px 12px; border-radius: 0 0 10px 10px;
  font: 13px/18px "JetBrains Mono", "SF Mono", Menlo, Consolas, monospace;
  box-shadow: 0 20px 60px rgba(0,0,0,.5); user-select: text; cursor: text; }
.row { height: 18px; white-space: pre; }
.screen a { color: inherit; }
figcaption { margin-top: 12px; color: #8b949e; max-width: 760px; }
figcaption b { color: #e6edf3; margin-right: 6px; }
#hud { position: fixed; right: 16px; bottom: 16px; display: flex; gap: 8px; align-items: center;
  background: rgba(22,27,34,.9); border: 1px solid #30363d; border-radius: 10px; padding: 6px 8px; color: #8b949e; font-size: 12px; }
#hud button { background: #21262d; color: #c9d1d9; border: 1px solid #30363d; border-radius: 6px; padding: 4px 10px; cursor: pointer; }
#zoom { min-width: 44px; text-align: center; }
</style>
</head>
<body>
<div id="viewport"><div id="world">
<h1>zel TUI — current states</h1>
<p class="lead">Real frames rendered from the TUI at 100×34 by <code>internal/tui/mockup_test.go</code>.
Drag to pan, scroll to move, Ctrl/⌘ + scroll or pinch to zoom, <kbd>0</kbd> fits everything, <kbd>1</kbd> is 100%.</p>
{{FRAMES}}
</div></div>
<div id="hud"><button id="out">−</button><span id="zoom">100%</span><button id="in">+</button><button id="fit">Fit</button><button id="one">100%</button></div>
<script>
const viewport = document.getElementById("viewport"), world = document.getElementById("world"), label = document.getElementById("zoom");
let x = 0, y = 0, scale = 1;
const draw = () => { world.style.transform = "translate(" + x + "px," + y + "px) scale(" + scale + ")"; label.textContent = Math.round(scale * 100) + "%";
  viewport.style.backgroundPosition = x + "px " + y + "px"; viewport.style.backgroundSize = 24 * scale + "px " + 24 * scale + "px"; };
const zoomAt = (factor, cx, cy) => { const next = Math.min(4, Math.max(0.05, scale * factor)); x = cx - (cx - x) * next / scale; y = cy - (cy - y) * next / scale; scale = next; draw(); };
const fit = () => { scale = Math.min(innerWidth / world.offsetWidth, innerHeight / world.offsetHeight); x = (innerWidth - world.offsetWidth * scale) / 2; y = Math.max(0, (innerHeight - world.offsetHeight * scale) / 2); draw(); };
viewport.addEventListener("wheel", e => { e.preventDefault();
  if (e.ctrlKey || e.metaKey) zoomAt(Math.exp(-e.deltaY * 0.01), e.clientX, e.clientY); else { x -= e.deltaX; y -= e.deltaY; draw(); } }, { passive: false });
let drag = null;
viewport.addEventListener("pointerdown", e => { if (e.target.closest(".screen") && !e.altKey) return; drag = { px: e.clientX, py: e.clientY, x, y }; viewport.classList.add("dragging"); viewport.setPointerCapture(e.pointerId); });
viewport.addEventListener("pointermove", e => { if (!drag) return; x = drag.x + e.clientX - drag.px; y = drag.y + e.clientY - drag.py; draw(); });
viewport.addEventListener("pointerup", () => { drag = null; viewport.classList.remove("dragging"); });
document.getElementById("in").onclick = () => zoomAt(1.25, innerWidth / 2, innerHeight / 2);
document.getElementById("out").onclick = () => zoomAt(0.8, innerWidth / 2, innerHeight / 2);
document.getElementById("fit").onclick = fit;
document.getElementById("one").onclick = () => zoomAt(1 / scale, innerWidth / 2, innerHeight / 2);
addEventListener("keydown", e => { if (e.key === "0") fit(); if (e.key === "1") zoomAt(1 / scale, innerWidth / 2, innerHeight / 2);
  if (e.key === "+" || e.key === "=") zoomAt(1.25, innerWidth / 2, innerHeight / 2); if (e.key === "-") zoomAt(0.8, innerWidth / 2, innerHeight / 2); });
addEventListener("load", fit);
</script>
</body>
</html>
`

func (m *model) applyAgent(e agent.Event) {
	m.entries = agent.ApplyEvent(m.entries, e)
	if e.Meta != nil && e.Meta.ContextInput != nil {
		m.contextTokens = *e.Meta.ContextInput
	}
}

func captureTerm(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	termOut = &out
	t.Cleanup(func() { termOut = nil })
	return &out
}
