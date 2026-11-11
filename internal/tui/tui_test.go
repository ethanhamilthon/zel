package tui

import (
	"context"
	"dashy/internal/agent"
	"dashy/internal/control"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestShiftEnterTerminalSequences(t *testing.T) {
	for _, sequence := range []string{"\x1b[13;2u", "\x1b[27;2;13~", "\x1b[13;2~"} {
		t.Run(fmt.Sprintf("%q", sequence), func(t *testing.T) {
			m := bareModel(t, "/project")
			p := tea.NewProgram(m,
				tea.WithInput(strings.NewReader("one"+sequence+"two\x03")),
				tea.WithOutput(&strings.Builder{}),
				tea.WithoutRenderer(),
				tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
					if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlC {
						return tea.QuitMsg{}
					}
					return msg
				}),
			)
			result, err := p.Run()
			if err != nil {
				t.Fatal(err)
			}
			got := result.(model)
			if got.input.Value() != "one\ntwo" || got.input.Height() != 2 || len(got.entries) != 0 {
				t.Fatalf("value %q, height %d, entries %v", got.input.Value(), got.input.Height(), got.entries)
			}
		})
	}
}

func TestInputHeightMatchesWrappedRows(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 78),
		strings.Repeat("word ", 40),
		strings.Repeat("界", 80),
		"first\n\nlast\n",
	} {
		m := bareModel(t, "/project")
		m.input.SetValue(text)
		m.refresh(false)
		probe := m.input
		rows := 0
		for _, line := range strings.Split(text, "\n") {
			probe.SetValue(line)
			rows += probe.LineInfo().Height
		}
		if m.input.Height() != min(rows, m.height-2) {
			t.Errorf("%q: height %d, rows %d", text, m.input.Height(), rows)
		}
		for _, line := range strings.Split(m.inputBlock(), "\n") {
			if ansi.StringWidth(line) != m.width {
				t.Errorf("width %d want %d", ansi.StringWidth(line), m.width)
			}
		}
	}
}

func TestWrappedMarkdownLinkTargetAndNoLeak(t *testing.T) {
	target := "https://example.com/a/very/long/path?foo=bar&baz=qux"
	for _, width := range []int{1, 2, 5, 10, 20, 80} {
		block := renderEntry(entry{Kind: "assistant", Text: "[Docs](" + target + ")\n\nPlain paragraph."}, width, -1)
		if strings.ContainsAny(block, "\u2063\u2064") {
			t.Fatalf("internal markers leaked at %d", width)
		}
		if width >= 10 && !strings.Contains(block, ansi.SetHyperlink(target)) {
			t.Fatalf("wrapped URL target changed at %d: %q", width, block)
		}
		for _, row := range strings.Split(block, "\n") {
			if ansi.StringWidth(row) > width {
				t.Fatalf("link exceeds width %d: %q", width, row)
			}
			seqs := hyperlinkSequence.FindAllString(row, -1)
			if len(seqs) > 0 && seqs[len(seqs)-1] != ansi.ResetHyperlink() {
				t.Fatalf("row leaks hyperlink: %q", row)
			}
		}
	}
}

func TestMarkdownWrapWithHyphens(t *testing.T) {
	text := "Загвоздка: мы уже за лимитом, 7131 строка при потолке в 7000. Так что сначала придётся где-то ужать код, иначе AGENTS.md нарушим. Делаем? Ещё раз-два-три и `go test -race`"
	for width := 20; width <= 160; width++ {
		rows := strings.Split(ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, width, -1)), "\n")[1:]
		var words []string
		for _, row := range rows {
			if w := ansi.StringWidth(strings.TrimRight(row, " ")); w > width-2 {
				t.Fatalf("width %d: row overflows (%d): %q", width, w, row)
			}
			words = append(words, strings.Fields(row)...)
		}
		if got := strings.Join(words, " "); got != strings.ReplaceAll(text, "`", "") {
			t.Fatalf("width %d: words were split or changed:\n%q", width, got)
		}
	}
	block := renderEntry(entry{Kind: "assistant", Text: "- one-two\n\n---\n\n| a-b | c |\n|---|---|\n| x | y |\n\nhttps://my-site.dev/a-b"}, 80, -1)
	plain := ansi.Strip(block)
	for _, want := range []string{"- one-two", "a-b", "│ c", "┼"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in %q", want, plain)
		}
	}
	if strings.Contains(block, "\u2011") || !strings.Contains(block, ansi.SetHyperlink("https://my-site.dev/a-b")) {
		t.Fatalf("hyphens not restored or URL not linked: %q", block)
	}
}

func TestStreamingMarkdownReusesCompletedBlocks(t *testing.T) {
	text := "# Title\n\nFirst paragraph with **bold**.\n\n```go\nx := 1\n\ny := 2\n```\n\nLast one"
	if split := stableSplit(text); text[split:] != "Last one" {
		t.Fatalf("split inside a fence or at the wrong place: %q", text[split:])
	}
	if stableSplit("one paragraph") != 0 || stableSplit("done\n\n") != 0 {
		t.Fatal("text without a following block must render in one piece")
	}
	streamed := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text, Preview: true}, 80, -1))
	final := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, 80, -1))
	if strings.Join(strings.Fields(streamed), " ") != strings.Join(strings.Fields(final), " ") {
		t.Fatalf("streamed render differs:\n%s\n---\n%s", streamed, final)
	}
	cached := mdStable.out
	renderEntry(entry{Kind: "assistant", Text: text + " grows", Preview: true}, 80, -1)
	if unsafe.StringData(cached) != unsafe.StringData(mdStable.out) {
		t.Fatal("completed blocks were rendered again")
	}
}

func TestSelectionWideCharacters(t *testing.T) {
	for _, c := range []struct {
		line     string
		from, to int
		want     string
	}{
		{"界ab", 2, 3, "ab"},
		{"界ab", 1, 2, "界a"},
		{"e\u0301xy", 1, 2, "xy"},
		{"e\u0301xy", 0, 1, "e\u0301x"},
		{"👍🏽 ok", 1, 2, "👍🏽"},
		{"ab  ", 0, 3, "ab"},
	} {
		s := selection{start: point{c.from, 0}, end: point{c.to, 0}}
		if got := s.text([]string{c.line}, 20); got != c.want {
			t.Errorf("%q [%d,%d]: %q, want %q", c.line, c.from, c.to, got, c.want)
		}
	}
}

func TestSelectionSpanAcrossRows(t *testing.T) {
	sel := selection{start: point{2, 1}, end: point{4, 3}}
	if _, _, ok := sel.span(0, 10); ok {
		t.Fatal("row before the selection must be untouched")
	}
	if from, to, ok := sel.span(1, 10); !ok || from != 2 || to != 10 {
		t.Fatalf("first row span = %d..%d %v", from, to, ok)
	}
	if from, to, ok := sel.span(2, 10); !ok || from != 0 || to != 10 {
		t.Fatalf("middle row span = %d..%d %v", from, to, ok)
	}
	if from, to, ok := sel.span(3, 10); !ok || from != 0 || to != 5 {
		t.Fatalf("last row span = %d..%d %v", from, to, ok)
	}
	if _, _, ok := sel.span(4, 10); ok {
		t.Fatal("row after the selection must be untouched")
	}
}

func TestComposerLayoutMatchesTextareaProbe(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pieces := []string{"a", "word", "longerword", " ", "  ", "界", "é", "-", "\n", "supercalifragilistic"}
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for n := r.Intn(30); n > 0; n-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		value, width := b.String(), 1+r.Intn(20)
		if got, want := composerLayout(value, width), probeLayout(value, width); !slices.Equal(got, want) {
			t.Fatalf("%q width %d: %v, textarea %v", value, width, got, want)
		}
	}
}

func TestNativeCursorPosition(t *testing.T) {
	m := bareModel(t, "/project")
	m.viewport.Width, m.viewport.Height = 20, 3
	m.viewport.lines = []string{"header", "▌ ab" + composerCursorMarker + "界", "footer"}
	view := m.View()
	if !strings.HasSuffix(view, "\x1b]1337;DashyCaret;2;5\x07") || strings.Contains(view, composerCursorMarker) {
		t.Fatalf("wrong native cursor position: %q", view)
	}
	m.viewport.YOffset = 2
	if got := m.View(); !strings.HasSuffix(got, "\x1b]1337;DashyCaret;0;0\x07") {
		t.Fatalf("offscreen cursor not hidden: %q", got)
	}
}

func TestCursorOutputRestoresRendererAndTerminal(t *testing.T) {
	var buf cursorBuffer
	out := &cursorOutput{ReadWriteCloser: &buf}
	write := func(s string) string {
		t.Helper()
		buf.Reset()
		if n, err := out.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("write: %d, %v", n, err)
		}
		return buf.String()
	}
	frame := "\x1b[?1049hhello\x1b]1337;DashyCaret;2;5\x07"
	got := write(frame)
	if strings.Contains(got, "DashyCaret") || !strings.HasSuffix(got, "\x1b7\x1b[6 q\x1b[2;5H\x1b[?25h") {
		t.Fatalf("native caret not positioned: %q", got)
	}
	got = write("next frame")
	if !strings.HasPrefix(got, "\x1b8next frame") {
		t.Fatalf("renderer position not restored: %q", got)
	}
	got = write("\x1b]1337;DashyCaret;0;0\x07")
	if !strings.HasSuffix(got, "\x1b[?25l") || out.saved {
		t.Fatalf("offscreen caret not hidden: %q", got)
	}
	got = write("\x1b[?1049l")
	if !strings.HasSuffix(got, "\x1b[0 q") || out.active || out.saved {
		t.Fatalf("terminal not restored: %q", got)
	}
}

func TestFindFileTokenBasics(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		cursor int
		wantOk bool
		wantQ  string
		wantS  int
		wantE  int
	}{
		{"empty", "", 0, false, "", 0, 0},
		{"just at", "@", 1, true, "", 0, 1},
		{"at with word", "@foo", 4, true, "foo", 0, 4},
		{"at with dir", "@dir/", 5, true, "dir/", 0, 5},
		{"cursor inside token", "check @dir/file more", 12, true, "dir/f", 6, 15},
		{"token with parens", "(@file)", 6, true, "file", 1, 6},
		{"token with quotes", "@\"my file.txt\"", 13, true, "my file.txt", 0, 14},
		{"multiline token", "line 1\nline 2 @src/main.go", 26, true, "src/main.go", 14, 26},
		{"space after token", "@foo ", 5, false, "", 0, 0},
		{"negative cursor", "@foo", -1, false, "", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, ok := findFileToken(tc.text, tc.cursor)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOk)
			}
			if ok {
				if token.query != tc.wantQ {
					t.Errorf("query = %q, want %q", token.query, tc.wantQ)
				}
				if token.start != tc.wantS || token.end != tc.wantE {
					t.Errorf("range = [%d, %d], want [%d, %d]", token.start, token.end, tc.wantS, tc.wantE)
				}
			}
		})
	}
}

func TestFilterRanking(t *testing.T) {
	indexed := []string{"nested/main.go", "main.go", "domain.go", "src/"}
	if got := filterFileCompletions(indexed, "main"); !reflect.DeepEqual(got, []string{"main.go", "domain.go"}) {
		t.Fatalf("ranking: %v", got)
	}
	if got := filterFileCompletions(indexed, ""); len(got) != 3 {
		t.Fatalf("only direct children: %v", got)
	}
	if got := filterFileCompletions([]string{"main.go", "sub/"}, "src/"); !reflect.DeepEqual(got, []string{"src/main.go", "src/sub/"}) {
		t.Fatalf("directory prefix: %v", got)
	}
}

func TestQuotedPathWithSpacesAcceptance(t *testing.T) {
	m := bareModel(t, t.TempDir())
	input := "look at @\"my fold"
	m.input.SetValue(input)
	m.input.CursorEnd()

	token, ok := findFileToken(input, len([]rune(input)))
	if !ok {
		t.Fatal("token not found")
	}

	m.fileCompletions = []string{"my folder/document.pdf"}
	m.fileTokenStart = token.start
	m.fileTokenEnd = token.end

	m.acceptFileCompletion()

	want := "look at @\"my folder/document.pdf\""
	if got := m.input.Value(); got != want {
		t.Fatalf("quoted path replacement = %q, want %q", got, want)
	}
}

func TestScanWalkBounded(t *testing.T) {
	dir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "src", "pkg", "app.go"), []byte("package pkg"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0644)

	_ = os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("git config"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, ".dashy"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".dashy", "state"), []byte("state"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("js"), 0644)

	_ = os.Symlink(filepath.Join(dir, "src"), filepath.Join(dir, "link_to_src"))

	ctx, cancel := context.WithTimeout(context.Background(), fileScanTimeout)
	defer cancel()

	paths, err := scanProjectFiles(ctx, dir)
	if err != nil {
		t.Fatalf("scanProjectFiles error: %v", err)
	}

	want := []string{".dashy/", ".git/", "link_to_src/", "node_modules/", "root.txt", "src/"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("direct children = %v, want %v", paths, want)
	}

}

func TestClipboardShortcutDecodesFromTerminalBytes(t *testing.T) {
	for _, sequence := range []string{"\x16", "\x1b[118;9u", "\x1b[1084;5u", "\x1b[1084::118;9u"} {
		m := bareModel(t, "/project")
		seen := false
		program := tea.NewProgram(m, tea.WithInput(strings.NewReader(sequence)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if key, ok := normalizeTerminalKey(msg).(tea.KeyMsg); ok && key.Type == tea.KeyCtrlV {
				seen = true
				return tea.QuitMsg{}
			}
			return msg
		}))
		if _, err := program.Run(); err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatalf("terminal sequence not recognized: %q", sequence)
		}
	}
}

func TestMenuShortcutDecodesFromTerminalBytes(t *testing.T) {
	for _, sequence := range []string{"\x0b", "\x1b[107;5u", "\x1b[1083;5u", "\x1b[1083::107;5u"} {
		seen := false
		program := tea.NewProgram(bareModel(t, "/project"), tea.WithInput(strings.NewReader(sequence)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if key, ok := normalizeTerminalKey(msg).(tea.KeyMsg); ok && key.Type == tea.KeyCtrlK {
				seen = true
				return tea.QuitMsg{}
			}
			return msg
		}))
		if _, err := program.Run(); err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatalf("terminal sequence not recognized: %q", sequence)
		}
	}
}

func TestMenuModelSearch(t *testing.T) {
	m := bareModel(t, "/project")
	m.models = []agent.ModelChoice{{Provider: "clip", Model: "claude-opus-5"}, {Provider: "clip", Model: "gpt-6-sol"}, {Provider: "other", Model: "claude-sonnet-5"}}
	m.menu = &menuState{page: menuModel, query: "CLAUDE 5"}
	if got := m.filteredModels(); len(got) != 2 || got[1].Model != "claude-sonnet-5" {
		t.Fatalf("filtered = %v", got)
	}
	m.menu.query = "other gpt"
	if got := m.menuRows(); len(got) != 1 || got[0].label != "No matches" {
		t.Fatalf("rows = %v", got)
	}
}

func TestChatShowsOnlyMessages(t *testing.T) {
	m := bareModel(t, "/project")
	m.width = 80
	m.entries = []agent.Record{
		{Kind: "user", Text: "go"},
		{Kind: "reasoning", Text: "thinking about it"},
		{Kind: "code", Text: "ls -la"},
		{Kind: "output", Text: "file.txt"},
		{Kind: "notice", Text: "<task-result id=\"t1\" status=\"done\"></task-result>"},
		{Kind: "assistant", Text: "done"},
		{Kind: "error", Text: "tool failure"},
	}
	view := ansi.Strip(m.timeline())
	for _, hidden := range []string{"thinking about it", "ls -la", "file.txt", "task-result", "tool failure"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("%q must stay out of the chat:\n%s", hidden, view)
		}
	}
	for _, shown := range []string{"go", "done"} {
		if !strings.Contains(view, shown) {
			t.Fatalf("%q missing from the chat:\n%s", shown, view)
		}
	}
}

func TestMenuInfoPage(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 80, 30
	info, err := m.ctl.Info()
	if err != nil {
		t.Fatal(err)
	}
	m.menu = &menuState{page: menuInfo, info: infoRows(info)}
	view := ansi.Strip(m.menuView())
	for _, want := range []string{"Menu/Info", "Session:", "Model:", "Configs:", "Context:", "none (defaults)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("%q missing in:\n%s", want, view)
		}
	}
}

func TestTimelineHidesTraceEntries(t *testing.T) {
	m := bareModel(t, "/project")
	m.width = 80
	for _, kind := range []string{"code", "output", "reasoning", "notice", "event", "user", "assistant"} {
		m.entries = append(m.entries, entry{Kind: kind, Text: kind + " text", Meta: &agent.Metadata{}})
	}
	m.timeline()
	for i, cached := range m.cache {
		text := ansi.Strip(cached.Text)
		kind := m.entries[i].Kind
		if kind == "user" || kind == "assistant" {
			if !strings.Contains(text, kind+" text") {
				t.Fatalf("%s is missing: %q", kind, text)
			}
			continue
		}
		if text != "" {
			t.Fatalf("%s must be hidden: %q", kind, text)
		}
	}
}

func TestTimelineCacheAndResize(t *testing.T) {
	m := bareModel(t, "/project")
	for i := 0; i < 100; i++ {
		m.add("assistant", fmt.Sprintf("**Answer %d**\n\n- bullet", i))
	}
	m.refresh(true)
	for m.renderPending {
		m.refresh(true)
	}
	cached := m.cache[0].Text
	m.applyAgent(agent.Event{Kind: "delta", ID: "stream", Text: "First"})
	m.refresh(true)
	if unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("completed entry was reformatted")
	}
	inputWidth, inputHeight := m.inputWidth, m.inputHeight
	second := agent.Event{Kind: "delta", ID: "stream", Text: " second"}
	m.applyAgent(second)
	next, _ := m.Update(agentEvent(second))
	m = flushFrame(next.(model))
	if m.inputWidth != inputWidth || m.inputHeight != inputHeight || unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("delta invalidated input layout or completed cache")
	}
	if !strings.Contains(m.cache[len(m.cache)-1].Text, "First second") {
		t.Fatal("stream cache not updated")
	}
	m.width = 40
	m.refresh(false)
	if m.cache[0].width != 40 || unsafe.StringData(cached) == unsafe.StringData(m.cache[0].Text) {
		t.Fatal("resize did not invalidate cache")
	}
}

func TestTermStateTurn(t *testing.T) {
	out := captureTerm(t)
	var ts termState
	ts.start("")
	if got := out.String(); got != "\x1b]9;4;3\x07\x1b]2;⠋ dashy\x07" {
		t.Fatalf("start = %q", got)
	}
	out.Reset()
	ts.rename("fix\nthe; bug")
	ts.rename("fix\nthe; bug")
	ts.tick()
	if got := out.String(); got != "\x1b]2;⠋ fix the bug\x07\x1b]2;⠙ fix the bug\x07" {
		t.Fatalf("rename/tick = %q", got)
	}
	out.Reset()
	ts.finish("ok", "fix the bug")
	if got := out.String(); got != "\x1b]9;4;0\x07\x1b]2;✓ fix the bug\x07" {
		t.Fatalf("done = %q", got)
	}
	out.Reset()
	ts.start("fix the bug")
	out.Reset()
	ts.finish("error", "fix the bug")
	got := out.String()
	for _, want := range []string{"\x1b]9;4;2;100\x07", "✗ fix the bug"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error finish missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "777;notify") {
		t.Fatalf("desktop notification sent: %q", got)
	}
	out.Reset()
	ts.reset()
	if got := out.String(); got != "\x1b]9;4;0\x07\x1b]2;\x07" {
		t.Fatalf("reset = %q", got)
	}
}

func TestCancelledTurnRestoresUnsentSteeringIntoComposer(t *testing.T) {
	m, _ := busyModel(t)
	m.input.SetValue("not accepted")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.requestCancel()
	m.input.SetValue("draft")
	m = awaitIdle(t, m)
	if m.input.Value() != "not accepted\n\ndraft" || len(m.ctl.Status().Queued) != 0 {
		t.Fatalf("pending text lost: %q", m.input.Value())
	}
}

func TestSteeringPreservesStepCount(t *testing.T) {
	m := bareModel(t, "/project")
	// Turn 1 starts
	next, _ := m.Update(control.Event{Kind: "started"})
	m = next.(model)
	m.applyAgent(agent.Event{Kind: "user", Text: "do task"})
	m.applyAgent(agent.Event{Kind: "reasoning", Text: "thinking"})
	m.applyAgent(agent.Event{Kind: "code", Text: "echo 1"})
	if got := m.turnSteps(); got != 2 {
		t.Fatalf("turnSteps before steer: got %d, want 2", got)
	}
	// Steering arrives during turn 1
	m.applyAgent(agent.Event{Kind: "user", Text: "steer: also do that"})
	if got := m.turnSteps(); got != 2 {
		t.Fatalf("turnSteps after steer: got %d, want 2 (must not restart)", got)
	}
	m.applyAgent(agent.Event{Kind: "code", Text: "echo 2"})
	if got := m.turnSteps(); got != 3 {
		t.Fatalf("turnSteps after second code: got %d, want 3", got)
	}
	// Turn 1 completes
	meta := &agent.Metadata{At: time.Now(), Duration: 2 * time.Second, Steps: 3}
	e := entry{Kind: "assistant", Text: "done", Meta: meta}
	if summary := answerMetadata(e); !strings.Contains(summary, "3 steps") {
		t.Fatalf("answerMetadata missing steps: %q", summary)
	}
	m.applyAgent(agent.Event{Kind: "assistant", Text: "done", Meta: meta})
	next, _ = m.Update(control.Event{Kind: "done"})
	m = next.(model)

	// Turn 2 starts
	next, _ = m.Update(control.Event{Kind: "started"})
	m = next.(model)
	m.applyAgent(agent.Event{Kind: "user", Text: "turn 2"})
	m.applyAgent(agent.Event{Kind: "code", Text: "echo 3"})
	if got := m.turnSteps(); got != 1 {
		t.Fatalf("turnSteps on turn 2: got %d, want 1", got)
	}
}

func TestCodeSpanLinksAndClicks(t *testing.T) {
	linkBase = "."
	out, ok := renderMarkdown("See `markdown.go:12`, `https://example.com/a-b` and `missing.go`.\n\n```\n`ui.go`\n```", 80, false)
	if !ok {
		t.Fatal("render failed")
	}
	rows := strings.Split(out, "\n")
	plain := ansi.Strip(rows[0])
	at := func(word string) string {
		return linkAt(rows[0], ansi.StringWidth(plain[:strings.Index(plain, word)])+1)
	}
	if got := at("markdown.go"); !strings.HasPrefix(got, "file://") || !strings.HasSuffix(got, "/markdown.go") {
		t.Errorf("file link: %q in %q", got, rows[0])
	}
	if got := at("https://example.com/a-b"); got != "https://example.com/a-b" {
		t.Errorf("url link: %q", got)
	}
	if got := at("missing.go"); got != "" {
		t.Errorf("missing file is linked: %q", got)
	}
	if strings.Contains(out, "\u2061") || strings.Contains(out, "\u2062") || strings.Count(out, "\x1b]8;") != 4 {
		t.Errorf("markers leaked or the fenced block was linked: %q", out)
	}
}

func TestQueuedComposerEditWithdrawsUntilEnter(t *testing.T) {
	m, release := busyModel(t)
	m.input.SetValue("queued instruction")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "queued instruction" || m.pendingSteer == 0 || len(m.ctl.Status().Queued) != 1 {
		t.Fatalf("queued draft disappeared: %q, %v", m.input.Value(), m.ctl.Status().Queued)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if len(m.ctl.Status().Queued) != 1 || m.pendingSteer == 0 {
		t.Fatal("Enter duplicated the message or cursor movement withdrew it")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!")})
	draft := m.input.Value()
	if m.pendingSteer != 0 || len(m.ctl.Status().Queued) != 0 || !strings.Contains(draft, "!") {
		t.Fatalf("edit did not withdraw queued input: %q, %v", draft, m.ctl.Status().Queued)
	}
	release()
	m = awaitIdle(t, m)
	if m.input.Value() != draft {
		t.Fatalf("edited draft lost: %q", m.input.Value())
	}
	for _, e := range m.entries {
		if e.Kind == "user" && strings.Contains(e.Text, "queued instruction") {
			t.Fatal("withdrawn message was sent without Enter")
		}
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = awaitIdle(t, m)
	var sent int
	for _, e := range m.entries {
		if e.Kind == "user" && e.Text == draft {
			sent++
		}
	}
	if sent != 1 || m.input.Value() != "" {
		t.Fatalf("edited draft not sent exactly once: %d, %q", sent, m.input.Value())
	}
}

func TestQueuedComposerAcceptedAndCancelled(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			m, release := busyModel(t)
			m.input.SetValue("pending message")
			m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
			if cancel {
				m.requestCancel()
			} else {
				release()
			}
			m = awaitIdle(t, m)
			want := ""
			if cancel {
				want = "pending message"
			}
			if m.input.Value() != want || m.pendingSteer != 0 {
				t.Fatalf("composer = %q, pending = %d", m.input.Value(), m.pendingSteer)
			}
		})
	}
}

func TestMenuSearchPreservesActionsAndComposer(t *testing.T) {
	m := bareModel(t, "/project")
	m.input.SetValue("keep this draft")
	m.openMenu()
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("model")})
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "Models" {
		t.Fatalf("main menu search: %v", rows)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuModel || m.menu.query != "" || m.input.Value() != "keep this draft" {
		t.Fatal("filtered main action opened the wrong page or overwrote the draft")
	}
	m.models = []agent.ModelChoice{{Provider: "p", Model: "m"}}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuEffort || agent.Efforts[m.menu.selected] != "medium" {
		t.Fatal("effort selection must start at medium")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("xhigh")})
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "xhigh" {
		t.Fatalf("effort search: %v", rows)
	}
	if indices := m.menuIndices(m.rawMenuRows()); len(indices) != 1 || agent.Efforts[indices[0]] != "xhigh" {
		t.Fatal("filtered effort lost its identity")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("missing")})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu == nil || m.menu.page != menuEffort {
		t.Fatal("empty search activated an action")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu.query != "" {
		t.Fatal("Esc did not clear search")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu.page != menuModel {
		t.Fatal("Esc did not return to Models")
	}
	m.menu = &menuState{page: menuResume, query: "second", sessions: []agent.SessionSummary{{ID: "a", Title: "first"}, {ID: "b", Title: "second"}}}
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "second" {
		t.Fatalf("resume search: %v", rows)
	}
	if indices := m.menuIndices(m.rawMenuRows()); len(indices) != 1 || m.menu.sessions[indices[0]].ID != "b" {
		t.Fatal("filtered session lost its identity")
	}
}

func TestMenuHeadersSearchCaretAndShortcutBytes(t *testing.T) {
	for _, tc := range []struct {
		sequence string
		page     menuPage
		shortcut string
	}{
		{"\x1b[109;5u", menuModel, "Ctrl+M"},
		{"\x1b[105;5u", menuInfo, "Ctrl+I"},
		{"\x12", menuResume, "Ctrl+R"},
		{"\x14", menuTracing, "Ctrl+T"},
		{"\x1b[116;5u", menuTracing, "Ctrl+T"},
		{"\x1b[1077:116;5u", menuTracing, "Ctrl+T"},
		{"\x1b[1100:109;5u", menuModel, "Ctrl+M"},
	} {
		t.Run(tc.shortcut+tc.sequence, func(t *testing.T) {
			m := bareModel(t, "/project")
			p := tea.NewProgram(m, tea.WithInput(strings.NewReader(tc.sequence+"\x03")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
				if k, ok := msg.(tea.KeyMsg); ok && k.Type == tea.KeyCtrlC {
					return tea.QuitMsg{}
				}
				return msg
			}))
			result, err := p.Run()
			if err != nil {
				t.Fatal(err)
			}
			m = result.(model)
			if m.menu == nil || m.menu.page != tc.page {
				t.Fatalf("shortcut opened wrong page: %v", m.menu)
			}
			view := ansi.Strip(m.menuView())
			header := strings.TrimSpace(strings.Split(strings.TrimSpace(view), "\n")[0])
			if !strings.HasPrefix(header, menuTitles[tc.page]) || !strings.HasSuffix(header, tc.shortcut) {
				t.Fatalf("header = %q", header)
			}
		})
	}
	m := bareModel(t, "/project")
	m.openMenu()
	view := m.menuView()
	if !strings.Contains(ansi.Strip(view), "dashy v"+strings.TrimPrefix(Version, "v")+"/Menu") || !strings.Contains(view, "▌") || strings.Contains(view, "DashyCaret;0;0") || strings.Contains(view, composerCursorMarker) {
		t.Fatalf("main header or search caret missing: %q", view)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mdel")})
	for range 3 {
		m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m.menu.query != "model" {
		t.Fatalf("search caret editing failed: %q", m.menu.query)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlK})
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.menu != nil {
		t.Fatal("legacy Tab must not become Ctrl+I")
	}
}

func TestAnswerHeaderOnlyDurationAndSteps(t *testing.T) {
	e := entry{Kind: "assistant", Text: "done", Meta: &agent.Metadata{At: time.Date(2026, 1, 1, 3, 16, 0, 0, time.UTC), Duration: 35500 * time.Millisecond, Steps: 3, Usage: &agent.Usage{Input: 91500, Output: 320}}}
	if got := answerMetadata(e); got != "36s · 3 steps" {
		t.Fatalf("metadata = %q", got)
	}
	view := ansi.Strip(renderEntry(e, 80, -1))
	if !strings.Contains(view, "* 36s · 3 steps") || strings.ContainsAny(view, "↑↓") || strings.Contains(view, "03:16") {
		t.Fatalf("answer header = %q", view)
	}
}

func TestMenuSearchGapBelowHeader(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 80, 24
	for _, page := range []menuPage{menuMain, menuResume, menuModel, menuEffort, menuTracing} {
		m.menu = &menuState{page: page}
		lines := strings.Split(ansi.Strip(m.menuView()), "\n")
		head := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) != "" })
		if head < 0 || !strings.Contains(lines[head], "Menu") {
			t.Fatalf("%v: header line missing: %q", page, lines)
		}
		if strings.TrimSpace(lines[head+1]) != "" || !strings.Contains(lines[head+2], "type to search") {
			t.Fatalf("%v: want one blank line between header and search, got %q %q", page, lines[head+1], lines[head+2])
		}
	}
}

func TestTracingPageListsSessionTrace(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 90, 30
	m.menu = &menuState{page: menuMain, selected: 4}
	if next, _ := m.menuActivate(); next != nil {
		m = next.(model)
	}
	if m.menu.page != menuTracing {
		t.Fatalf("Tracing row opened %v", m.menu.page)
	}
	m.menu.trace = []agent.TraceEntry{
		{Kind: "system", Text: "You are Dashy"},
		{Kind: "user", Text: "fix it"},
		{Kind: "bash", Text: "rg bug"},
		{Kind: "output", Text: "a\nb"},
	}
	view := ansi.Strip(m.menuView())
	for _, want := range []string{"Menu/Tracing", "type to search", "system", "user", "bash", "output"} {
		if !strings.Contains(view, want) {
			t.Fatalf("%q missing in:\n%s", want, view)
		}
	}
	m.menu.query = "rg bug"
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "bash" {
		t.Fatalf("trace search: %v", rows)
	}
	m.menu.query = ""
	m.menu.selected = 1
	next, _ := m.menuActivate()
	m = next.(model)
	if m.menu.page != menuText || m.menu.parent != menuTracing || !strings.Contains(m.menu.text, "fix it") {
		t.Fatalf("entry page = %+v", m.menu)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu == nil || m.menu.page != menuTracing {
		t.Fatalf("Esc from an entry left %+v", m.menu)
	}
}

func TestUserMessageWraps(t *testing.T) {
	text := strings.Repeat("word ", 40)
	view := ansi.Strip(renderEntry(entry{Kind: "user", Text: text}, 40, -1))
	rows := strings.Split(view, "\n")
	if len(rows) < 2 {
		t.Fatalf("user message did not wrap: %q", view)
	}
	for _, row := range rows {
		if ansi.StringWidth(row) > 40 {
			t.Fatalf("row overflows: %q", row)
		}
	}
	if got := strings.Join(strings.Fields(strings.ReplaceAll(view, "▌", " ")), " "); got != strings.TrimSpace(text) {
		t.Fatalf("text changed: %q", got)
	}
}

func TestCenteredMenuAndShortcutEscape(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 100, 50
	m.openMenu()
	want := (m.height - 3 - len(mainRows)) / 2
	if m.menuTop() != want {
		t.Fatalf("top %d, want %d", m.menuTop(), want)
	}
	for _, page := range []menuPage{menuModel, menuResume, menuTracing, menuInfo} {
		next, _ := m.openMenuPage(page)
		m = next.(model)
		m.menu.query = "typed search"
		m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
		if m.menu != nil {
			t.Fatalf("shortcut %v did not return to chat", page)
		}
	}
	m.openMenu()
	m.menu.selected = 6
	next, _ := m.menuActivate()
	m = key(next.(model), tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu == nil || m.menu.page != menuMain {
		t.Fatal("Menu/Info should return to Menu")
	}
}

func TestTracingNumbersBoundariesAndLiveText(t *testing.T) {
	m := bareModel(t, "/project")
	m.menu = &menuState{page: menuTracing, trace: []agent.TraceEntry{{Kind: "user", Text: "hi"}, {Kind: "assistant", Text: "hello"}}}
	for _, text := range []string{"1 user", "2 assistant"} {
		if !strings.Contains(ansi.Strip(m.menuView()), text) {
			t.Fatalf("missing %q", text)
		}
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.menu.selected != 0 {
		t.Fatal("Up wrapped")
	}
	m.menu.selected = 1
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.menu.selected != 1 {
		t.Fatal("Down wrapped")
	}
	m.entries = []entry{{Kind: "assistant", Text: "streaming", Preview: true}}
	m.refreshMenu()
	if len(m.menu.trace) == 0 || m.menu.trace[len(m.menu.trace)-1].Text != "streaming" {
		t.Fatal("streaming trace missing")
	}
	m.menu.selected = len(m.menu.trace) - 1
	next, _ := m.menuActivate()
	m = next.(model)
	m.entries[0].Text = "streaming answer"
	m.refreshMenu()
	if !strings.Contains(m.menu.text, "streaming answer") {
		t.Fatal("open trace detail is stale")
	}
}

func TestInfoRefreshAndCurrentModelFirst(t *testing.T) {
	m := bareModel(t, "/project")
	m.menu = &menuState{page: menuInfo}
	m.refreshMenu()
	if !strings.Contains(m.menuText(), "Session:") || strings.Contains(ansi.Strip(m.menuView()), " <") {
		t.Fatal("Info layout missing or has a selection arrow")
	}
	if err := m.ctl.Rename("updated title"); err != nil {
		t.Fatal(err)
	}
	m.refreshMenu()
	if !strings.Contains(m.menuText(), "updated title") {
		t.Fatal("Info did not refresh")
	}
	setConfig(t, m.ctl, func(s *agent.Settings) {
		s.Provider = "clip"
		s.Model = "current"
		s.Providers["clip"] = agent.Provider{BaseURL: "http://127.0.0.1:1/v1"}
	})
	m.menu = &menuState{page: menuModel}
	current := m.ctl.Settings()
	m.models = []agent.ModelChoice{{Provider: "other", Model: "other"}, {Provider: current.Provider, Model: current.Model}}
	choices := m.filteredModels()
	if len(choices) != 2 || choices[0].Provider != current.Provider || choices[0].Model != current.Model {
		t.Fatalf("choices: %+v", choices)
	}
}

func TestInputHasNoLineLimitAndUsesAvailableScreen(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 80, 40
	m.setComposer(strings.Repeat("line\n", 30))
	m.refresh(false)
	if m.input.Height() != 31 || m.input.MaxHeight != 0 {
		t.Fatalf("height %d, max %d", m.input.Height(), m.input.MaxHeight)
	}
	m.setComposer(strings.Repeat("line\n", 500))
	if m.input.LineCount() != 501 {
		t.Fatalf("input truncated: %d", m.input.LineCount())
	}
}

func TestExternalEditorReturnsSavedDraft(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$(cat \"$1\")\" = original ] || exit 4\nprintf 'edited\\ntext' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "sh "+script)
	m := bareModel(t, dir)
	m.setComposer("original")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p *tea.Program
	var editorErr error
	p = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(strings.NewReader("\x05")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if e, ok := msg.(editorFinished); ok {
			editorErr = e.err
			go p.Send(tea.QuitMsg{})
		}
		return msg
	}))
	result, err := p.Run()
	if err != nil || editorErr != nil {
		t.Fatalf("run: %v; editor: %v", err, editorErr)
	}
	got := result.(model)
	if got.input.Value() != "edited\ntext" || got.ctl.Status().Busy {
		t.Fatalf("editor did not restore draft: %q", got.input.Value())
	}
}

func TestTaskWindowsRefreshWithoutReopening(t *testing.T) {
	m, _ := freshModel(t)
	t.Cleanup(m.ctl.Close)
	if _, err := agent.RunTask(m.cwd, m.ctl.Status().Session, "sleep 600"); err != nil {
		t.Fatal(err)
	}
	m.menu = &menuState{page: menuTasks}
	m.refreshMenu()
	if len(m.menu.tasks) != 1 {
		t.Fatalf("tasks: %+v", m.menu.tasks)
	}
	task := m.menu.tasks[0]
	defer m.ctl.StopTask(task.ID)
	m.menu = &menuState{page: menuText, parent: menuTask, task: task.ID, offset: 1}
	if err := os.WriteFile(task.Log, []byte("first line\nnew output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m.refreshMenu()
	if !strings.Contains(m.menu.text, "new output") || m.menu.offset != 1 {
		t.Fatal("task output did not refresh or lost its scroll position")
	}
	if err := m.ctl.StopTask(task.ID); err != nil {
		t.Fatal(err)
	}
	m.menu = &menuState{page: menuTasks}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.refreshMenu()
		if len(m.menu.tasks) == 1 && m.menu.tasks[0].Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task status did not refresh")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClientErrorsAndNoticesOnlyAppearInTracing(t *testing.T) {
	m := bareModel(t, "/project")
	m.add("error", "client failure")
	next, _ := m.Update(control.Event{Kind: "error", Text: "agent failure"})
	m = next.(model)
	next, _ = m.Update(control.Event{Kind: "notice", Text: "status notice"})
	m = next.(model)
	for _, text := range []string{"client failure", "agent failure", "status notice"} {
		if strings.Contains(ansi.Strip(m.timeline()), text) {
			t.Fatalf("event leaked into chat: %s", text)
		}
		trace := m.ctl.Trace()
		if !slices.ContainsFunc(trace, func(e agent.TraceEntry) bool { return e.Text == text }) {
			t.Fatalf("event missing in Tracing: %s", text)
		}
	}
}

func TestDurationUsesSharedClockFormat(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     string
	}{
		{0, "0s"}, {35500 * time.Millisecond, "36s"},
		{59900 * time.Millisecond, "1m0s"}, {680600 * time.Millisecond, "11m21s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h2m3s"},
	} {
		if got := formatDuration(tc.duration); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.duration, got, tc.want)
		}
	}
}

func TestEditorReleasesKeyboardProtocol(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = old; reader.Close(); writer.Close() }()
	keyboard := keyboardProtocol{enabled: true}
	current := model{editorActive: true}
	message := tea.KeyMsg{Type: tea.KeyCtrlE}
	keyboard.filter(current, message)
	if keyboard.enabled {
		t.Fatal("Dashy keyboard protocol left active for editor")
	}
	keyboard.filter(current, editorFinished{})
	if keyboard.enabled {
		t.Fatal("protocol restored before editor completion was processed")
	}
	keyboard.filter(model{}, redraw{})
	if !keyboard.enabled {
		t.Fatal("keyboard protocol not restored after editor")
	}
	writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "\x1b[<u\x1b[>5u" {
		t.Fatalf("keyboard handoff: %q, %v", data, err)
	}
}
