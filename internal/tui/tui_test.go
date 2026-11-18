package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"zel/internal/config"
	"zel/internal/entities"

	"zel/internal/core"

	"github.com/gdamore/tcell/v2"
)

func testApp(t *testing.T) *app {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	init := "return {settings = {editor = 'perl -pi -e s/draft/EDITED/'}, prompts = {plan = 'expanded'}}\n"
	if err := os.WriteFile(config.DefaultPaths(home, dir, "").Init, []byte(init), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := core.New(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(60, 20)
	ctx, cancel := context.WithCancel(context.Background())
	q := newEventQueue()
	m := &app{runtime: a, screen: screen, ctx: ctx, cancel: cancel, results: make(chan result, 8), cwd: dir}
	m.ctl = core.NewController(a, q.Push)
	a.OnChoiceChanged(func() {})
	t.Cleanup(func() {
		cancel()
		m.cancelPrompt()
		m.ctl.Close()
		a.Close()
		q.Close()
		screen.Fini()
	})
	return m
}
func TestNewSessionClearsCachedHistory(t *testing.T) {
	m := testApp(t)
	m.records = []core.Record{{Kind: "user", Text: "OLD HISTORY"}}
	m.historyRows(60)
	s, err := m.ctl.Session()
	if err != nil {
		t.Fatal(err)
	}
	s.Records = nil
	m.control(core.Event{Kind: "session", Session: &s})
	m.historyRows(60)
	if len(m.history) != 0 || len(m.historyBase) != 0 {
		t.Fatal("old history remains")
	}
}

func TestEnterPathTokenAndTabDirectory(t *testing.T) {
	for _, descend := range []bool{false, true} {
		m := testApp(t)
		m.input.set("@folder")
		m.completionKind, m.completionStart, m.completionEnd = '@', 0, len(m.input.text)
		m.completions.items = []item{{label: "folder/", id: "folder/"}}
		key := tcell.KeyEnter
		if descend {
			key = tcell.KeyTab
		}
		m.key(tcell.NewEventKey(key, 0, 0))
		if descend {
			if m.input.text != "folder/" || len(m.input.tokens) != 0 {
				t.Fatalf("Tab: %+v", m.input)
			}
		} else if m.input.text != "folder/ " || len(m.input.tokens) != 1 {
			t.Fatalf("Enter: %+v", m.input)
		}
	}
}

func TestLuaCallCountAndCompletionPreview(t *testing.T) {
	m := testApp(t)
	for i := 0; i < 15; i++ {
		m.records = append(m.records, core.Record{Kind: "code", Text: "true"}, core.Record{Kind: "output", Text: "ok"})
	}
	m.records = append(m.records, core.Record{Kind: "reasoning", Text: "thinking"})
	if n := m.turnSteps(); n != 15 {
		t.Fatalf("lua calls = %d", n)
	}
	m.input.set("#pl")
	m.updateCompletions()
	if len(m.completions.items) != 1 || m.completions.items[0].detail == "" {
		t.Fatal("no preview")
	}
}

func TestRussianAndConfiguredShortcuts(t *testing.T) {
	for _, r := range []rune{'k', 'K', 'л', 'Л'} {
		k := normalizeControl(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModCtrl))
		if got := shortcut(k, nil); got != "menu" {
			t.Errorf("%c: %s", r, got)
		}
	}
	keys := map[string]string{"menu": "alt+k"}
	if shortcut(tcell.NewEventKey(tcell.KeyRune, 'л', tcell.ModAlt), keys) != "menu" {
		t.Fatal("Russian Alt binding")
	}
	if shortcut(tcell.NewEventKey(tcell.KeyCtrlK, 0, 0), keys) != "" {
		t.Fatal("old key still bound")
	}
	for _, key := range []tcell.Key{tcell.KeyEnter, tcell.KeyTab} {
		if shortcut(tcell.NewEventKey(key, 0, 0), nil) != "" {
			t.Fatal("legacy Enter/Tab stolen")
		}
	}
	m := testApp(t)
	m.key(tcell.NewEventKey(tcell.KeyRune, 'л', tcell.ModCtrl))
	if m.menu == nil || m.menu.page != "main" {
		t.Fatal("Russian Ctrl+K did not open menu")
	}
	plain := tcell.NewEventKey(tcell.KeyRune, 'л', 0)
	if normalizeControl(plain) != plain || shortcut(plain, nil) != "" {
		t.Fatal("plain Russian altered")
	}
}

func TestChatStateAndChoicePersistence(t *testing.T) {
	m := testApp(t)
	m.records = []core.Record{{Kind: "user", Text: "first"}, entities.ChoiceRecord("Question: detail", "Answer")}
	m.pending = "sent immediately"
	m.historyRows(60)
	first := len(m.history)
	m.historyRows(60)
	if len(m.history) != first {
		t.Fatal("optimistic input duplicated on redraw")
	}
	m.control(core.Event{Kind: "agent", Agent: &core.TurnEvent{Kind: "user", Text: "sent immediately", Final: true}})
	m.historyRows(60)
	var all strings.Builder
	for _, r := range m.history {
		all.WriteString(rowText(r))
		all.WriteByte('\n')
	}
	if strings.Count(all.String(), "sent immediately") != 1 || !strings.Contains(all.String(), "Question: detail: Answer") {
		t.Fatalf("history %q", all.String())
	}
	id, err := m.runtime.Ask("Pick", []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	m.syncChoice()
	m.writing = true
	m.answer.set("custom\nanswer")
	m.choiceKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	if m.choice != nil || id < 1 || m.records[len(m.records)-1].Kind != "choice" {
		t.Fatal("answer not kept")
	}
}
func TestCompletionAndStaleAsyncResults(t *testing.T) {
	m := testApp(t)
	m.input.set("#pl")
	m.updateCompletions()
	if len(m.completions.items) != 1 {
		t.Fatal("prompt missing")
	}
	m.pickCompletion()
	if m.input.text != "#plan " || len(m.input.tokens) != 1 {
		t.Fatalf("completion %q", m.input.text)
	}
	m.input.set("draft")
	m.promptSerial = 2
	m.accept(result{kind: "submit", generation: m.generation, value: submission{draft: "draft", text: "wrong", serial: 1}})
	if m.input.text != "draft" {
		t.Fatal("stale expansion changed draft")
	}
	m.fileRoot = "/new"
	m.fileLoading = true
	m.accept(result{kind: "files", value: fileResult{root: "/old", files: []string{"wrong"}}})
	if len(m.fileIndex) != 0 {
		t.Fatal("stale directory result")
	}
	m.records = []core.Record{{Kind: "assistant", Text: "retained"}, {Kind: "turn_end", Text: "stopped by user", Meta: &core.Metadata{Duration: time.Second}}}
	m.draw()
	var out strings.Builder
	for y := range 20 {
		for x := range 60 {
			s, _, _ := m.screen.Get(x, y)
			out.WriteString(s)
		}
	}
	if !strings.Contains(out.String(), "stopped by user") {
		t.Fatal("cancelled turn is invisible")
	}
}
func TestChoiceAnsweredWithTheKeyboard(t *testing.T) {
	m := testApp(t)
	if _, err := m.runtime.Ask("Pick", []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	m.syncChoice()
	m.draw()
	m.key(tcell.NewEventKey(tcell.KeyDown, 0, 0))
	m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	if m.choice != nil {
		t.Fatal("the choice stays open")
	}
	if r := m.records[len(m.records)-1]; r.Text != "Pick"+entities.ChoiceSeparator+"B" {
		t.Fatalf("answered %+v", r)
	}
}

func TestPruneBeforeTheNextFrame(t *testing.T) {
	m := testApp(t)
	m.records = []core.Record{{Kind: "user", Text: "start"}, {Kind: "reasoning", Text: "hidden"}}
	m.historyRows(60)
	m.records = append(m.records, core.Record{Kind: "assistant", Text: "answer"}, core.Record{Kind: "turn_end", Text: "stopped by user"})
	m.pruneRecords()
	m.draw()
	if len(m.records) != 3 || len(m.cache) != 3 {
		t.Fatal("lost an uncached final record")
	}
}

func TestInputFollowsChat(t *testing.T) {
	for _, tc := range []struct {
		name       string
		records    []core.Record
		pending    string
		indicators []core.Indicator
		inputY     int
	}{
		{name: "empty"},
		{name: "user", records: []core.Record{{Kind: "user", Text: "hello"}}, inputY: 2},
		{name: "assistant", records: []core.Record{{Kind: "assistant", Text: "hello"}}, inputY: 3},
		{name: "pending", pending: "hello", inputY: 2},
		{name: "working", indicators: []core.Indicator{{ID: "i1", Text: "Running indicator", Started: time.Now()}}, inputY: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testApp(t)
			m.records, m.pending, m.indicators = tc.records, tc.pending, tc.indicators
			m.input.set("draft")
			s := m.screen.(tcell.SimulationScreen)
			for _, height := range []int{20, 40} {
				s.SetSize(60, height)
				m.draw()
				x, y, visible := s.GetCursor() // the cursor is on the first line of the input
				if !visible || x != 5 || y != tc.inputY {
					t.Fatalf("height %d: cursor at %d,%d (visible %v), want 5,%d", height, x, y, visible, tc.inputY)
				}
				for y := y + 1; y < height; y++ {
					for x := range 60 {
						if text, _, _ := s.Get(x, y); strings.TrimSpace(text) != "" {
							t.Fatalf("unexpected footer at %d,%d: %q", x, y, text)
						}
					}
				}
			}
		})
	}
}

func TestComposerScrollsWithHistory(t *testing.T) {
	m := testApp(t)
	m.records = []core.Record{{Kind: "user", Text: strings.Repeat("history line\n", 40)}}
	m.input.set("draft")
	m.draw()
	screen := m.screen.(tcell.SimulationScreen)
	_, y, visible := screen.GetCursor()
	if !visible || y != 19 {
		t.Fatalf("following tail: cursor %d visible %v", y, visible)
	}
	m.scroll = 10
	m.draw()
	if _, _, visible := screen.GetCursor(); visible {
		t.Fatal("composer should scroll out with the timeline")
	}
	m.editKey(tcell.NewEventKey(tcell.KeyRune, 'X', 0))
	m.draw()
	if _, _, visible := screen.GetCursor(); !visible {
		t.Fatal("typing should return to the composer")
	}
	m.openMenu("info")
	if m.menu == nil || m.menu.page != "info" {
		t.Fatal("Info remains available")
	}
}

func TestTinyScreenAndChoiceEditor(t *testing.T) {
	m := testApp(t)
	_, err := m.runtime.Ask("Long question", []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	m.syncChoice()
	m.writing = true
	m.answer.set("я界👩🏽‍💻\nsecond line")
	s := m.screen.(tcell.SimulationScreen)
	for _, size := range [][2]int{{1, 1}, {2, 3}, {12, 8}, {60, 20}} {
		s.SetSize(size[0], size[1])
		m.draw()
	}
	m.menu = &menu{page: "form", title: "Edit", form: &form{kind: "rename", fields: []string{"Title"}, values: []string{""}}}
	for _, size := range [][2]int{{1, 1}, {2, 3}, {12, 8}, {60, 20}} {
		s.SetSize(size[0], size[1])
		m.draw()
	}
	m.openMenu("main")
	for _, size := range [][2]int{{1, 1}, {2, 3}, {12, 8}, {60, 20}} {
		s.SetSize(size[0], size[1])
		m.draw()
	}
}

func TestStyledRows(t *testing.T) {
	base := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	rows := styledRows("a\x1b[38;2;255;0;0m界\x1b[0me\u0301\n\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\!", 4, base)
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rowText(rows[0]) != "a界e\u0301" {
		t.Fatalf("graphemes: %q", rowText(rows[0]))
	}
	fg, bg, _ := rows[0][1].style.Decompose()
	if fg != tcell.NewRGBColor(255, 0, 0) || bg != tcell.ColorBlack {
		t.Fatalf("colors: %v %v", fg, bg)
	}
	rows = styledRows("a\x1b[2Jb\x1b]52;c;malicious\a", 4, base)
	if rowText(rows[0]) != "ab" {
		t.Fatalf("terminal controls leaked: %q", rowText(rows[0]))
	}
}
func TestEditorScreenCursorAndColors(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(12, 8)
	m := app{screen: screen}
	e := editor{}
	e.set("я界\nhello")
	e.tokens = []string{"hello"}
	m.drawEditor(&e, 1, 2, 8, 2, false)
	screen.Show()
	x, y, visible := screen.GetCursor()
	if !visible || x != 6 || y != 3 {
		t.Fatalf("cursor %d,%d %v", x, y, visible)
	}
	s, _, w := screen.Get(2, 2)
	if s != "界" || w != 2 {
		t.Fatalf("wide text: %q width %d", s, w)
	}
	_, st, _ := screen.Get(1, 3)
	fg, _, _ := st.Decompose()
	if fg != tcell.GetColor(string(accentColor)) {
		t.Fatal("completed token color")
	}
	screen.Clear()
	e.set("secret")
	m.drawEditor(&e, 0, 0, 10, 1, true)
	for x := range 6 {
		s, _, _ := screen.Get(x, 0)
		if s != "•" {
			t.Fatal("API key is not masked")
		}
	}
}
func TestSearchKeepsActionIdentity(t *testing.T) {
	l := list{items: []item{{label: "Resume", id: "sessions"}, {label: "Models", id: "models"}, {label: "Providers", detail: "URL", id: "providers"}}, query: "prv url"}
	it, ok := l.current()
	if !ok || it.id != "providers" {
		t.Fatalf("filtered action: %+v", it)
	}
	l.move(10)
	if l.selected != 0 {
		t.Fatal("single-item wrap")
	}
	l.query = "nothing"
	if _, ok := l.current(); ok {
		t.Fatal("empty search selected an action")
	}
	l = list{items: []item{{"Models", "Switch model and effort", "models"}, {"Info", "Model, context and spend", "info"}}, query: "info"}
	if it, _ := l.current(); it.id != "info" {
		t.Fatalf("a label match must come before a detail match: %+v", l.visible())
	}
}
func TestFileTokensAndPaths(t *testing.T) {
	text := "Read @\"my file.go\" please"
	token, ok := findFileToken(text, len([]rune("Read @\"my")))
	if !ok || token.query != "my" || string([]rune(text)[token.start:token.end]) != "@\"my file.go\"" {
		t.Fatalf("quoted token %+v %v", token, ok)
	}
	for _, text := range []string{"a@b.com", "https://example.com/@me"} {
		if _, ok := findFileToken(text, len([]rune(text))); ok {
			t.Fatalf("not a file reference: %s", text)
		}
	}
	if quotePath("my file.go") != "@\"my file.go\"" {
		t.Fatal("path quoting")
	}
	if got := filterFileCompletions([]string{"z.go", "foo.go", "folder/"}, "fo"); len(got) != 2 || got[0] != "folder/" {
		t.Fatalf("completions: %v", got)
	}
}
func TestEventQueueOrderingAndOverflow(t *testing.T) {
	q := newEventQueue()
	defer q.Close()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 700 {
			q.Push(core.Event{Kind: "notice", Text: fmt.Sprint(i)})
		}
	})
	wg.Wait()
	for i := range 700 {
		e, ok := q.TryPop()
		if !ok || e.Text != fmt.Sprint(i) {
			t.Fatalf("event %d: %+v %v", i, e, ok)
		}
	}
	if _, ok := q.TryPop(); ok {
		t.Fatal("extra event")
	}
	for _, text := range []string{"one", "two"} {
		q.Push(core.Event{Kind: "agent", Agent: &core.TurnEvent{Kind: "delta", ID: "x", Text: text}})
	}
	e, _ := q.TryPop()
	if e.Agent.Text != "onetwo" {
		t.Fatal("stream coalescing")
	}
}
func TestFormattingAndValidation(t *testing.T) {
	m := testApp(t)
	if m.checkField(&form{kind: "model", fields: []string{"Provider"}, values: []string{""}}, 0) == "" ||
		m.checkField(&form{kind: "model", fields: []string{"Provider"}, values: []string{"nope"}}, 0) == "" {
		t.Fatal("accepted an invalid model form")
	}
	started := time.Now().Add(-5 * time.Second)
	info := infoText(core.Info{
		Session: "s1", Title: "My Session", Project: "/tmp/p", Config: "/tmp/c", Provider: "p", Model: "m", Effort: "medium",
		Subagents: []core.SubagentRun{
			{ID: "c1", Command: "first task", Provider: "p", Model: "m", Effort: "low", Context: 12000, Started: started, Ended: started.Add(3 * time.Second)},
			{ID: "c2", Command: "second task", Provider: "p", Model: "m", Effort: "high", Context: 4000, Started: started},
		},
	})
	if !strings.Contains(info, "Subagents (2)") || !strings.Contains(info, "first task") || !strings.Contains(info, "12K · done 3s") || !strings.Contains(info, "second task") || !strings.Contains(info, "4K · running ") {
		t.Fatalf("infoText subagents:\n%s", info)
	}
	text, ok := renderMarkdown("# Heading\n\n**bold** and `code`\n\n- item", 30, false)
	if !ok || !strings.Contains(text, "Heading") || !strings.Contains(text, "item") {
		t.Fatal("markdown rendering")
	}
	for _, r := range styledRows(text, 30, tcell.StyleDefault) {
		w := 0
		for _, c := range r {
			w += c.width
		}
		if w > 30 {
			t.Fatal("markdown overflow")
		}
	}
}

// text is what the screen shows, row by row.
func screenRows(m *app) []string {
	w, h := m.screen.Size()
	var rows []string
	for y := range h {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c, _, width := m.screen.Get(x, y)
			b.WriteString(c)
			x += max(0, width-1)
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return rows
}

func TestMenuPagesShareOneBoxAndScrollUnderTheTitle(t *testing.T) {
	m := testApp(t)
	s := m.screen.(tcell.SimulationScreen)
	s.SetSize(80, 12)
	// titleAt is where the title of the open page is drawn.
	titleAt := func(title string) (int, int) {
		for y, row := range screenRows(m) {
			if x := strings.Index(row, title); x >= 0 {
				return x, y
			}
		}
		t.Fatalf("no %q:\n%s", title, strings.Join(screenRows(m), "\n"))
		return 0, 0
	}
	m.openMenu("help")
	m.draw()
	hx, hy := titleAt("Help")
	for i := 0; i < 40; i++ {
		m.menuKey(tcell.NewEventKey(tcell.KeyDown, 0, 0))
	}
	m.draw()
	if x, y := titleAt("Help"); x != hx || y != hy || strings.Contains(strings.Join(screenRows(m), "\n"), "Ctrl+K: menu") {
		t.Fatalf("the title must stay while the text scrolls:\n%s", strings.Join(screenRows(m), "\n"))
	}
	m.openMenu("main")
	m.draw()
	rows := screenRows(m)
	if x, y := titleAt("Zel "); x != hx || y != hy {
		t.Fatalf("main at %d,%d, help at %d,%d", x, y, hx, hy)
	}
	s.SetSize(80, 24)
	m.draw()
	rows = screenRows(m)
	for _, row := range rows {
		if strings.Contains(row, "select") || strings.Contains(row, "Esc") || strings.Contains(row, "Enter") || strings.Contains(row, "·") {
			t.Fatalf("a hint or a separator is left: %q", row)
		}
	}
	var resume, quit string
	for _, row := range rows {
		if strings.Contains(row, "Resume") {
			resume = row
		}
		if strings.Contains(row, "Quit") {
			quit = row
		}
	}
	// The box is 60 wide at x=10: the descriptions end at its right edge minus
	// the arrow column, and the arrow is in the last column of the box.
	boxX, boxW := strings.Index(resume, "Resume"), 60
	end := func(row, detail string) int { return strings.Index(row, detail) + len(detail) }
	if boxX != 10 || end(resume, "Open a past session") != boxX+boxW-2 || end(quit, "Save and exit") != boxX+boxW-2 {
		t.Fatalf("the descriptions are not flush with the right edge of the box:\n%q\n%q", resume, quit)
	}
	if !strings.HasSuffix(resume, "Open a past session ‹") || len([]rune(resume)) != boxX+boxW || strings.Contains(quit, "‹") || strings.Contains(resume, "Ctrl+") {
		t.Fatalf("the arrow is in the last column of the selected row only, no hotkeys: %q %q", resume, quit)
	}
}

// Tracing groups entries by turn, Tab filters them, the arrows of an entry go
// through the filter only, Esc returns to the same row and Ctrl+H hides the hooks.
func TestTracingFiltersTurnsAndEntries(t *testing.T) {
	m := testApp(t)
	m.screen.(tcell.SimulationScreen).SetSize(100, 30)
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	req := &core.TraceRequest{N: 1, Duration: 1500 * time.Millisecond, Usage: &core.Usage{Input: 16000, Output: 780, Cached: 14000}}
	m.openMenu("trace")
	mu := m.menu
	mu.loading = false
	mu.trace = []core.TraceEntry{
		{Kind: "system", Text: "prompt"},
		{Kind: "hook", Text: "session_created: init.lua:3 -> ok, 5ms", At: at, Duration: 5 * time.Millisecond},
		{Kind: "user", Text: "list the files", At: at, Turn: 1},
		{Kind: "lua", Text: "ls", Output: "a.go", At: at.Add(time.Second), Turn: 1, Duration: 300 * time.Millisecond, Request: req, First: true},
		{Kind: "agent", Text: "Two files.", At: at.Add(2 * time.Second), Turn: 1, Request: req},
		{Kind: "user", Text: "fail", At: at.Add(3 * time.Second), Turn: 2},
		{Kind: "lua", Text: "false", Output: "[exit 1]", Status: "exit 1", At: at.Add(4 * time.Second), Turn: 2, Duration: 10 * time.Millisecond},
	}
	m.showTrace()
	key := func(k tcell.Key, r rune) { m.menuKey(tcell.NewEventKey(k, r, 0)); m.draw() }
	view := func() string { m.draw(); return strings.Join(screenRows(m), "\n") }
	if v := view(); !strings.Contains(v, "Tracing · All") || !strings.Contains(v, "Turn 1 · list the files") ||
		!strings.Contains(v, "Turn 2 · fail") || !strings.Contains(v, "exit 1 · 10ms") || !strings.Contains(v, "300ms") {
		t.Fatalf("the list has turn rows and the durations:\n%s", v)
	}
	if it, _ := mu.list.current(); it.id != "6" {
		t.Fatalf("Tracing opens at the latest entry: %+v", it)
	}
	key(tcell.KeyUp, 0)
	key(tcell.KeyUp, 0)
	if it, _ := mu.list.current(); it.id != "4" {
		t.Fatalf("Up skips the turn row: %+v", it)
	}
	key(tcell.KeyDown, 0)
	key(tcell.KeyTab, 0)
	v := view()
	if !strings.Contains(v, "Tracing · Lua") || strings.Contains(v, "Two files") || !strings.Contains(v, "false") {
		t.Fatalf("Tab shows the Lua entries only:\n%s", v)
	}
	key(tcell.KeyEnter, 0)
	if v := view(); !strings.Contains(v, "Tracing / lua · 2/2 · turn 2") || !strings.Contains(v, "exit 1 · 10ms") {
		t.Fatalf("Enter opens the selected call:\n%s", v)
	}
	key(tcell.KeyLeft, 0)
	key(tcell.KeyLeft, 0)
	v = view()
	if !strings.Contains(v, "Tracing / lua · 1/2 · turn 1") || !strings.Contains(v, "── output") || !strings.Contains(v, "a.go") ||
		!strings.Contains(v, "ok · 300ms") || !strings.Contains(v, "request 1 · 1.5s · 16.0k in (14.0k cached) · 780 out") {
		t.Fatalf("the entry has its command, output, time and tokens:\n%s", v)
	}
	key(tcell.KeyEscape, 0)
	if it, _ := mu.list.current(); mu.page != "trace" || it.id != "3" {
		t.Fatalf("Esc returns to the row of the entry: %s %+v", mu.page, it)
	}
	key(tcell.KeyBacktab, 0)
	if v := view(); !strings.Contains(v, "Tracing · All") || !strings.Contains(v, "session_created") {
		t.Fatalf("Shift+Tab goes back to All:\n%s", v)
	}
	rows := screenRows(m)
	turnRow, sysRow := "", ""
	for y, r := range rows {
		if strings.Contains(r, "Turn 1 · list the files") {
			turnRow = r
			x := strings.Index(r, "Turn 1")
			_, _, style, _ := m.screen.GetContent(x, y)
			if fg, _, _ := style.Decompose(); fg != tcell.GetColor(string(darkGray)) {
				t.Fatalf("turn header color = %v, want darkGray", fg)
			}
		}
		if strings.Contains(r, "system   prompt") {
			sysRow = r
		}
	}
	if turnRow == "" || sysRow == "" || strings.Index(turnRow, "Turn 1") != strings.Index(sysRow, "system") {
		t.Fatalf("entry rows must align with turn headers without left indent:\n%q\n%q", turnRow, sysRow)
	}
	key(tcell.KeyCtrlH, 0)
	if v := view(); !strings.Contains(v, "hooks hidden") || strings.Contains(v, "session_created") || mu.search.text != "" {
		t.Fatalf("Ctrl+H hides the hooks:\n%s", v)
	}
	key(tcell.KeyRune, 'h')
	if mu.search.text != "h" || m.traceHooksShown() {
		t.Fatalf("h types into the search: %q", mu.search.text)
	}
}

func abs(n int) int { return max(n, -n) }

func TestWorkingRowColorsAndSingleLine(t *testing.T) {
	status := core.Status{Busy: true, Started: time.Now().Add(-3 * time.Second), Activity: "go test ./...\nsecond line " + strings.Repeat("x", 100)}
	r := workingRow(status, "", 2, true, 40)
	text := rowText(r)
	if w := rowWidth(r); w > 40 || strings.Contains(text, "\n") || !strings.HasSuffix(text, "...") || !strings.Contains(text, "2 calls") {
		t.Fatalf("%d wide: %q", w, text)
	}
	fg := func(c span) tcell.Color { f, _, _ := c.style.Decompose(); return f }
	if len(r) != 2 || fg(r[0]) != tcell.GetColor(string(accentColor)) || fg(r[1]) != tcell.GetColor(string(darkGray)) {
		t.Fatalf("the star and the time are accent, what runs is dark gray: %+v", r)
	}
	if hidden := rowText(workingRow(status, "", 2, false, 40)); strings.Contains(hidden, "go test") {
		t.Fatalf("show_code = false must hide the command: %q", hidden)
	}
	if explicit := rowText(workingRow(status, "read main.go", 2, false, 40)); !strings.Contains(explicit, "read main.go") {
		t.Fatalf("callStatus must show even when show_code = false: %q", explicit)
	}
	indRow := indicatorRow(core.Indicator{ID: "i1", Text: "make test", Started: time.Now().Add(-6 * time.Second)}, 40)
	indTxt := rowText(indRow)
	if !strings.ContainsAny(indTxt, "◐◓◑◒") || !strings.Contains(indTxt, "6s · make test") {
		t.Fatalf("indicatorRow = %q", indTxt)
	}
}

func TestMenuClosesWithConfiguredKey(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	if err := os.WriteFile(config.DefaultPaths(home, dir, "").Init, []byte("return {settings = {keys = {menu = 'alt+k'}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := core.New(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := newEventQueue()
	m := &app{runtime: a, screen: screen, ctx: ctx, cancel: cancel, results: make(chan result, 8), cwd: dir}
	m.ctl = core.NewController(a, q.Push)
	t.Cleanup(func() {
		cancel()
		m.ctl.Close()
		a.Close()
		q.Close()
		screen.Fini()
	})

	m.key(tcell.NewEventKey(tcell.KeyRune, 'k', tcell.ModAlt))
	if m.menu == nil || m.menu.page != "main" {
		t.Fatal("alt+k did not open menu")
	}
	m.key(tcell.NewEventKey(tcell.KeyCtrlK, 0, 0))
	if m.menu == nil {
		t.Fatal("hard-coded Ctrl+K closed menu when keys.menu = alt+k")
	}
	m.key(tcell.NewEventKey(tcell.KeyRune, 'k', tcell.ModAlt))
	if m.menu != nil {
		t.Fatal("keys.menu (alt+k) did not close menu")
	}
}

func TestPrintRecordAndConfigScreenAndCommandShortcut(t *testing.T) {
	m := testApp(t)
	m.control(core.Event{Kind: "print", Text: "printed note"})
	m.pruneRecords()
	if len(m.records) != 1 || m.records[0].Kind != "print" || m.records[0].Text != "printed note" {
		t.Fatalf("print record lost: %+v", m.records)
	}
	m.historyRows(60)
	if len(m.cache) != 1 || len(m.cache[0].rows) == 0 {
		t.Fatalf("print record not rendered: %+v", m.cache)
	}
	fg, _, _ := m.cache[0].rows[0][0].style.Decompose()
	if fg != tcell.GetColor(string(darkGray)) {
		t.Fatalf("print row color = %v, want darkGray", fg)
	}

	cfg := &core.Config{
		Files:         []string{"/tmp/init.lua"},
		Systems:       []core.System{{Name: "style", Text: "short", Source: "/tmp/init.lua"}},
		Functions:     []core.Function{{Name: "sh", Description: "`sh(cmd)` - run\nsecond line", Source: "/tmp/init.lua"}},
		Prompts:       map[string]string{"plan": "expanded"},
		PromptSources: map[string]string{"plan": "/tmp/init.lua"},
		Hooks:         []core.Hook{{Events: []string{"idle"}, Async: true, Source: "init.lua:5"}},
		Commands:      []core.Command{{Name: "Commit", Description: "commit diff", Key: "alt+g", Source: "/tmp/init.lua"}},
		Warnings:      []string{"warn1"},
		Errors:        []string{"err1"},
	}
	txt := configText(cfg, "Reloaded")
	for _, want := range []string{"Reloaded", "Errors (1)", "err1", "Warnings (1)", "warn1", "Files (1)", "Systems (1)", "style", "Functions (1)", "`sh(cmd)` - run", "Prompts (1)", "plan", "Hooks (1)", "idle (async)", "Commands (1)", "Commit · alt+g"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("configText missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "second line") {
		t.Fatalf("configText must only show first line of function description:\n%s", txt)
	}

	if name, ok := commandShortcut(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModAlt), cfg); !ok || name != "Commit" {
		t.Fatalf("commandShortcut = %q, %v", name, ok)
	}
	m.openMenu("config")
	m.draw()
	m.key(tcell.NewEventKey(tcell.KeyCtrlR, 0, 0))
	select {
	case r := <-m.results:
		m.accept(r)
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not finish")
	}
	if m.menu == nil || m.menu.page != "config" || !strings.Contains(m.menu.body, "Files (") {
		t.Fatalf("Config page after Ctrl+R: %+v", m.menu)
	}
}

func rowText(r row) string {
	var b strings.Builder
	for _, c := range r {
		b.WriteString(c.text)
	}
	return b.String()
}

func TestCtrlEEditsTheInputInTheEditor(t *testing.T) {
	m := testApp(t)
	m.input.set("my draft")
	m.key(tcell.NewEventKey(tcell.KeyCtrlE, 0, 0))
	if !m.editing {
		t.Fatal("the editor did not start")
	}
	m.draw() // drawing waits while the editor owns the terminal
	select {
	case r := <-m.results:
		m.accept(r)
	case <-time.After(5 * time.Second):
		t.Fatal("the editor did not finish")
	}
	if m.editing || m.input.text != "my EDITED" {
		t.Fatalf("editing %v, input %q", m.editing, m.input.text)
	}
}

func TestCollapsePrompts(t *testing.T) {
	in := "fix <prompt name=\"plan-1\">\nline 1\n\nline 2\n</prompt> and <prompt name=\"b\">\nx\n</prompt>."
	if got, want := collapsePrompts(in), "fix #plan-1 and #b."; got != want {
		t.Fatalf("collapsePrompts = %q, want %q", got, want)
	}
}
