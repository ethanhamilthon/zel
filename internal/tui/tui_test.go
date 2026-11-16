package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"

	"zel/internal/config"
	"zel/internal/core"
	"zel/internal/ipc"
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
			if w := ansi.StringWidth(strings.TrimRight(row, " ")); w > width {
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
	m.viewport.TopMargin = 1
	m.viewport.SetLines([]string{"header", "▌ ab" + composerCursorMarker + "界", "footer"})
	view := m.View()
	if !strings.HasSuffix(view, "\x1b]1337;ZelCaret;3;5\x07") || strings.Contains(view, composerCursorMarker) {
		t.Fatalf("wrong native cursor position: %q", view)
	}
	m.viewport.YOffset = 2
	if got := m.View(); !strings.HasSuffix(got, "\x1b]1337;ZelCaret;0;0\x07") {
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
	frame := "\x1b[?1049hhello\x1b]1337;ZelCaret;2;5\x07"
	got := write(frame)
	if strings.Contains(got, "ZelCaret") || !strings.HasSuffix(got, "\x1b7\x1b[6 q\x1b[2;5H\x1b[?25h") {
		t.Fatalf("native caret not positioned: %q", got)
	}
	got = write("next frame")
	if !strings.HasPrefix(got, "\x1b8next frame") {
		t.Fatalf("renderer position not restored: %q", got)
	}
	got = write("\x1b]1337;ZelCaret;0;0\x07")
	if !strings.HasSuffix(got, "\x1b[?25l") || out.saved {
		t.Fatalf("offscreen caret not hidden: %q", got)
	}
	got = write("\x1b[?1049l")
	if !strings.HasSuffix(got, "\x1b[0 q") || out.active || out.saved {
		t.Fatalf("terminal not restored: %q", got)
	}
}

// Moving content must scroll the terminal rather than repaint it, so a text
// selection survives while the chat follows a working agent.
func TestDisplayScrollsInsteadOfRepainting(t *testing.T) {
	var buf strings.Builder
	d := &display{out: &buf}
	d.draw("a\nb\nc\nd\x1b]1337;ZelCaret;4;1\x07", 10, 4)
	buf.Reset()
	d.draw("c\nd\ne\nf\x1b]1337;ZelCaret;4;1\x07", 10, 4)
	if got := buf.String(); got != "\x1b[m\x1b[2S\x1b[3;1He\x1b[m\x1b[K\x1b[4;1Hf\x1b[m\x1b[K\x1b]1337;ZelCaret;4;1\x07" {
		t.Fatalf("scroll up = %q", got)
	}
	buf.Reset()
	d.draw("b\nc\nd\ne\x1b]1337;ZelCaret;4;1\x07", 10, 4)
	if got := buf.String(); !strings.HasPrefix(got, "\x1b[m\x1b[1T\x1b[1;1Hb") || strings.Contains(got, "1Hc") {
		t.Fatalf("scroll down = %q", got)
	}
	buf.Reset()
	d.draw("b\nc\nd\ne\x1b]1337;ZelCaret;4;1\x07", 10, 4)
	if buf.Len() != 0 {
		t.Fatalf("unchanged frame wrote %q", buf.String())
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
	_ = os.MkdirAll(filepath.Join(dir, ".zel"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".zel", "state"), []byte("state"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("js"), 0644)

	_ = os.Symlink(filepath.Join(dir, "src"), filepath.Join(dir, "link_to_src"))

	ctx, cancel := context.WithTimeout(context.Background(), fileScanTimeout)
	defer cancel()

	paths, err := scanProjectFiles(ctx, dir)
	if err != nil {
		t.Fatalf("scanProjectFiles error: %v", err)
	}

	want := []string{".git/", ".zel/", "link_to_src/", "node_modules/", "root.txt", "src/"}
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

func TestCtrlADecodesFromTerminalBytes(t *testing.T) {
	for _, sequence := range []string{"\x01", "\x1b[97;5u", "\x1b[1092;5u", "\x1b[1060;5u"} {
		seen := false
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		program := tea.NewProgram(bareModel(t, t.TempDir()), tea.WithContext(ctx), tea.WithInput(strings.NewReader(sequence)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if key, ok := normalizeTerminalKey(msg).(tea.KeyMsg); ok && key.Type == tea.KeyCtrlA {
				seen = true
				return tea.QuitMsg{}
			}
			return msg
		}))
		_, err := program.Run()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Errorf("Ctrl+A terminal sequence not recognized: %q", sequence)
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
	m.models = []core.ModelChoice{{Provider: "clip", Model: "claude-opus-5"}, {Provider: "clip", Model: "gpt-6-sol"}, {Provider: "other", Model: "claude-sonnet-5"}}
	m.menu = &menuState{page: menuModel, query: "CLAUDE"}
	if got := m.filteredModels(); len(got) != 2 || got[1].Model != "claude-sonnet-5" {
		t.Fatalf("filtered = %v", got)
	}
	m.menu.query = "other gpt"
	if got := m.menuRows(); len(got) != 3 || got[0].label != "No matches" {
		t.Fatalf("rows = %v", got)
	}
}

func TestFuzzySearchAcceptance(t *testing.T) {
	m := bareModel(t, "/project")
	m.models = []core.ModelChoice{
		{Provider: "openai", Model: "gpt-5.1"},
		{Provider: "anthropic", Model: "claude-3-7-sonnet"},
	}

	// 1. Model menu: fuzzy query 'gpt5' matches 'gpt-5.1'
	m.menu = &menuState{page: menuModel, query: "gpt5"}
	if got := m.filteredModels(); len(got) != 1 || got[0].Model != "gpt-5.1" {
		t.Fatalf("expected gpt-5.1 to match 'gpt5', got %v", got)
	}

	// 2. Main menu: still strict (prefix only)
	m.menu = &menuState{page: menuMain, query: "es"}
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "No matches" {
		t.Fatalf("main menu must not match 'es' non-strictly: %v", rows)
	}
	m.menu.query = "res"
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "Resume" {
		t.Fatalf("main menu must match 'res' prefix: %v", rows)
	}

	// 4. File autocomplete completion: fuzzy
	if got := filterFileCompletions([]string{"nested/main.go", "gpt-5.1.go", "main.go"}, "gpt5"); !reflect.DeepEqual(got, []string{"gpt-5.1.go"}) {
		t.Fatalf("file completions must match 'gpt5' fuzzily, got %v", got)
	}
}

func TestChatShowsOnlyMessages(t *testing.T) {
	m := bareModel(t, "/project")
	m.width = 80
	m.entries = []core.Record{
		{Kind: "user", Text: "go"},
		{Kind: "reasoning", Text: "thinking about it"},
		{Kind: "code", Text: "ls -la"},
		{Kind: "output", Text: "file.txt"},
		{Kind: "notice", Text: "<notification>t1 finished</notification>"},
		{Kind: "assistant", Text: "done"},
		{Kind: "error", Text: "tool failure"},
	}
	view := ansi.Strip(m.timeline())
	for _, hidden := range []string{"thinking about it", "ls -la", "file.txt", "t1 finished", "tool failure"} {
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
	for _, want := range []string{"ID:", "Dir:", "Model:", "Context:", "Spend:"} {
		if !strings.Contains(view, want) {
			t.Fatalf("%q missing in:\n%s", want, view)
		}
	}
}

func TestTimelineHidesTraceEntries(t *testing.T) {
	m := bareModel(t, "/project")
	m.width = 80
	for _, kind := range []string{"code", "output", "reasoning", "notice", "event", "user", "assistant"} {
		m.entries = append(m.entries, entry{Kind: kind, Text: kind + " text", Meta: &core.Metadata{}})
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
	m.applyAgent(core.TurnEvent{Kind: "delta", ID: "stream", Text: "First"})
	m.refresh(true)
	if unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("completed entry was reformatted")
	}
	inputWidth, inputHeight := m.inputWidth, m.inputHeight
	second := core.TurnEvent{Kind: "delta", ID: "stream", Text: " second"}
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
	next, _ := m.Update(core.Event{Kind: "started"})
	m = next.(model)
	m.applyAgent(core.TurnEvent{Kind: "user", Text: "do task"})
	m.applyAgent(core.TurnEvent{Kind: "reasoning", Text: "thinking"})
	m.applyAgent(core.TurnEvent{Kind: "code", Text: "echo 1"})
	if got := m.turnSteps(); got != 2 {
		t.Fatalf("turnSteps before steer: got %d, want 2", got)
	}
	// Steering arrives during turn 1
	m.applyAgent(core.TurnEvent{Kind: "user", Text: "steer: also do that"})
	if got := m.turnSteps(); got != 2 {
		t.Fatalf("turnSteps after steer: got %d, want 2 (must not restart)", got)
	}
	m.applyAgent(core.TurnEvent{Kind: "code", Text: "echo 2"})
	if got := m.turnSteps(); got != 3 {
		t.Fatalf("turnSteps after second code: got %d, want 3", got)
	}
	m.applyAgent(core.TurnEvent{Kind: "assistant", Text: "Still working"})
	if got := m.turnSteps(); got != 3 {
		t.Fatalf("steps reset after intermediate answer: %d", got)
	}
	m.applyAgent(core.TurnEvent{Kind: "reasoning", Text: "continue"})
	if got := m.turnSteps(); got != 4 {
		t.Fatalf("steps after intermediate answer: %d", got)
	}
	// Turn 1 completes
	meta := &core.Metadata{At: time.Now(), Duration: 2 * time.Second, Steps: 4}
	e := entry{Kind: "assistant", Text: "done", Meta: meta}
	if summary := answerMetadata(e); !strings.Contains(summary, "4 steps") {
		t.Fatalf("answerMetadata missing steps: %q", summary)
	}
	m.applyAgent(core.TurnEvent{Kind: "assistant", Text: "done", Meta: meta})
	next, _ = m.Update(core.Event{Kind: "done"})
	m = next.(model)

	// Turn 2 starts
	next, _ = m.Update(core.Event{Kind: "started"})
	m = next.(model)
	m.applyAgent(core.TurnEvent{Kind: "user", Text: "turn 2"})
	m.applyAgent(core.TurnEvent{Kind: "code", Text: "echo 3"})
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

func TestQueuedComposerPulse(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(profile)
	m := bareModel(t, "/project")
	m.input.SetValue("queued message\nsecond line")
	bright, dim := time.UnixMilli(0), time.UnixMilli(800)
	normal := m.inputBlockAt(bright)
	if normal != m.inputBlockAt(dim) {
		t.Fatal("unqueued composer animates")
	}
	m.pendingSteer = 1
	a, b := m.inputBlockAt(bright), m.inputBlockAt(dim)
	if a == b || ansi.Strip(a) != ansi.Strip(b) {
		t.Fatal("queued composer must change color without changing content")
	}
	if a != m.inputBlockAt(time.UnixMilli(1600)) {
		t.Fatal("pulse does not return to its initial color")
	}
	m.pendingSteer = 0
	if got := m.inputBlockAt(dim); got != normal {
		t.Fatal("composer style was not restored after leaving queue")
	}
	if queuedColor("invalid", dim) != "invalid" {
		t.Fatal("invalid colors should remain unchanged")
	}
}

func TestQueuedComposerEditWithdrawsUntilEnter(t *testing.T) {
	m, release := busyModel(t)
	m.input.SetValue("queued instruction")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "queued instruction" || m.pendingSteer == 0 || len(m.ctl.Status().Queued) != 1 {
		t.Fatalf("queued draft disappeared: %q, %v", m.input.Value(), m.ctl.Status().Queued)
	}
	if !strings.HasSuffix(ansi.Strip(m.workingBlock()), " · queued · thinking...\n\n") {
		t.Fatalf("working indicator does not mention the queue: %q", ansi.Strip(m.workingBlock()))
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
	m.menu.query, m.menu.search = "i", nil
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "Info" {
		t.Fatalf("main menu search must match labels by prefix only: %v", rows)
	}
	m.menu.query = "model"
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuModel || m.menu.query != "" || m.input.Value() != "keep this draft" {
		t.Fatal("filtered main action opened the wrong page or overwrote the draft")
	}
	m.models = []core.ModelChoice{{Provider: "p", Model: "m"}}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuEffort || core.Efforts[m.menu.selected] != "medium" {
		t.Fatal("effort selection must start at medium")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("xhigh")})
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "xhigh" {
		t.Fatalf("effort search: %v", rows)
	}
	if indices := m.menuIndices(m.rawMenuRows()); len(indices) != 1 || core.Efforts[indices[0]] != "xhigh" {
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
	m.menu = &menuState{page: menuResume, query: "second", sessions: []core.SessionSummary{{ID: "a", Title: "first"}, {ID: "b", Title: "second"}}}
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
			title := menuTitles[tc.page]
			if tc.page == menuInfo {
				title = "Session" // the Info page is titled with the session name
			}
			if !strings.HasPrefix(header, title) || !strings.HasSuffix(header, tc.shortcut) {
				t.Fatalf("header = %q", header)
			}
		})
	}
	m := bareModel(t, "/project")
	m.openMenu()
	oldVersion := Version
	Version = "0.3.3"
	t.Cleanup(func() { Version = oldVersion })
	view := m.menuView()
	mainHeader := strings.TrimSpace(strings.Split(ansi.Strip(view), "\n")[m.menuTop()])
	if !strings.HasPrefix(mainHeader, "zel 0.3.3") || !strings.HasSuffix(mainHeader, "Ctrl+K") || strings.Contains(mainHeader, "/Menu") || !strings.Contains(view, "▌") || strings.Contains(view, "ZelCaret;0;0") || strings.Contains(view, composerCursorMarker) {
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
	e := entry{Kind: "assistant", Text: "done", Meta: &core.Metadata{At: time.Date(2026, 1, 1, 3, 16, 0, 0, time.UTC), Duration: 35500 * time.Millisecond, Steps: 3, Usage: &core.Usage{Input: 91500, Output: 320}}}
	if got := answerMetadata(e); got != "36s · 3 steps" {
		t.Fatalf("metadata = %q", got)
	}
	view := ansi.Strip(renderEntry(e, 80, -1))
	if !strings.Contains(view, "* 36s · 3 steps") || strings.ContainsAny(view, "↑↓") || strings.Contains(view, "03:16") {
		t.Fatalf("answer header = %q", view)
	}
}

func TestTurnEndClosesInChatWithTimeStepsAndReason(t *testing.T) {
	e := entry{Kind: "turn_end", Text: "stopped by user", Meta: &core.Metadata{Duration: 17 * time.Second, Steps: 5}}
	lines := strings.Split(ansi.Strip(renderEntry(e, 80, -1)), "\n")
	if len(lines) != 2 || lines[0] != "* 17s · 5 steps" || lines[1] != "stopped by user" {
		t.Fatalf("closing rows = %q", lines)
	}
	m := bareModel(t, "/project")
	m.width = 80
	m.entries = []core.Record{{Kind: "user", Text: "go"}, e}
	view := ansi.Strip(m.timeline())
	if !strings.Contains(view, "* 17s · 5 steps") || !strings.Contains(view, "stopped by user") {
		t.Fatalf("closing missing from the chat:\n%s", view)
	}
}

func TestMenuSearchGapBelowHeader(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 80, 24
	for _, page := range []menuPage{menuMain, menuResume, menuModel, menuEffort, menuTracing} {
		m.menu = &menuState{page: page}
		lines := strings.Split(ansi.Strip(m.menuView()), "\n")
		head := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) != "" })
		if head < 0 || (page == menuMain && !strings.HasPrefix(strings.TrimSpace(lines[head]), "zel ")) || (page != menuMain && !strings.Contains(lines[head], "Menu")) {
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
	m.menu = &menuState{page: menuMain, selected: 3}
	if next, _ := m.menuActivate(); next != nil {
		m = next.(model)
	}
	if m.menu.page != menuTracing {
		t.Fatalf("Tracing row opened %v", m.menu.page)
	}
	m.menu.trace = []core.TraceEntry{
		{Kind: "system", Text: "You are Zel"},
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
	m.menu.query = "BA"
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "bash" {
		t.Fatalf("trace label-prefix search: %v", rows)
	}
	m.menu.query = "BA :bash >3"
	if indices := m.menuIndices(m.rawMenuRows()); !reflect.DeepEqual(indices, []int{2}) {
		t.Fatalf("trace label search lost type/index filters: %v", indices)
	}
	m.menu.query = "rg bug"
	if indices := m.menuIndices(m.rawMenuRows()); !reflect.DeepEqual(indices, []int{2}) {
		t.Fatalf("trace search failed to match body text, indices %v", indices)
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

func TestTraceEntryUpdateDisablesMouseOnEveryExit(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlK} {
		t.Run(key.String(), func(t *testing.T) {
			m := bareModel(t, "/project")
			m.menu = &menuState{
				page: menuText, parent: menuTracing, traceIndex: 0,
				trace: []core.TraceEntry{{Kind: "user", Text: "hello"}},
			}
			next, cmd := m.Update(tea.KeyMsg{Type: key})
			updated := next.(model)
			if key == tea.KeyEsc {
				if updated.menu == nil || updated.menu.page != menuTracing {
					t.Fatalf("Escape state = %+v, want tracing list", updated.menu)
				}
			} else if updated.menu != nil {
				t.Fatalf("Ctrl+K left menu open: %+v", updated.menu)
			}
			if !slices.Contains(commandMessages(cmd), "tea.disableMouseMsg") {
				t.Fatalf("exit command = %T, want mouse disable", cmd)
			}

			if key == tea.KeyEsc {
				_, cmd = updated.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
				if cmd != nil {
					t.Fatalf("closing tracing list returned unexpected command %T", cmd)
				}
			}
		})
	}
}

func TestMenuBodyRoomKeepsTraceLimit(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 80, 30
	m.menu = &menuState{
		page: menuText, parent: menuTracing, traceIndex: 0,
		trace: []core.TraceEntry{{Kind: "user", Text: strings.Repeat("line\n", 30)}},
	}
	wantTraceRoom := min(10, max(1, m.height-m.menuBodyStart()))
	if got := m.menuBodyRoom(); got != wantTraceRoom {
		t.Fatalf("trace body room = %d, want %d", got, wantTraceRoom)
	}
	if got := len(m.pageLines("", m.menuWidth())); got > 10 {
		t.Fatalf("trace viewport has %d rows, want at most 10", got)
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
	if strings.Contains(view, "▌") {
		t.Fatalf("user message must not contain rail: %q", view)
	}
	if got := strings.Join(strings.Fields(view), " "); got != strings.TrimSpace(text) {
		t.Fatalf("text changed: %q", got)
	}

	raw := renderEntry(entry{Kind: "user", Text: "dark gray test"}, 40, -1)
	wantColor := lipgloss.NewStyle().Foreground(darkGray).Render("dark gray test")
	if raw != wantColor {
		t.Fatalf("user message color mismatch:\ngot  %q\nwant %q", raw, wantColor)
	}

	assistantRaw := renderEntry(entry{Kind: "assistant", Text: "answer text"}, 40, -1)
	for _, row := range strings.Split(ansi.Strip(assistantRaw), "\n") {
		if strings.HasPrefix(row, " ") {
			t.Fatalf("assistant row must not have leading space: %q", row)
		}
	}

	f, release := busyModel(t)
	defer release()
	if wb := f.workingBlock(); strings.HasPrefix(wb, " ") || strings.Contains(wb, "▌") {
		t.Fatalf("working block must not have leading space or rail: %q", wb)
	}

	m := bareModel(t, "/project")
	m.input.SetValue("user input")
	m.refresh(false)
	if ib := m.inputBlock(); strings.Contains(ib, "▌") {
		t.Fatalf("input block must not contain rail: %q", ib)
	}
}

func TestCenteredMenuAndShortcutEscape(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 100, 50
	m.openMenu()
	want := (m.height - 4 - len(mainRows)) / 2
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
	m.menu.selected = 5
	next, _ := m.menuActivate()
	m = key(next.(model), tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu == nil || m.menu.page != menuMain {
		t.Fatal("Menu/Info should return to Menu")
	}
}

func TestTracingNumbersBoundariesAndLiveText(t *testing.T) {
	m := bareModel(t, "/project")
	m.menu = &menuState{page: menuTracing, trace: []core.TraceEntry{{Kind: "user", Text: "hi"}, {Kind: "assistant", Text: "hello"}}}
	for _, text := range []string{"1 user", "2 agent"} {
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
	if !strings.Contains(m.menuText(), "ID:") || strings.Contains(ansi.Strip(m.menuView()), " <") {
		t.Fatal("Info layout missing or has a selection arrow")
	}
	if err := m.ctl.Rename("updated title"); err != nil {
		t.Fatal(err)
	}
	m.refreshMenu()
	if !strings.Contains(m.menuText(), "updated title") {
		t.Fatal("Info did not refresh")
	}
	setConfig(t, m.ctl, func(s *core.Settings) {
		s.Provider = "clip"
		s.Model = "current"
		s.Providers["clip"] = core.Provider{BaseURL: "http://127.0.0.1:1/v1"}
	})
	m.menu = &menuState{page: menuModel}
	current := m.ctl.Settings()
	m.models = []core.ModelChoice{{Provider: "other", Model: "other"}, {Provider: current.Provider, Model: current.Model}}
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

// Each running task has its own row under the chat: a blinking dot, its name and
// how long it runs. The rows animate, and they go away when the tasks end.
func TestTaskRowsShowUnderTheChat(t *testing.T) {
	m := bareModel(t, "/project")
	m.width, m.height = 60, 20
	m.tasks = []core.ActiveTask{{ID: "t1", Name: "build checks", Started: time.Now().Add(-4*time.Minute - 8*time.Second)}, {ID: "t2", Started: time.Now()}}
	block := ansi.Strip(m.taskBlock())
	if !strings.Contains(block, "build checks · 4m8s") || !strings.Contains(block, "t2 · ") || strings.Contains(block, "◆") {
		t.Fatalf("rows: %q", block)
	}
	if !m.hasRows() {
		t.Fatal("task rows must keep the indicator animating")
	}
	shown := ansi.Strip(m.formatTaskRow(core.ActiveTask{Name: "x", Started: time.Now()}))
	hidden := ansi.Strip(m.formatTaskRow(core.ActiveTask{Name: "x", Started: time.Now().Add(-taskBlink - 10*time.Millisecond)}))
	if !strings.HasPrefix(shown, "●") || strings.HasPrefix(hidden, "●") {
		t.Fatalf("the dot does not blink: %q / %q", shown, hidden)
	}
	m.tasks = nil
	if m.taskBlock() != "" || m.hasRows() {
		t.Fatal("rows stay after the tasks ended")
	}
}

func TestClientErrorsAndNoticesOnlyAppearInTracing(t *testing.T) {
	m := bareModel(t, "/project")
	m.add("error", "client failure")
	next, _ := m.Update(core.Event{Kind: "error", Text: "agent failure"})
	m = next.(model)
	next, _ = m.Update(core.Event{Kind: "notice", Text: "status notice"})
	m = next.(model)
	for _, text := range []string{"client failure", "agent failure", "status notice"} {
		if strings.Contains(ansi.Strip(m.timeline()), text) {
			t.Fatalf("event leaked into chat: %s", text)
		}
		trace := m.ctl.Trace()
		if !slices.ContainsFunc(trace, func(e core.TraceEntry) bool { return e.Text == text }) {
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

func TestOnboardingStepsPersistProviderAndSelectModel(t *testing.T) {
	f := newFixture(t, false)
	m := f.m
	if m.menu == nil || m.menu.page != menuForm || !m.menu.form.onboarding {
		t.Fatal("first-run onboarding missing")
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "Welcome to Zel") || !strings.Contains(view, "Tab / Shift+Tab") {
		t.Fatalf("onboarding hints missing: %s", view)
	}
	m.menu.form.input.SetValue("chat")
	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.menu.form.step != 0 {
		t.Fatal("left arrow changed step")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.menu.form.step != 1 || m.menu.form.values[0] != "chat" {
		t.Fatal("Tab did not preserve field")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.menu.form.step != 0 || m.menu.form.input.Value() != "chat" {
		t.Fatal("Shift+Tab did not restore field")
	}
	values := []string{core.APIChat, "http://127.0.0.1:1/v1", "private-test-key", "local"}
	for i, value := range values {
		m.menu.form.input.SetValue(value)
		if i == 2 && !strings.Contains(ansi.Strip(m.View()), value) {
			t.Fatal("API key must be visible in form")
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	if m.menu.page != menuModel {
		t.Fatal("provider form did not advance to model selection")
	}
	s := m.ctl.Settings()
	if p := s.Providers["local"]; p.API != core.APIChat || p.APIKey != "private-test-key" {
		t.Fatalf("provider: %+v", p)
	}
	m.openManualModel()
	m.menu.form.input.SetValue("local")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.menu.form.input.SetValue("custom-model")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuEffort {
		t.Fatal("manual model did not open effort picker")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu != nil || m.ctl.Settings().Model != "custom-model" || m.ctl.Settings().Effort != "medium" {
		t.Fatalf("selection: %+v", m.ctl.Settings())
	}
}

func TestProvidersCtrlAStartsAddFlowWithoutSelectingListItem(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.ctl.Settings().Providers["existing"] = core.Provider{BaseURL: "http://127.0.0.1:1/v1"}
	m.menu = &menuState{page: menuProviders, provider: "existing", selected: 0}
	next, _ := m.menuKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = next.(model)
	if m.menu == nil || m.menu.page != menuForm || m.menu.form == nil || m.menu.form.step != 0 {
		t.Fatalf("Ctrl+A should open the add-provider form, not activate the selected provider: %#v", m.menu)
	}
	if m.menu.form.values[3] != "" {
		t.Fatalf("Ctrl+A opened an edit flow for %q instead of a new provider", m.menu.form.values[3])
	}
}

func TestProviderAndSettingsMenusValidateAndPersist(t *testing.T) {
	f := newFixture(t, true)
	m := f.m
	m.menu = &menuState{}
	m.openProviderForm("p", false)
	m.menu.form.input.SetValue(core.APIChat)
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.menu.form.input.SetValue("invalid")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.form.step != 1 || m.menu.form.err == "" {
		t.Fatal("invalid provider URL accepted")
	}
	m.menu.page = menuSettings
	m.openSettingForm(0)
	m.menu.form.input.SetValue("4096")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuSettings || m.ctl.Settings().CompactionThreshold != 4096 {
		t.Fatal("compaction preference not saved")
	}
	m.openSettingForm(0)
	m.menu.form.input.SetValue("-1")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuForm || m.menu.form.err == "" {
		t.Fatal("invalid compaction threshold accepted")
	}
	m.menu = &menuState{page: menuSettings, selected: 4}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuForm || m.menu.form.input.Value() != "30" {
		t.Fatalf("Bash timeout form: page %v", m.menu.page)
	}
	m.menu.form.input.SetValue("90")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuSettings || m.ctl.Settings().BashTimeout != 90 || m.menuRows()[4] != (menuRow{"Bash timeout", "90 s"}) {
		t.Fatalf("bash timeout not applied: %d %v", m.ctl.Settings().BashTimeout, m.menuRows())
	}
	m.menu = &menuState{page: menuSettings, selected: 5}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuForm || m.menu.form.input.Value() != "8000 bytes" {
		t.Fatalf("Bash output form: page %v", m.menu.page)
	}
	m.menu.form.input.SetValue("off")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuSettings || m.ctl.Settings().BashOutput != 0 || m.menuRows()[5] != (menuRow{"Bash output", "off"}) {
		t.Fatalf("bash output not applied: %d %v", m.ctl.Settings().BashOutput, m.menuRows())
	}
	m.openSettingForm(5)
	m.menu.form.input.SetValue("500")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuForm || m.menu.form.err == "" || m.ctl.Settings().BashOutput != 0 {
		t.Fatal("bash output below 1000 accepted")
	}
	m.menu = &menuState{page: menuSettings, selected: 6}
	if m.menuRows()[6] != (menuRow{"Show bash", "on"}) {
		t.Fatalf("show bash row: %v", m.menuRows())
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.ctl.Settings().UI.ShowBash || m.menuRows()[6] != (menuRow{"Show bash", "off"}) {
		t.Fatalf("show bash not toggled: %v", m.menuRows())
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.menu = &menuState{page: menuSettings}
	if saved := core.LoadConfig(f.a.Home(), f.project); saved.BashTimeout != 90 || saved.CompactionThreshold != 4096 {
		t.Fatalf("settings not saved in zel.toml: %+v", saved)
	}
	for _, bad := range []string{"0", "soon"} {
		m.openSettingForm(4)
		m.menu.form.input.SetValue(bad)
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.menu.page != menuForm || m.menu.form.err == "" || m.ctl.Settings().BashTimeout != 90 {
			t.Fatalf("bash timeout %q accepted", bad)
		}
	}
	setConfig(t, m.ctl, func(s *core.Settings) { s.Providers["spare"] = core.Provider{BaseURL: "http://127.0.0.1:1/v1"} })
	m.menu = &menuState{page: menuProvider, provider: "spare", selected: 1}
	next, _ := m.menuActivate()
	m = next.(model)
	if m.menu.page != menuConfirm || len(m.ctl.Settings().Providers) != 2 {
		t.Fatal("provider deleted before confirmation")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if s := m.ctl.Settings(); len(s.Providers) != 1 || s.Provider != "p" || m.menu == nil || m.menu.page != menuProviders {
		t.Fatalf("provider not deleted: %+v", s)
	}
}

func TestTracingPrefixFiltersEveryType(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.menu = &menuState{page: menuTracing}
	for _, kind := range []string{"system", "user", "assistant", "thinking", "bash", "output", "error", "notice", "task", "event", "hook"} {
		m.menu.trace = append(m.menu.trace, core.TraceEntry{Kind: kind, Text: "needle text"})
	}
	for _, e := range m.menu.trace {
		m.menu.query = ":" + e.Kind
		rows := m.menuRows()
		if len(rows) != 1 || rows[0].label != traceKindLabel(e.Kind) {
			t.Fatalf("query %s: %v", m.menu.query, rows)
		}
	}
	m.menu.query = ":USER"
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "user" {
		t.Fatalf("case/whitespace: %v", rows)
	}
	m.menu.query = ":user absent"
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "No matches" {
		t.Fatalf("unmatched text: %v", rows)
	}
	m.menu.query = "needle"
	if rows := m.menuRows(); len(rows) != len(m.menu.trace) {
		t.Fatalf("ordinary search failed to match entry bodies: %v", rows)
	}
}

// Every [colors] key reaches its variable; bg fills whole rows, also after resets.
func TestTheme(t *testing.T) {
	defer applyTheme(config.DefaultColors())
	var c config.Colors
	for i := range c {
		c[i] = fmt.Sprintf("#0000%02x", i+1)
	}
	applyTheme(c)
	got := []lipgloss.Color{bgColor, accentColor, agentColor, userColor, userHighlight, grayColor, darkGray, errorColor,
		traceColors["system"], traceColors["user"], traceColors["agent"], traceColors["bash"], traceColors["notice"], traceColors["output"], traceColors["error"]}
	for i, key := range config.ColorKeys {
		if string(got[i]) != c[i] {
			t.Errorf("%s = %s, want %s", key, got[i], c[i])
		}
	}
	if traceColors["task"] != traceColors["notice"] || traceColors["assistant"] != traceColors["agent"] || !strings.Contains(markdownStyle, c[1]) {
		t.Errorf("trace kinds or markdown do not follow the theme")
	}

	bg := "\x1b[48;2;1;2;3m"
	row := withBackground("a\x1b[38;2;0;0;0mb\x1b[0mc\x1b[1;49md\x1b[38;5;0me", 8, bg)
	want := bg + "a\x1b[38;2;0;0;0mb\x1b[0m" + bg + "c\x1b[1;49m" + bg + "d\x1b[38;5;0me   \x1b[m"
	if row != want {
		t.Errorf("row = %q\nwant  %q", row, want)
	}
	if withBackground("", 3, bg) != bg+"   \x1b[m" || withBackground("x", 3, "") != "x" {
		t.Error("empty rows must be filled, and no bg leaves rows as they are")
	}
}

func TestAccentChoiceFromSettings(t *testing.T) {
	defer applyTheme(config.DefaultColors())
	m, a := freshModel(t)
	m.menu = &menuState{page: menuSettings, selected: 3}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu.page != menuAccent || len(m.menuRows()) < 15 || m.menuRows()[0].detail != "current" {
		t.Fatalf("accent page not opened: %v %v", m.menu.page, m.menuRows())
	}
	m.menu.selected = accentIndex("#80b3ad")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if accent := m.ctl.Settings().Colors.Get("accent"); accent != "#80b3ad" || string(accentColor) != "#80b3ad" || m.menu.page != menuSettings {
		t.Fatalf("accent not applied: %q %q %v", accent, accentColor, m.menu.page)
	}
	if rows := m.menuRows(); rows[3].detail != "Teal" {
		t.Fatalf("settings row = %v", rows[3])
	}
	if data, _ := os.ReadFile(a.ConfigFiles()[0]); !strings.Contains(string(data), "[colors]\naccent = \"#80b3ad\"") {
		t.Fatalf("the accent must be written as colors.accent:\n%s", data)
	}
	if accentName("#123456") != "Custom" || accentName("#D97757") != "Clay" {
		t.Fatal("a hex outside the palette is Custom")
	}
}

func TestStopAndQuitShortcuts(t *testing.T) {
	for _, page := range []menuPage{-1, menuMain, menuForm} {
		for _, k := range []tea.KeyType{tea.KeyCtrlC, tea.KeyCtrlD, tea.KeyCtrlQ} {
			m := bareModel(t, t.TempDir())
			if page == menuForm {
				m.openMenu()
				m.openProviderForm("", false)
			} else if page >= 0 {
				m.menu = &menuState{page: page}
			}
			_, cmd := m.Update(tea.KeyMsg{Type: k})
			if k == tea.KeyCtrlQ {
				if cmd == nil {
					t.Fatal("Ctrl+Q did not quit")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Fatal("Ctrl+Q did not return QuitMsg")
				}
			} else if cmd != nil {
				if _, ok := cmd().(tea.QuitMsg); ok {
					t.Fatalf("%v unexpectedly quit", k)
				}
			}
		}
	}
	for _, page := range []menuPage{-1, menuMain, menuForm} {
		m, _ := busyModel(t)
		if page == menuForm {
			m.openMenu()
			m.openProviderForm("", false)
		} else if page >= 0 {
			m.menu = &menuState{page: page}
		}
		m.input.SetValue("draft")
		m = key(m, tea.KeyMsg{Type: tea.KeyCtrlC})
		if !m.ctl.Status().Busy {
			t.Fatal("Ctrl+C stopped the turn instead of leaving copying to the terminal")
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyCtrlD})
		m = awaitIdle(t, m)
		if m.ctl.Status().Busy {
			t.Fatal("Ctrl+D did not stop the turn")
		}
		if m.input.Value() != "draft" {
			t.Fatal("copy/stop shortcuts changed the draft")
		}
	}
}

func TestMainMenuLabelsMatchAcceptance(t *testing.T) {
	m := bareModel(t, "/project")
	m.menu = &menuState{page: menuMain}
	want := []menuRow{
		{"Resume", "View all sessions"}, {"New", "Or just Ctrl+N"}, {"Models", "And Efforts, Ctrl+M"},
		{"Tracing", "Session Details, Ctrl+T"}, {"Rename", "Rename this session"},
		{"Info", "Tokens, Session, Ctrl+I"}, {"Help", "Keyboard shortcuts"},
		{"Quit", "Exit, Ctrl+Q"}, {"Providers", "Add, edit, remove"},
		{"Settings", "Bash timeout and output, compaction, accent"},
	}
	if got := m.buildMenuRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("main menu rows = %#v, want %#v", got, want)
	}
	m.width, m.height = 80, 30
	lines := m.pageLines("", m.menuWidth())
	if len(lines) != len(want) || strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "more items") {
		t.Fatalf("main menu fits without an overflow hint: %q", lines)
	}
}

func TestAutocompleteSelectionUsesArrow(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.fileCompletions = []string{"@first", "@second"}
	m.fileSelected = 1
	m.refresh(true)
	rows := strings.Split(ansi.Strip(strings.Join(m.timelineRows(0), "\n")), "\n")
	if len(rows) < 2 || strings.TrimSpace(rows[len(rows)-2]) != "@second <" || strings.TrimSpace(rows[len(rows)-3]) != "@first" {
		t.Fatalf("completion selection should place the menu-style arrow after the selected label: %q", rows)
	}
}

func TestListLimitHintAndTraceCache(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.width, m.height = 100, 50
	m.menu = &menuState{page: menuTracing}
	for i := 0; i < 25; i++ {
		m.menu.trace = append(m.menu.trace, core.TraceEntry{Kind: "user", Text: fmt.Sprintf("entry %d", i)})
	}
	lines := m.pageLines("", 56)
	if len(lines) != 11 || !strings.Contains(lines[10], "+ 15 more items") {
		t.Fatalf("list not capped: %v", lines)
	}
	cache := m.menu.listCache
	rows := m.menuRows()
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.menu.listCache != cache || &m.menuRows()[0] != &rows[0] {
		t.Fatal("cursor movement rebuilt trace rows")
	}
	m.menu.query = ":user >25 <25"
	if got := m.menuRows(); len(got) != 1 || !strings.Contains(got[0].detail, "24") {
		t.Fatalf("stale query cache: %v", got)
	}
	m.menu.query = ""
	m.menu.selected = 24
	first, visible := m.listWindow(25)
	if visible != 10 || first != 15 {
		t.Fatalf("last page: %d %d", first, visible)
	}
	if got := m.pageLines("", 56); len(got) != 10 {
		t.Fatalf("unexpected last-page hint: %v", got)
	}
	m.height = 9
	if got := m.pageLines("", 56); len(got) > m.menuBodyRoom() {
		t.Fatal("list overflows small terminal")
	}
}

func TestInputBlankLines(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.input.SetValue("draft")
	m.refresh(true)
	if m.viewport.topMargin() != 0 || m.inputTop != 0 {
		t.Fatalf("empty chat has extra composer spacing: margin=%d, input top=%d", m.viewport.topMargin(), m.inputTop)
	}
	m.entries = []entry{{Kind: "assistant", Text: "answer"}}
	m.refresh(true)
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	top := m.inputTop + m.viewport.topMargin() - m.viewport.YOffset
	if top < 1 || strings.TrimSpace(rows[top-1]) != "" || strings.TrimSpace(rows[top+m.input.Height()]) != "" {
		t.Fatalf("history has extra composer spacing: %q", rows)
	}
	m.openMenu()
	view := strings.Split(ansi.Strip(m.menuView()), "\n")
	input := m.menuBodyStart() - 2
	if strings.TrimSpace(view[input-1]) != "" || strings.TrimSpace(view[input+1]) != "" {
		t.Fatal("search needs blank lines")
	}
}

func TestTerminalShortcutBytes(t *testing.T) {
	for _, tc := range []struct {
		sequence string
		key      tea.KeyType
	}{
		{"\x19", tea.KeyCtrlY}, {"\x1b[121;5u", tea.KeyCtrlY},
		{"\x1b[1085;5u", tea.KeyCtrlY}, {"\x1b[1085::121;5u", tea.KeyCtrlY},
		{"\x04", tea.KeyCtrlD}, {"\x1b[100;5u", tea.KeyCtrlD},
		{"\x1b[68;5u", tea.KeyCtrlD}, {"\x1b[1074;5u", tea.KeyCtrlD},
		{"\x1b[1042;5u", tea.KeyCtrlD}, {"\x1b[1074::100;5u", tea.KeyCtrlD},
		{"\x03", tea.KeyCtrlC}, {"\x1b[99;5u", tea.KeyCtrlC},
		{"\x0e", tea.KeyCtrlN}, {"\x1b[110;5u", tea.KeyCtrlN},
		{"\x1b[1090;5u", tea.KeyCtrlN}, {"\x1b[1090::110;5u", tea.KeyCtrlN},
		{"\x1b[78;5u", tea.KeyCtrlN}, {"\x1b[1058;5u", tea.KeyCtrlN},
		{"\x1b[102;5u", tea.KeyCtrlF}, {"\x1b[1072;5u", tea.KeyCtrlF},
		{"\x1b[113;5u", tea.KeyCtrlQ}, {"\x1b[1081;5u", tea.KeyCtrlQ},
	} {
		seen := false
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		program := tea.NewProgram(bareModel(t, t.TempDir()), tea.WithContext(ctx), tea.WithInput(strings.NewReader(tc.sequence)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if key, ok := normalizeTerminalKey(msg).(tea.KeyMsg); ok && key.Type == tc.key {
				seen = true
				return tea.QuitMsg{}
			}
			return msg
		}))
		_, err := program.Run()
		cancel()
		if err != nil || !seen {
			t.Fatalf("%q: seen=%v err=%v", tc.sequence, seen, err)
		}
	}
}

func TestTracingOpensLatestEntry(t *testing.T) {
	for _, shortcut := range []bool{false, true} {
		t.Run(fmt.Sprint(shortcut), func(t *testing.T) {
			m := bareModel(t, t.TempDir())
			m.width, m.height = 90, 24
			for i := range 30 {
				m.entries = append(m.entries, entry{Kind: "output", Text: fmt.Sprintf("live %d", i), Preview: true})
			}
			if shortcut {
				m = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
			} else {
				m.menu = &menuState{page: menuMain, selected: 3}
				next, _ := m.menuActivate()
				m = next.(model)
			}
			count := len(m.menu.trace)
			if count < 30 || m.menu.selected != count-1 {
				t.Fatalf("latest entry not selected: %d of %d", m.menu.selected, count)
			}
			first, visible := m.listWindow(len(m.menuRows()))
			if first+visible != count {
				t.Fatalf("latest entry not visible: %d + %d != %d", first, visible, count)
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
			if !strings.Contains(m.menu.text, "live 29") {
				t.Fatalf("opened wrong entry: %q", m.menu.text)
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
			if m.menu == nil || m.menu.page != menuTracing || m.menu.selected != count-1 {
				t.Fatal("Esc did not restore the latest entry in the list")
			}
		})
	}
	m := bareModel(t, t.TempDir())
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.menu.selected != max(0, len(m.menu.trace)-1) {
		t.Fatal("invalid selection for an empty or initial trace")
	}
}

func TestTracingEntryNavigationPreservesList(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) {
			m := bareModel(t, t.TempDir())
			m.width, m.height = 90, 24
			m.menu = &menuState{page: menuTracing, direct: direct, query: "match", selected: 1,
				trace: []core.TraceEntry{
					{Kind: "match", Text: "first"},
					{Kind: "excluded", Text: "not included"},
					{Kind: "match", Text: "middle\n" + strings.Repeat("line\n", 100)},
					{Kind: "match", Text: "last"},
				},
			}
			m.menuRows() // Exercise the filtered-list cache as well.
			first, visible := m.listWindow(3)
			next, _ := m.menuActivate()
			m = next.(model)
			if m.menu.query != "match" || m.menu.traceIndex != 2 {
				t.Fatal("opening an entry lost the filter or opened the wrong entry")
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyDown})
			if m.menu.offset == 0 || m.menu.traceIndex != 2 {
				t.Fatal("Down should scroll the current entry")
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
			if m.menu == nil || m.menu.page != menuTracing || m.menu.query != "match" || m.menu.selected != 1 {
				t.Fatal("Esc did not preserve list state")
			}
			if f, v := m.listWindow(3); f != first || v != visible {
				t.Fatal("Esc changed the list position")
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
			for _, step := range []struct {
				key   tea.KeyType
				index int
			}{
				{tea.KeyRight, 3}, {tea.KeyRight, 3}, {tea.KeyLeft, 2}, {tea.KeyLeft, 0}, {tea.KeyLeft, 0},
			} {
				m = key(m, tea.KeyMsg{Type: step.key})
				if m.menu.traceIndex != step.index || m.menu.query != "match" || m.menu.offset != 0 {
					t.Fatalf("navigation lost state: %+v", m.menu)
				}
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
			if m.menu.page != menuTracing || m.menu.selected != 0 || m.menu.query != "match" {
				t.Fatal("Esc did not select the currently viewed entry")
			}
			if direct {
				m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
				if m.menu != nil {
					t.Fatal("Esc from a shortcut-opened list should return to chat")
				}
			}
		})
	}
}

func TestNewSessionShortcut(t *testing.T) {
	for _, menu := range []bool{false, true} {
		t.Run(fmt.Sprint(menu), func(t *testing.T) {
			m := bareModel(t, t.TempDir())
			before, err := m.ctl.Session()
			if err != nil {
				t.Fatal(err)
			}
			if menu {
				m.openMenu()
			}
			m = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
			after, err := m.ctl.Session()
			if err != nil {
				t.Fatal(err)
			}
			if before.ID == after.ID || m.menu != nil {
				t.Fatal("Ctrl+N did not create a new session and return to chat")
			}
		})
	}
}

func TestNewSessionShortcutBlockedDuringTurn(t *testing.T) {
	m, release := busyModel(t)
	defer release()
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if !m.ctl.Status().Busy {
		t.Fatal("Ctrl+N interrupted the active turn")
	}
	found := false
	for _, e := range m.ctl.Trace() {
		if e.Kind == "error" && strings.Contains(e.Text, "Wait for the active turn") {
			found = true
		}
	}
	if !found {
		t.Fatal("Ctrl+N did not report that the session is busy")
	}
}

func TestTracingNavigationAcrossRefresh(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.entries = []entry{
		{Kind: "match", Text: "first", Preview: true},
		{Kind: "excluded", Text: "excluded", Preview: true},
		{Kind: "match", Text: "middle", Preview: true},
		{Kind: "match", Text: "last", Preview: true},
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m.menu.query, m.menu.selected = "match", 2
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, step := range []struct {
		key      tea.KeyType
		text     string
		selected int
	}{
		{tea.KeyLeft, "middle", 1},
		{tea.KeyLeft, "first", 0},
		{tea.KeyRight, "middle", 1},
		{tea.KeyRight, "last", 2},
	} {
		m.refreshMenu()
		m = key(m, tea.KeyMsg{Type: step.key})
		if !strings.Contains(m.menu.text, step.text) || m.menu.selected != step.selected {
			t.Fatalf("navigation after refresh: selected=%d text=%q; want %d %q", m.menu.selected, m.menu.text, step.selected, step.text)
		}
	}
	m.refreshMenu()
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu == nil || m.menu.page != menuTracing || m.menu.selected != 2 || m.menu.query != "match" {
		t.Fatal("refresh lost the tracing list state")
	}
}

func TestTracingEntryNumber(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.width, m.height = 90, 24
	m.entries = []entry{
		{Kind: "match", Text: "first", Preview: true},
		{Kind: "excluded", Text: "excluded", Preview: true},
		{Kind: "match", Text: "last", Preview: true},
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m.menu.query, m.menu.selected = "match", 1
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	check := func() {
		t.Helper()
		_, counter := m.menuHead()
		want := fmt.Sprintf("%d/%d", m.menu.traceIndex+1, len(m.menu.trace))
		if counter != want || !strings.Contains(ansi.Strip(m.menuView()), want) {
			t.Fatalf("entry number: got %q, want %q", counter, want)
		}
	}
	check()
	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	check()
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	check()
	m.entries = append(m.entries, entry{Kind: "output", Text: "new entry", Preview: true})
	m.refreshMenu()
	check()
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	_, shortcut := m.menuHead()
	if shortcut != "Ctrl+T" {
		t.Fatalf("list header changed: %q", shortcut)
	}
}

func TestTracingCombinedTypeAndInclusiveIndexFilters(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.menu = &menuState{page: menuTracing}
	for i := 0; i < 130; i++ {
		kind := "system"
		if i == 122 || i == 124 {
			kind = "bash"
		} else if i == 123 {
			kind = "user"
		}
		m.menu.trace = append(m.menu.trace, core.TraceEntry{Kind: kind, Text: "entry"})
	}
	rows := m.rawMenuRows()
	m.menu.query = ":bash+user"
	if got := m.buildMenuIndices(rows); !reflect.DeepEqual(got, []int{122, 123, 124}) {
		t.Fatalf("combined types: %v", got)
	}
	m.menu.query = ">123 :bash"
	if got := m.buildMenuIndices(rows); !reflect.DeepEqual(got, []int{122, 124}) {
		t.Fatalf("inclusive index lower bound: %v", got)
	}
	m.menu.query = "<123 :bash+user"
	if got := m.buildMenuIndices(rows); !reflect.DeepEqual(got, []int{122}) {
		t.Fatalf("inclusive index upper bound: %v", got)
	}
}

func TestSearchMatches(t *testing.T) {
	cases := []struct {
		text  string
		query string
		want  bool
	}{
		{"bash zel -p command", "zel -p", true},
		{"bash zel -p command", "zel", true},
		{"bash echo hello", "zel -p", false},
		{"bash echo hello", "", true},
		{"BASH ZEL -P", "zel -p", true},
		{"bash zel -p", "'zel -p'", true},
	}
	for _, tc := range cases {
		if got := searchMatches(tc.text, tc.query); got != tc.want {
			t.Errorf("searchMatches(%q, %q) = %v, want %v", tc.text, tc.query, got, tc.want)
		}
	}
}

func TestTracingFullContextSearch(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.menu = &menuState{page: menuTracing}
	for i := 0; i < 150; i++ {
		m.menu.trace = append(m.menu.trace, core.TraceEntry{
			Kind: "system",
			Text: fmt.Sprintf("system entry %d", i+1),
		})
	}
	m.menu.trace[49] = core.TraceEntry{Kind: "bash", Text: "zel -p subagent"}
	m.menu.trace[104] = core.TraceEntry{Kind: "bash", Text: "git status"}
	m.menu.trace[109] = core.TraceEntry{Kind: "user", Text: "please run zel -p"}
	m.menu.trace[114] = core.TraceEntry{Kind: "bash", Text: "zel -p --timeout 60"}
	m.menu.trace[119] = core.TraceEntry{Kind: "bash", Text: "echo running\nzel -p /tmp/work"}
	m.menu.trace[124] = core.TraceEntry{Kind: "output", Text: "output from zel -p"}

	rows := m.rawMenuRows()
	m.menu.query = ":bash >100 zel -p"
	got := m.buildMenuIndices(rows)
	want := []int{114, 119}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query :bash >100 zel -p = %v, want %v", got, want)
	}

	matchedRows := m.menuRows()
	if len(matchedRows) != 2 || matchedRows[0].label != "bash" || matchedRows[1].label != "bash" {
		t.Fatalf("menuRows: %v", matchedRows)
	}
}

func TestTracingEntryMarkdownHeaderColorsAndScrolling(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(profile)
	m := bareModel(t, t.TempDir())
	m.width, m.height = 80, 30
	start := time.Unix(100, 0)
	m.menu = &menuState{page: menuTracing, trace: []core.TraceEntry{
		{Kind: "system", Text: "system message", At: start},
		{Kind: "assistant", Text: "**markdown bold** content", At: start.Add(2 * time.Second)},
	}}
	m.openTrace(1)
	view := m.menuView()
	plain := ansi.Strip(view)
	if !strings.Contains(view, "\x1b[38;2;255;255;255m") {
		t.Fatal("agent Markdown body does not use the trace white color")
	}
	for _, kind := range []string{"system", "user"} {
		m.menu.trace[1] = core.TraceEntry{Kind: kind, Text: "**trace markdown**"}
		m.openTrace(1)
		body := ansi.Strip(strings.Join(m.textBody(m.menuWidth()), "\n"))
		if strings.Contains(body, "**trace markdown**") || !strings.Contains(body, "trace markdown") {
			t.Errorf("%s Markdown was not rendered: %q", kind, body)
		}
	}
	m.menu.trace[1] = core.TraceEntry{Kind: "assistant", Text: "**markdown bold** content", At: start.Add(2 * time.Second)}
	m.openTrace(1)
	view = m.menuView()
	plain = ansi.Strip(view)
	if !strings.Contains(plain, "2s") || !strings.Contains(plain, "agent") || !strings.Contains(plain, "2/2") {
		t.Fatalf("trace header missing elapsed/type/position: %q", plain)
	}
	header := strings.Split(plain, "\n")[m.menuTop()]
	if got, want := strings.Index(header, "agent"), m.menuLeft()+(m.menuWidth()-len("agent"))/2; got != want {
		t.Fatalf("entry type column %d, want centered column %d: %q", got, want, header)
	}
	if strings.Contains(plain, "**markdown bold**") || !strings.Contains(plain, "markdown bold") {
		t.Fatalf("trace Markdown was not rendered: %q", plain)
	}
	if chat, ok := renderMarkdown("plain chat text", 40, false); !ok || strings.Contains(chat, "\x1b[38;2;255;255;255m") {
		t.Fatal("trace color leaked into normal chat Markdown")
	}
	lines := strings.Split(plain, "\n")
	for i := m.menuTop(); i < min(len(lines), m.menuTop()+3); i++ {
		if strings.TrimSpace(lines[i]) == "assistant" || strings.TrimSpace(lines[i]) == "agent" {
			t.Fatalf("duplicate type-only line remains: %q", lines[i])
		}
	}
	for kind, want := range map[string]string{"system": string(darkGray), "user": string(accentColor), "assistant": "#ffffff", "bash": "#a0a0a0", "notice": "#808080", "task": "#808080", "output": "#404040"} {
		if got := string(traceColor(kind)); got != want {
			t.Errorf("%s color = %s, want %s", kind, got, want)
		}
	}
	brightness := func(color string) int {
		var r, g, b int
		if _, err := fmt.Sscanf(color, "#%02x%02x%02x", &r, &g, &b); err != nil {
			t.Fatalf("invalid trace color %q: %v", color, err)
		}
		return r + g + b
	}
	if brightness(string(traceColor("output"))) >= brightness(string(traceColor("system"))) {
		t.Fatal("output text must be darker than system text")
	}

	body := make([]string, 30)
	for i := range body {
		body[i] = fmt.Sprintf("line %02d", i)
	}
	m.menu.trace[1] = core.TraceEntry{Kind: "bash", Text: strings.Join(body, "\n"), At: start.Add(2 * time.Second)}
	m.openTrace(1)
	if got := len(m.pageLines("", m.menuWidth())); got > 10 {
		t.Fatalf("trace viewport has %d rows, want at most 10", got)
	}
	_, _ = m.menuMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.menu.offset == 0 {
		t.Fatal("mouse wheel did not scroll the trace body")
	}
	before := m.menu.offset
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.menu.offset >= before {
		t.Fatal("arrow key did not scroll the trace body")
	}
}

func TestEscapeNeverCancelsActiveWork(t *testing.T) {
	f := newFixture(t, true)
	gate := make(chan struct{})
	f.hold.Store(&gate)
	f.m.width, f.m.height = 100, 34
	f.m.refresh(true)
	f.m.input.SetValue("work")
	f.m = key(f.m, tea.KeyMsg{Type: tea.KeyEnter})
	awaitRequest(t, f.m)
	for _, overlay := range []string{"none", "menu", "completion"} {
		switch overlay {
		case "menu":
			f.m.menu = &menuState{page: menuMain}
		case "completion":
			f.m.fileCompletions = []string{"file.txt"}
			f.m.fileLoading = true
		}
		f.m = key(f.m, tea.KeyMsg{Type: tea.KeyEsc})
		if status := f.m.ctl.Status(); !status.Busy || status.Cancelling {
			t.Fatalf("Esc cancelled work with %s: %+v", overlay, status)
		}
		if overlay == "menu" && f.m.menu != nil {
			t.Fatal("Esc did not dismiss menu")
		}
		if overlay == "completion" && (f.m.fileLoading || f.m.hasFileCompletions()) {
			t.Fatal("Esc did not dismiss completions")
		}
	}
	f.m = key(f.m, tea.KeyMsg{Type: tea.KeyCtrlD})
	f.m = awaitIdle(t, f.m)
	close(gate)
}

func TestComposerSpacingDependsOnTimelineContent(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.width, m.height = 100, 34
	assertComposer := func(wantBlankBefore bool, state string) {
		t.Helper()
		m.cache = nil
		m.refresh(true)
		rows := strings.Split(ansi.Strip(m.View()), "\n")
		top := m.inputTop + m.viewport.topMargin() - m.viewport.YOffset
		if top < 0 || top >= len(rows) {
			t.Fatalf("%s: composer row out of bounds: %d/%d", state, top, len(rows))
		}
		blank := top > 0 && strings.TrimSpace(rows[top-1]) == ""
		if blank != wantBlankBefore {
			t.Fatalf("%s: blank row before composer = %v, want %v (top=%d rows=%q)", state, blank, wantBlankBefore, top, rows)
		}
	}
	assertComposer(false, "empty session")
	m.entries = []entry{{Kind: "user", Text: "first message"}}
	assertComposer(true, "first message")
	m.entries = nil
	m.input.SetValue("draft")
	assertComposer(false, "draft in empty session")
}

func TestEventQueueBoundsMemoryAndPreservesFinalOrder(t *testing.T) {
	q := newEventQueue()
	defer q.Close()
	for i := 0; i < 2000; i++ {
		q.Push(core.Event{Kind: "done", Text: fmt.Sprint(i)})
	}
	if len(q.items) > 256 || q.spill == nil {
		t.Fatalf("queue not bounded: %d", len(q.items))
	}
	for i := 0; i < 2000; i++ {
		e, ok := q.TryPop()
		if !ok || e.Text != fmt.Sprint(i) {
			t.Fatalf("event %d: %+v %v", i, e, ok)
		}
	}
	if _, ok := q.TryPop(); ok || q.spill != nil {
		t.Fatal("queue/spool not drained")
	}
	q.Push(agentEvent(core.TurnEvent{Kind: "delta", ID: "x", Text: "a"}))
	q.Push(agentEvent(core.TurnEvent{Kind: "delta", ID: "x", Text: "b"}))
	q.Push(agentEvent(core.TurnEvent{Kind: "assistant", ID: "x", Text: "ab", Final: true}))
	e, _ := q.TryPop()
	final, _ := q.TryPop()
	if e.Agent.Text != "ab" || !final.Agent.Final {
		t.Fatalf("coalescing lost final: %+v %+v", e, final)
	}
}

// taskModel is a chat whose session starts with one background task running:
// a session_created hook in zel.toml leaves the session socket, and the test
// starts the task through it the way zel task run does.
func taskModel(t *testing.T) model {
	t.Helper()
	t.Setenv("ZEL_HOME", t.TempDir())
	project := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "socket")
	toml := "[hooks]\nsession_created = 'printf \"%s\\n%s\" \"$ZEL_SOCKET\" \"$ZEL_TOKEN\" > " + envFile + "'\n"
	if err := os.WriteFile(filepath.Join(project, "zel.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := core.New(project, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	m := newModel(a, project)
	t.Cleanup(m.ctl.Close)
	var socket, token string
	for deadline := time.Now().Add(5 * time.Second); socket == ""; time.Sleep(5 * time.Millisecond) {
		if data, _ := os.ReadFile(envFile); strings.Count(string(data), "\n") == 1 {
			socket, token, _ = strings.Cut(string(data), "\n")
		}
		if time.Now().After(deadline) {
			t.Fatal("the session_created hook never ran")
		}
	}
	r, err := ipc.Call(context.Background(), socket, ipc.Request{Token: token, Op: "task.run", Args: []byte(`{"name":"hold","cmd":"sleep 30"}`)})
	if err != nil || !r.OK {
		t.Fatalf("task.run = %+v, %v", r, err)
	}
	for deadline := time.Now().Add(5 * time.Second); m.ctl.Status().Tasks != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session_created task never started")
		}
	}
	return m
}

func TestSessionSwitchConfirmsStoppingTasks(t *testing.T) {
	m := taskModel(t)
	id := m.ctl.Status().Session
	if got := m.ctl.ActiveTasks(); len(got) != 1 || got[0].Name != "hold" {
		t.Fatalf("active tasks: %+v", got)
	}
	next, _ := m.switchSession("")
	m = next.(model)
	if m.menu == nil || m.menu.page != menuConfirm || m.menu.selected != 0 {
		t.Fatal("missing safe confirmation")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.ctl.Status().Session != id || m.ctl.Status().Tasks != 1 {
		t.Fatal("abort stopped task or switched session")
	}
	next, _ = m.switchSession("")
	m = next.(model)
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	// The new session's own session_created starts a fresh task; the old one is stopped.
	if m.ctl.Status().Session == id {
		t.Fatal("confirmation did not switch and stop tasks")
	}
}

func TestEventSpoolFailureIsFatalAndExplicit(t *testing.T) {
	q := newEventQueue()
	defer q.Close()
	for i := 0; i < 257; i++ {
		q.Push(core.Event{Kind: "done"})
	}
	if err := q.spill.Close(); err != nil {
		t.Fatal(err)
	}
	q.Push(core.Event{Kind: "done"})
	var failure core.Event
	for {
		e, ok := q.TryPop()
		if !ok {
			break
		}
		if e.Kind == "fatal" {
			failure = e
		}
	}
	if !strings.Contains(failure.Text, "UI event spool") {
		t.Fatalf("spool failure hidden: %+v", failure)
	}
	m := bareModel(t, t.TempDir())
	next, cmd := m.Update(failure)
	if next.(model).failure == nil || cmd == nil {
		t.Fatal("fatal queue failure did not terminate the UI explicitly")
	}
}

func TestLiveTraceDoesNotPrecedeOlderNotices(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.ctl.LogEvent("notice", "older notice")
	m.entries = []entry{{Kind: "reasoning", Text: "live thinking", Preview: true, Meta: &core.Metadata{At: time.Now().Add(time.Second)}}}
	trace := m.sessionTrace()
	if len(trace) < 2 || trace[len(trace)-1].Text != "live thinking" || trace[len(trace)-2].Text != "older notice" {
		t.Fatalf("live trace order: %+v", trace)
	}
}

func TestChatTopMarginSurvivesMessagesAndScrolling(t *testing.T) {
	f := newFixture(t, true)
	gate := make(chan struct{})
	f.hold.Store(&gate)
	f.m.width, f.m.height = 60, 12
	check := func(state string) {
		t.Helper()
		rows := strings.Split(ansi.Strip(f.m.View()), "\n")
		if len(rows) != f.m.height || f.m.viewport.topMargin() != 0 {
			t.Fatalf("%s: unexpected viewport top margin: margin=%d rows=%q", state, f.m.viewport.topMargin(), rows)
		}
	}
	f.m.refresh(true)
	check("new session")
	if f.m.inputTop != 0 || f.m.viewport.topMargin() != 0 {
		t.Fatal("new session has duplicate top spacing")
	}
	f.m.input.SetValue("hello")
	f.m = key(f.m, tea.KeyMsg{Type: tea.KeyEnter})
	awaitRequest(t, f.m)
	f.m.refresh(true)
	check("after sending")
	close(gate)
	f.settle(t)
	check("after answer")
	id := f.m.ctl.Status().Session
	f.m = key(f.m, tea.KeyMsg{Type: tea.KeyCtrlN})
	check("new session after conversation")
	if err := f.m.ctl.Resume(id); err != nil {
		t.Fatal(err)
	}
	f.m = settle(f.m)
	check("resumed session")
	for i := 0; i < 30; i++ {
		f.m.entries = append(f.m.entries, entry{Kind: "assistant", Text: fmt.Sprintf("row %d", i)})
	}
	f.m.refresh(true)
	check("scrolled to bottom")
	if f.m.viewport.YOffset == 0 {
		t.Fatal("test did not overflow viewport")
	}
	f.m.viewport.GotoTop()
	check("scrolled to top")
	f.m.viewport.ScrollDown(3)
	check("scrolled midway")
	f.m.height = 4
	f.m.refresh(true)
	check("resized")
	f.m.input.SetValue("draft")
	f.m.refresh(true)
	y := f.m.inputTop + f.m.viewport.topMargin() - f.m.viewport.YOffset
	if offset, ok := f.m.composerOffset(0, y); !ok || offset != 0 {
		t.Fatalf("margin shifted composer mouse mapping: %d %v", offset, ok)
	}
	f.m.viewport.YOffset = f.m.inputTop + 1
	if _, ok := f.m.composerOffset(0, 0); ok {
		t.Fatal("blank screen margin accepts composer clicks")
	}
	f.m.height = 1
	f.m.refresh(true)
	if f.m.viewport.topMargin() != 0 {
		t.Fatal("margin hides content in one-row terminal")
	}
}

func TestScrollUpMovesThroughComposerBeforeChat(t *testing.T) {
	for _, text := range []string{"", "draft", "first\nsecond\nthird\nfourth\nfifth", strings.Repeat("word ", 90)} {
		for _, wheel := range []bool{false, true} {
			t.Run(fmt.Sprintf("length=%d/wheel=%t", len(text), wheel), func(t *testing.T) {
				m := bareModel(t, "/project")
				m.add("assistant", strings.Repeat("history line\n\n", 100))
				m.setComposer(text)
				m.input.SetCursor(len([]rune(strings.Split(text, "\n")[m.input.Line()])))
				m.refresh(true)
				var msg tea.Msg = tea.KeyMsg{Type: tea.KeyUp}
				steps := 1
				if wheel {
					msg = tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}
					steps = 3
				}
				for range 100 {
					before := m.viewport.YOffset
					if before == 0 {
						t.Fatal("history unexpectedly reached the top")
					}
					probe := m.input
					remaining := 0
					for probe.Line() > 0 || probe.LineInfo().RowOffset > 0 {
						probe.CursorUp()
						remaining++
					}
					next, _ := m.update(msg)
					m = next.(model)
					want := max(0, before-max(0, steps-remaining))
					if m.viewport.YOffset != want || m.input.Value() != text {
						t.Fatalf("offset=%d, want %d; composer=%q", m.viewport.YOffset, want, m.input.Value())
					}
					if remaining == 0 {
						return
					}
				}
				t.Fatal("cursor never reached the top of the composer")
			})
		}
	}
}

func TestDownScrollsBackToComposer(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.width, m.height = 48, 12
	m.entries = []entry{{Kind: "assistant", Text: strings.Repeat("answer row\n", 40)}}
	m.input.SetValue("first\nsecond\nthird")
	m.setCursorOffset(0)
	m.refresh(true)
	m.viewport.GotoTop()
	before := m.viewport.YOffset
	cursor := m.composerCursorOffset()
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.viewport.YOffset != before+1 || m.composerCursorOffset() != cursor {
		t.Fatal("Down must scroll the chat before moving the composer cursor")
	}
	for range m.viewport.maxYOffset() + 1 {
		if m.viewport.AtBottom() {
			break
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if !m.viewport.AtBottom() || m.input.Value() != "first\nsecond\nthird" {
		t.Fatal("Down failed to reach the composer or changed the draft")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.composerCursorOffset() <= cursor {
		t.Fatal("Down must move the cursor once the composer is visible")
	}
}

func TestSlashMenu(t *testing.T) {
	for _, tc := range []struct {
		name, input, text        string
		enabled, paste, wantMenu bool
	}{
		{"empty composer", "", "/", true, false, true},
		{"disabled", "", "/", false, false, false},
		{"existing text", "hello", "/", true, false, false},
		{"pasted slash", "", "/", true, true, false},
		{"pasted path", "", "/tmp", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bareModel(t, "/project")
			setConfig(t, m.ctl, func(s *core.Settings) { s.UI.SlashMenu = tc.enabled })
			m.input.SetValue(tc.input)
			m.input.CursorEnd()
			m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.text), Paste: tc.paste})
			if (m.menu != nil) != tc.wantMenu {
				t.Fatalf("menu = %v", m.menu)
			}
			want := tc.input + tc.text
			if tc.wantMenu {
				want = tc.input
			}
			if m.input.Value() != want {
				t.Fatalf("input = %q, want %q", m.input.Value(), want)
			}
			if tc.wantMenu {
				m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
				if m.input.Value() != "" {
					t.Fatal("slash remained after closing menu")
				}
			}
		})
	}
	m := bareModel(t, "/project")
	m.menu = &menuState{page: menuSettings, selected: 2}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.ctl.Settings().UI.SlashMenu {
		t.Fatal("setting not disabled")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.ctl.Settings().UI.SlashMenu {
		t.Fatal("setting not enabled")
	}
}

func TestWorkingIndicatorDuringResponse(t *testing.T) {
	m, release := busyModel(t)
	defer release()
	m.entries = []entry{{Kind: "assistant", Text: "Previous answer"}}
	m.turnIndex = len(m.entries)
	if m.workingBlock() == "" {
		t.Fatal("previous turn hid the working indicator")
	}
	for _, tc := range []struct {
		event   core.TurnEvent
		visible bool
	}{
		{core.TurnEvent{Kind: "delta", ID: "answer", Text: " ", Provisional: true}, true},
		{core.TurnEvent{Kind: "delta", ID: "answer", Text: "Hello", Provisional: true}, false},
		{core.TurnEvent{Kind: "assistant", ID: "answer", Text: "Hello", Final: true}, false},
		{core.TurnEvent{Kind: "code", ID: "tool", Text: "ls", Provisional: true}, true},
		{core.TurnEvent{Kind: "reasoning", ID: "thought", Text: "Thinking", Provisional: true}, true},
		{core.TurnEvent{Kind: "delta", ID: "next", Text: "Done", Provisional: true}, false},
	} {
		m.entries = core.ApplyEvent(m.entries, tc.event)
		if got := m.workingBlock() != ""; got != tc.visible {
			t.Fatalf("after %s %s: indicator visible = %v, want %v", tc.event.Kind, tc.event.ID, got, tc.visible)
		}
	}
}

func TestTurnStepsReconcilesAnswerTotals(t *testing.T) {
	m := bareModel(t, "/project")
	m.entries = []entry{
		{Kind: "assistant", Meta: &core.Metadata{Steps: 20}},
		{Kind: "user", Text: "new turn"},
		{Kind: "code", Text: "first tool"},
		{Kind: "reasoning", Text: "thinking"},
		{Kind: "assistant", Text: "Working", Meta: &core.Metadata{Steps: 4}},
		{Kind: "code", Text: "next tool"},
	}
	m.turnIndex = 1
	if got := m.turnSteps(); got != 5 {
		t.Fatalf("steps after answer: got %d, want 5", got)
	}
	m.entries = append(m.entries, entry{Kind: "assistant", Preview: true, Meta: &core.Metadata{Steps: 99}})
	if got := m.turnSteps(); got != 5 {
		t.Fatalf("preview changed steps: %d", got)
	}
	m.entries = append(m.entries, entry{Kind: "assistant", Meta: &core.Metadata{Steps: 3}})
	if got := m.turnSteps(); got != 5 {
		t.Fatalf("older total reduced steps: %d", got)
	}
}

func TestInfoRowsDisplayHomePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "projects", "zel")
	config := filepath.Join(home, ".config", "zel", "config.toml")
	info := core.Info{Project: project, Configs: []string{config}}
	rows := infoRows(info)
	if info.Project != project || info.Configs[0] != config {
		t.Fatal("display path conversion modified the filesystem paths")
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.label+" "+row.detail)
	}
	text := strings.Join(got, "\n")
	for _, want := range []string{"~/projects/zel"} {
		if !strings.Contains(text, want) {
			t.Errorf("Info rows do not contain %q:\n%s", want, text)
		}
	}
	for _, absolute := range []string{project} {
		if strings.Contains(text, absolute) {
			t.Errorf("Info rows leaked home path %q", absolute)
		}
	}
}

func TestFrameDropsTrailingPadding(t *testing.T) {
	cases := map[string]string{
		"\x1b[38;2;214;214;214mХей     \x1b[0m   ": "\x1b[38;2;214;214;214mХей\x1b[0m",
		"\x1b[38;2;48;7;0m   \x1b[0m":              "\x1b[38;2;48;7;0m\x1b[0m",
		"a b  ":                                    "a b",
		"x\x1b[7m  \x1b[0m ":                       "x\x1b[7m  \x1b[0m",
		"   ":                                      "",
	}
	for in, want := range cases {
		if got := trimTrailingSpaces(in); got != want {
			t.Errorf("trim(%q) = %q, want %q", in, got, want)
		}
	}
	if got := frame([]string{"ab" + composerCursorMarker + "  "}); got != "ab\x1b]1337;ZelCaret;1;3\x07" {
		t.Fatalf("frame = %q", got)
	}
}

// Performance baselines (go test -run xxx -bench . ./internal/tui). They cover
// the paths that run on the UI thread: timeline refresh, text pages of large
// documents, long lists, and the periodic menu snapshot.

func perfText(size int) string {
	line := "func example(value int) string { return fmt.Sprintf(\"%d items of the list\", value) } // comment\n"
	return strings.Repeat(line, size/len(line)+1)[:size]
}

func perfModel(b *testing.B, entries int) model {
	m := bareModel(b, b.TempDir())
	m.width, m.height = 100, 40
	for i := 0; i < entries; i++ {
		kind := "user"
		text := fmt.Sprintf("question %d about the project", i)
		if i%2 == 1 {
			kind, text = "assistant", fmt.Sprintf("## Answer %d\n\nSome **markdown** with `code` and a list:\n\n- first\n- second\n", i)
		}
		m.entries = append(m.entries, entry{Kind: kind, Text: text})
	}
	m.refresh(true)
	return m
}

func BenchmarkTimelineRefresh(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n, "entries"), func(b *testing.B) {
			m := perfModel(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.refresh(false)
			}
		})
	}
}

func BenchmarkTextPageOpen(b *testing.B) {
	for _, size := range []int{100 << 10, 1 << 20, 10 << 20} {
		b.Run(fmt.Sprint(size>>10, "KB"), func(b *testing.B) {
			m := bareModel(b, b.TempDir())
			m.width, m.height = 100, 40
			text := perfText(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.menu = &menuState{page: menuText, parent: menuTracing, text: text}
				_ = m.menuView()
			}
		})
	}
}

func BenchmarkTextPageScroll(b *testing.B) {
	for _, size := range []int{100 << 10, 1 << 20} {
		b.Run(fmt.Sprint(size>>10, "KB"), func(b *testing.B) {
			m := bareModel(b, b.TempDir())
			m.width, m.height = 100, 40
			m.menu = &menuState{page: menuText, parent: menuTracing, text: perfText(size)}
			_ = m.menuView()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.scrollText(1)
				_ = m.menuView()
			}
		})
	}
}

func BenchmarkTraceMarkdownEntry(b *testing.B) {
	m := bareModel(b, b.TempDir())
	m.width, m.height = 100, 40
	text := strings.Repeat("## Heading\n\nA paragraph with **bold**, `code` and a [link](http://example.com).\n\n- item one\n- item two\n\n", 1000)
	m.menu = &menuState{page: menuText, parent: menuTracing, traceIndex: 0, trace: []core.TraceEntry{{Kind: "assistant", Text: text}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.menuView()
		m.scrollText(1)
	}
}

func BenchmarkTraceList(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n, "items"), func(b *testing.B) {
			m := bareModel(b, b.TempDir())
			m.width, m.height = 100, 40
			m.menu = &menuState{page: menuTracing}
			for i := 0; i < n; i++ {
				m.menu.trace = append(m.menu.trace, core.TraceEntry{Kind: "output", Text: fmt.Sprintf("entry %d with some text", i)})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.menu.selected = i % n
				_ = m.menuView()
			}
		})
	}
}

func BenchmarkMenuSnapshot(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprint(n, "events"), func(b *testing.B) {
			m := bareModel(b, b.TempDir())
			for i := 0; i < n; i++ {
				m.ctl.LogEvent("notice", fmt.Sprintf("event %d", i))
			}
			m.menu = &menuState{page: menuTracing}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.refreshMenu()
			}
		})
	}
}

// commandMessages runs cmd, expanding batches, and names the message types it
// produced. Timer commands take their delay, so use it sparingly.
func commandMessages(cmd tea.Cmd) []string {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []string{fmt.Sprintf("%T", msg)}
	}
	var out []string
	for _, c := range batch {
		out = append(out, commandMessages(c)...)
	}
	return out
}

// referenceTimelineRows is the original single-slice timeline builder, kept to
// prove the segmented one produces the same rows.
func (m *model) referenceTimelineRows(budget time.Duration) []string {
	var screen screenRows
	if len(m.cache) > len(m.entries) {
		m.cache = m.cache[:len(m.entries)]
	} else if diff := len(m.entries) - len(m.cache); diff > 0 {
		m.cache = append(m.cache, make([]renderedEntry, diff)...)
	}
	started, rendered := time.Now(), 0
	m.renderPending = false
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		limit := 0
		if e.Kind == "assistant" || e.Kind == "user" || e.Kind == "compaction" || e.Kind == "turn_end" {
			limit = -1
		}
		cached := &m.cache[i]
		if cached.width == m.width && cached.source == e && cached.maxLines == limit && !cached.draft {
			continue
		}
		draft := budget > 0 && e.Kind == "assistant" && rendered > 0 && time.Since(started) >= budget
		if draft {
			m.renderPending = true
			if cached.draft && cached.width == m.width && cached.source == e && cached.maxLines == limit {
				continue
			}
		}
		cached.source, cached.width, cached.maxLines, cached.draft = e, m.width, limit, draft
		cached.Text = renderEntryMode(e, m.width, limit, !draft)
		cached.rows = strings.Split(cached.Text, "\n")
		if e.Kind == "assistant" && !draft {
			rendered++
		}
	}
	for _, cached := range m.cache {
		if cached.Text != "" {
			screen.addLines(cached.rows)
			screen.add("\n\n")
		}
	}
	screen.add(m.workingBlock())
	screen.add(m.taskBlock())
	// Remove only trailing boundary whitespace, preserving all message-internal rows.
	for len(screen) > 0 && strings.TrimSpace(ansi.Strip(screen[len(screen)-1])) == "" {
		screen = screen[:len(screen)-1]
	}
	inputPrefix := ""
	if len(screen) > 0 {
		// screenRows joins chunks at their boundary; the leading newline keeps
		// one actual empty row between content and the composer rail.
		inputPrefix = "\n\n"
	}
	m.inputTop = len(screen)
	if inputPrefix != "" {
		m.inputTop++
	}
	screen.add(inputPrefix + m.inputBlock())
	screen.add("\n\n")
	start, rows := 0, min(8, max(1, m.height-m.input.Height()-5))
	if count := len(m.fileCompletions); m.hasFileCompletions() {
		start = max(0, m.fileSelected%count-rows+1)
		for i := start; i < min(count, start+rows); i++ {
			selected := i == m.fileSelected%count
			marker, fg := "  ", grayColor
			if selected {
				marker, fg = " <", agentColor
				if m.width < ansi.StringWidth(marker) {
					marker = "<"
				}
			}
			label := ansi.Truncate(sanitizeLabel(m.fileCompletions[i]), max(0, m.width-ansi.StringWidth(marker)), "…")
			row := lipgloss.NewStyle().Foreground(fg).Render(label) + lipgloss.NewStyle().Foreground(accentColor).Render(marker)
			screen.add(row + "\n")
		}
		if remaining := count - min(count, start+rows); remaining > 0 {
			screen.add(lipgloss.NewStyle().Foreground(grayColor).Render(fmt.Sprintf("  + %d more items", remaining)) + "\n")
		}
	}
	return screen
}

func TestTimelineSegmentsMatchReference(t *testing.T) {
	build := func(m model, n int) model {
		m.entries = nil
		for i := 0; i < n; i++ {
			kind, text := "user", fmt.Sprintf("question %d", i)
			if i%2 == 1 {
				kind, text = "assistant", fmt.Sprintf("**answer** %d\n\n- a\n- b", i)
			}
			m.entries = append(m.entries, entry{Kind: kind, Text: text})
		}
		return m
	}
	base := bareModel(t, t.TempDir())
	base.width, base.height = 60, 20
	for _, tc := range []struct {
		name    string
		entries int
		input   string
		files   []string
	}{
		{"empty", 0, "", nil},
		{"entries", 5, "hello\nworld", nil},
		{"completions", 3, "@ma", []string{"main.go", "makefile", "map.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := build(base, tc.entries), build(base, tc.entries)
			a.cache, b.cache = nil, nil
			for _, m := range []*model{&a, &b} {
				m.input.SetValue(tc.input)
				m.fileCompletions = tc.files
				m.fileSelected = 1
			}
			want := a.referenceTimelineRows(0)
			head, tail := b.timelineSegments(0)
			got := append(slices.Clone(head), tail...)
			if !slices.Equal(want, got) || a.inputTop != b.inputTop {
				t.Fatalf("segments differ (inputTop %d vs %d)\nwant %q\ngot  %q", a.inputTop, b.inputTop, want, got)
			}
			// A second build reuses the history and must not change anything.
			head2, tail2 := b.timelineSegments(0)
			if !slices.Equal(got, append(slices.Clone(head2), tail2...)) {
				t.Fatal("rebuild changed the rows")
			}
			// Changing an entry invalidates the cached history.
			if tc.entries > 0 {
				b.entries[0].Text = "changed"
				head3, tail3 := b.timelineSegments(0)
				if !strings.Contains(strings.Join(append(slices.Clone(head3), tail3...), "\n"), "changed") {
					t.Fatal("stale history after an entry changed")
				}
			}
		})
	}
}

func TestHighlightBashWrapsAndKeepsText(t *testing.T) {
	lines, ok := highlightBash("echo \"abcdefgh\" # note\n\tcat x\n", 10)
	if !ok {
		t.Fatal("bash lexer missing")
	}
	var plain []string
	for _, l := range lines {
		plain = append(plain, ansi.Strip(l))
	}
	want := []string{"echo \"abcd", "efgh\" # no", "te", "    cat x"}
	if strings.Join(plain, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", plain)
	}
}

func TestWorkingIndicatorShowsActivity(t *testing.T) {
	status := core.Status{Busy: true, Started: time.Now().Add(-69*time.Second - 300*time.Millisecond), Activity: "go test ./..."}
	if got := workingText(status, 4, true); got != "1m9s · 4 steps · go test ./..." {
		t.Fatalf("working text = %q", got)
	}
	if got := ansi.Truncate(workingText(status, 4, true), 20, "…"); !strings.HasPrefix(got, "1m9s · 4 steps") {
		t.Fatalf("narrow indicator lost time or steps: %q", got)
	}
	status.Activity = ""
	if got := workingText(status, 4, true); got != "1m9s · 4 steps · thinking..." {
		t.Fatalf("indicator without an action = %q", got)
	}
	for _, activity := range []string{"", "go test ./..."} {
		status.Activity = activity
		if got := workingText(status, 4, false); got != "1m9s · 4 steps" {
			t.Fatalf("show_bash=false leaves %q (activity %q)", got, activity)
		}
	}
	status.Activity, status.Cancelling = "go test ./...", true
	if got := workingText(status, 4, true); got != "1m9s · cancelling" {
		t.Fatalf("cancelling indicator = %q", got)
	}
}

// A running multi-line command cycles through its lines; a finished one rests on its first.
func TestWorkingIndicatorCyclesLines(t *testing.T) {
	since := time.Now()
	status := core.Status{Busy: true, Activity: "cd x\nmake\nls", Live: true, LiveSince: since}
	var got []string
	for i := range 4 {
		got = append(got, activityLine(status, since.Add(time.Duration(i)*activityStep+time.Millisecond)))
	}
	if strings.Join(got, "|") != "cd x|make|ls|cd x" {
		t.Fatalf("cycle = %q", got)
	}
	status.Live = false
	if got := activityLine(status, since.Add(activityStep)); got != "cd x" {
		t.Fatalf("finished command = %q", got)
	}
}

// The # list filters the registered names, Tab writes the token, and Enter expands
// it outside the UI loop: the composer is read-only meanwhile and takes the
// expanded text. A failing completion keeps the draft.
func TestPromptCompletionComposerFlow(t *testing.T) {
	f := newFixture(t, true)
	f.completions(t, `plan = "PLAN BODY"
polish = "POLISH BODY"
boom = "${{ exit 3 }}"`)

	typed := func(m model, s string) model { return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	m := typed(f.m, "#")
	if got := strings.Join(m.promptItems, " "); got != "boom plan polish" || !m.hasPromptCompletions() {
		t.Fatalf("list after # = %q", got)
	}
	m = typed(m, "pl")
	if got := strings.Join(m.promptItems, " "); got != "plan" {
		t.Fatalf("filtered list = %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "#plan" || m.hasPromptCompletions() {
		t.Fatalf("Tab must write #plan and close the list: %q", m.input.Value())
	}
	m = typed(m, " now")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.promptPending == 0 || cmd == nil {
		t.Fatal("Enter must start the expansion")
	}
	m = typed(m, "x") // read-only while the expansion runs
	if m.input.Value() != "#plan now" {
		t.Fatalf("composer changed during expansion: %q", m.input.Value())
	}
	next, _ = m.Update(cmd())
	m = awaitIdle(t, next.(model))
	if m.promptPending != 0 || m.input.Value() != "" {
		t.Fatalf("expanded message must be taken: pending=%d composer=%q", m.promptPending, m.input.Value())
	}
	if !hasEntry(m, "user", "<completion name=\"plan\">\nPLAN BODY\n</completion> now") {
		t.Fatalf("the agent must receive the expanded text: %+v", m.entries)
	}

	// a failing completion keeps the draft and says why
	m = typed(m, "#boom")
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	next, _ = m.Update(cmd())
	m = settle(next.(model))
	if m.input.Value() != "#boom" || m.promptPending != 0 || hasEntry(m, "user", "boom") {
		t.Fatalf("failed expansion must keep the draft unsent: %q, entries = %+v", m.input.Value(), m.entries)
	}
	if !hasEntry(m, "prompt_error", "your draft is kept") {
		t.Fatalf("the reason must be shown in the chat: %+v", m.entries)
	}
}

func hasEntry(m model, kind, text string) bool {
	for _, e := range m.entries {
		if e.Kind == kind && strings.Contains(e.Text, text) {
			return true
		}
	}
	return false
}

// Ctrl+D cancels a slow completion: nothing is sent, the draft stays in the
// composer, the reason shows, and a result that arrives afterwards is dropped.
func TestPromptExpansionCancelKeepsDraft(t *testing.T) {
	f := newFixture(t, true)
	f.completions(t, `slow = "${{ sleep 30 }}SLOW BODY"`)

	typed := func(m model, s string) model { return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	m := typed(f.m, "#slow now")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.promptPending == 0 || cmd == nil {
		t.Fatal("Enter must start the expansion")
	}
	seq := m.promptPending
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = settle(next.(model))
	if m.promptPending != 0 || m.input.Value() != "#slow now" {
		t.Fatalf("cancel must end the expansion and keep the draft: pending=%d composer=%q", m.promptPending, m.input.Value())
	}
	if !hasEntry(m, "prompt_error", "cancelled; your draft is kept") {
		t.Fatalf("the cancel must say why: %+v", m.entries)
	}
	select {
	case msg := <-result:
		if pe, ok := msg.(promptExpanded); !ok || pe.err == nil || pe.text != "" {
			t.Fatalf("cancelled expansion returned %#v", msg)
		}
		next, _ = m.Update(msg)
		m = settle(next.(model))
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled expansion kept running")
	}
	// A result that somehow arrives late is ignored: its expansion is over.
	next, _ = m.Update(promptExpanded{seq: seq, session: m.ctl.Status().Session, draft: "#slow now", text: "SLOW BODY now"})
	m = settle(next.(model))
	if hasEntry(m, "user", "SLOW BODY") || m.input.Value() != "#slow now" {
		t.Fatalf("a late result was applied: composer=%q entries=%+v", m.input.Value(), m.entries)
	}
	// The composer is usable again, and a fresh send works.
	m = typed(m, " x")
	if m.input.Value() != "#slow now x" {
		t.Fatalf("composer still read-only after cancel: %q", m.input.Value())
	}
}

// The # list wraps in both directions, and accepting a name in the middle of the
// text replaces only its token, leaving the rest of the composer as it was.
func TestPromptCompletionWrapsAndReplacesMidText(t *testing.T) {
	f := newFixture(t, true)
	f.completions(t, `plan = "PLAN BODY"
polish = "POLISH BODY"
boom = "BOOM"`)

	typed := func(m model, s string) model { return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	m := typed(f.m, "#")
	if got := strings.Join(m.promptItems, " "); got != "boom plan polish" {
		t.Fatalf("list = %q", got)
	}
	selected := func(m model) string { items, i := m.completionList(); return items[i] }
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if got := selected(m); got != "polish" {
		t.Fatalf("up from the first name must wrap to the last, got %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := selected(m); got != "boom" {
		t.Fatalf("down from the last name must wrap to the first, got %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := selected(m); got != "plan" {
		t.Fatalf("down from boom must select plan, got %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc}) // closes the list, the text stays
	m = typed(m, "x")
	m = key(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	m = typed(m, "say #pl now")
	for range len(" now") {
		m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	if got := strings.Join(m.promptItems, " "); got != "plan" {
		t.Fatalf("list at the cursor after #pl = %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "say #plan now" {
		t.Fatalf("mid-text accept must replace only the token: %q", m.input.Value())
	}
}

// A result that belongs to another session is not sent: the draft stays and the
// reason shows in the chat.
func TestPromptExpansionSessionChangeKeepsDraft(t *testing.T) {
	f := newFixture(t, true)
	f.completions(t, `plan = "PLAN BODY"`)
	m := key(f.m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("#plan")})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	seq := m.promptPending
	if seq == 0 {
		t.Fatal("Enter must start the expansion")
	}
	next, _ = m.Update(promptExpanded{seq: seq, session: "some-other-session", draft: "#plan", text: "PLAN BODY"})
	m = settle(next.(model))
	if m.promptPending != 0 || m.input.Value() != "#plan" || hasEntry(m, "user", "PLAN BODY") {
		t.Fatalf("a result from another session was applied: pending=%d composer=%q", m.promptPending, m.input.Value())
	}
	if !hasEntry(m, "prompt_error", "the session changed while the prompt was expanding") {
		t.Fatalf("the reason must be shown: %+v", m.entries)
	}
}

// ---- test helpers ----

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
	a       *core.Runtime
	fail    atomic.Bool
	hold    atomic.Pointer[chan struct{}]
	reached chan struct{}
	project string
}

// completions appends a [completions] table to ~/.zel/zel.toml and reloads.
func (f *fixture) completions(t *testing.T, table string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(f.a.Home(), "zel.toml"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n[completions]\n" + table + "\n")
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	f.m.ctl.Reload()
	f.m = settle(f.m)
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
	s := core.DefaultSettings()
	if providers {
		s.Provider, s.Model = "p", "m"
		s.Providers["p"] = core.Provider{BaseURL: server.URL}
	}
	if err := core.WriteConfig(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := core.New(f.project, core.Options{})
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
	next, _ := f.m.sendInput(core.Input{Text: text})
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

func heldModel(t *testing.T) (model, *core.Runtime, func()) {
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
	if err := m.ctl.Input(core.Input{Text: "busy"}); err != nil {
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

func agentEvent(e core.TurnEvent) core.Event { return core.Event{Kind: "agent", Agent: &e} }

func freshAgent(t testing.TB) (*core.Runtime, string) {
	t.Helper()
	t.Setenv("ZEL_HOME", t.TempDir())
	dir := t.TempDir()
	a, err := core.New(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, dir
}

func freshModel(t testing.TB) (model, *core.Runtime) {
	t.Helper()
	a, dir := freshAgent(t)
	m := newModel(a, dir)
	t.Cleanup(m.ctl.Close)
	return m, a
}

func bareModel(t testing.TB, cwd string) model {
	t.Helper()
	m, _ := freshModel(t)
	m.cwd = cwd
	m.menu = nil // Most unit tests exercise chat, not first-run onboarding.
	return m
}

func renderEntry(e entry, width, limit int) string { return renderEntryMode(e, width, limit, true) }

func (m *model) timeline() string { return strings.Join(m.timelineRows(0), "\n") }

// setConfig changes the settings the way the menus do: into zel.toml, then reload.
func setConfig(t *testing.T, ctl *core.Controller, change func(*core.Settings)) {
	t.Helper()
	s := ctl.Settings()
	change(&s)
	if err := ctl.UpdateSettings(s); err != nil {
		t.Fatal(err)
	}
}

func (m *model) applyAgent(e core.TurnEvent) {
	m.entries = core.ApplyEvent(m.entries, e)
	if e.Meta != nil && e.Meta.ContextInput != nil {
		m.contextTokens = *e.Meta.ContextInput
	}
}

// Colors changed behind the UI's back (hot reload) apply on the next message, with
// no key press, the composer included.
func TestThemeFollowsReload(t *testing.T) {
	defer applyTheme(config.DefaultColors())
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := bareModel(t, t.TempDir())
	m.refresh(true)
	setConfig(t, m.ctl, func(s *core.Settings) { s.Colors.Set("bg", "#102030"); s.Colors.Set("user_highlight", "#405060") })
	next, _ := m.Update(redraw{})
	m = next.(model)
	m.input.SetValue("draft")
	if bgColor != "#102030" || userHighlight != "#405060" || !strings.Contains(m.input.View(), "38;2;64;80;96mdraft") {
		t.Fatalf("theme after reload: bg %q highlight %q composer %q", bgColor, userHighlight, m.input.View())
	}
}

func TestPlainPathsUnderlinedAndWrapped(t *testing.T) {
	oldBase := linkBase
	linkBase = t.TempDir()
	t.Cleanup(func() { linkBase = oldBase })
	name := "long_existing_file-name.txt"
	if err := os.WriteFile(filepath.Join(linkBase, name), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{12, 24, 100} {
		text := "See " + name + ":12:3, `" + name + "` and missing-file.txt. https://example.com/a?q=1&b=2\n\n```\n" + name + "\n```"
		out, ok := renderMarkdown(text, width, false)
		if !ok || !strings.Contains(out, "\x1b[4m") || strings.ContainsAny(out, "\u2061\u2062\u2063\u2064") {
			t.Fatalf("bad rendering at %d: %q", width, out)
		}
		if !strings.Contains(out, ansi.SetHyperlink("file://"+filepath.Join(linkBase, name))) || !strings.Contains(out, ansi.SetHyperlink("https://example.com/a?q=1&b=2")) {
			t.Fatalf("missing targets at %d: %q", width, out)
		}
		for _, row := range linkedRows(out, width) {
			if ansi.StringWidth(row) > width {
				t.Fatalf("overflow: %q", row)
			}
			plain := ansi.Strip(row)
			if at := strings.Index(plain, "missing-file"); at >= 0 && linkAt(row, ansi.StringWidth(plain[:at])) != "" {
				t.Fatalf("missing path linked: %q", row)
			}
		}
		// Two file references and one URL, never the fenced code.
		if strings.Count(out, "\x1b]8;") != 6 {
			t.Fatalf("unexpected links: %q", out)
		}
	}
	for _, text := range []string{"missing-file.txt", "`missing-file.txt`", "[unsafe](javascript:alert)", "    " + name} {
		out, _ := renderMarkdown(text, 80, false)
		if strings.Contains(out, "\x1b]8;") {
			t.Fatalf("unexpected link in %q: %q", text, out)
		}
	}
	trace, _ := renderTraceMarkdown(name, 80, grayColor)
	if strings.Contains(trace, "\x1b]8;") {
		t.Fatal("plain paths in tracing should not be linked")
	}
}

func TestInlinePathWithSpacesAndUnderlineReset(t *testing.T) {
	oldBase := linkBase
	linkBase = t.TempDir()
	t.Cleanup(func() { linkBase = oldBase })
	if err := os.WriteFile(filepath.Join(linkBase, "my file.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, _ := renderMarkdown("`my file.txt:3`", 80, false)
	if !strings.Contains(out, "my%20file.txt") {
		t.Fatalf("space-containing path not linked: %q", out)
	}
	rows := linkedRows(terminalLink("https://example.com", "first\x1b[0m\nsecond", false), 80)
	if !strings.Contains(rows[0], "\x1b[0m\x1b[4m") || !strings.Contains(rows[1], "\x1b[4msecond") || !strings.HasSuffix(rows[1], "\x1b[24m"+ansi.ResetHyperlink()) {
		t.Fatalf("underline lost or leaked: %q", rows)
	}
}

func TestHookRecordsHiddenOnlyFromTracing(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.ctl.LogEvent("hook", "private hook output")
	m.ctl.LogEvent("notice", "visible notice")
	for _, e := range m.sessionTrace() {
		if e.Kind == "hook" || strings.Contains(e.Text, "private hook output") {
			t.Fatalf("hook visible in tracing: %+v", e)
		}
	}
	found := false
	for _, e := range m.ctl.Trace() {
		found = found || e.Kind == "hook" && e.Text == "private hook output"
	}
	if !found {
		t.Fatal("UI filtering removed the stored diagnostic")
	}
	m.menu = &menuState{page: menuTracing, trace: m.sessionTrace(), query: "private hook output"}
	req, ok := m.menuRequest()
	if !ok {
		t.Fatal("no live tracing request")
	}
	m.applyMenuSnapshot(req.load(m.ctl))
	for _, e := range m.menu.trace {
		if e.Kind == "hook" {
			t.Fatal("live refresh restored a hidden hook")
		}
	}
	if rows := m.menuRows(); len(rows) != 1 || rows[0].label != "No matches" {
		t.Fatalf("hook searchable: %+v", rows)
	}
}
