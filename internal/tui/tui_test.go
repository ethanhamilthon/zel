package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"zel/internal/entities"

	"github.com/gdamore/tcell/v2"
	"zel/internal/core"
)

func testApp(t *testing.T) *app {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "zel.toml"), []byte("[completions]\nplan=\"expanded\"\n"), 0600); err != nil {
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
func TestMouseCopyAndChoiceClick(t *testing.T) {
	m := testApp(t)
	m.records = []core.Record{{Kind: "user", Text: "copy these words"}}
	m.draw()
	m.mouse(tcell.NewEventMouse(0, 0, tcell.Button1, 0))
	m.mouse(tcell.NewEventMouse(3, 0, tcell.Button1, 0))
	m.mouse(tcell.NewEventMouse(3, 0, tcell.ButtonNone, 0))
	if text := string(m.screen.(tcell.SimulationScreen).GetClipboardData()); text != "copy" {
		t.Fatalf("copied %q", text)
	}
	m.selected = nil
	_, err := m.runtime.Ask("Pick", []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	m.syncChoice()
	m.draw()
	m.mouse(tcell.NewEventMouse(3, m.inputY+2, tcell.Button1, 0))
	if m.choice != nil {
		t.Fatal("mouse did not answer")
	}
	if r := m.records[len(m.records)-1]; r.Text != "Pick"+entities.ChoiceSeparator+"B" {
		t.Fatalf("mouse answered %+v", r)
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
	m.menu = &menu{page: "form", title: "Edit", form: &form{kind: "provider", fields: []string{"Key"}, values: []string{""}}}
	m.draw()
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
	if rows[1][0].link != "https://example.com" || rows[2][0].link != "" {
		t.Fatal("hyperlink boundary")
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
}
func TestSelectionWideTextAndReverseDrag(t *testing.T) {
	rows := plainRows("a界b\nnext", 20, agentColor)
	s := selection{rows: rows, start: point{2, 1}, end: point{1, 0}}
	if got := s.text(); got != "界b\nnex" {
		t.Fatalf("selection: %q", got)
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := q.Pop(ctx); ok {
		t.Fatal("cancelled pop")
	}
}
func TestFormattingAndValidation(t *testing.T) {
	for _, f := range []*form{{kind: "provider", values: []string{"wrong", "https://example.com", "key", "name"}}, {kind: "provider", values: []string{"responses", "file:///tmp", "key", "name"}}, {kind: "model", fields: []string{"Provider"}, values: []string{""}}} {
		bad := false
		for i := range f.values {
			if validateField(f, i) != nil {
				bad = true
				break
			}
		}
		if !bad {
			t.Fatal("accepted an invalid form")
		}
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
