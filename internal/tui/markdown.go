package tui

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// terminalLink wraps text in an OSC 8 hyperlink to an http(s) target; file targets
// are allowed only for code spans checked against the disk.
func terminalLink(target, text string, allowFile bool) string {
	u, err := url.Parse(target)
	web := err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
	if err != nil || !(web || allowFile && u.Scheme == "file" && u.Path != "") || strings.ContainsFunc(target, unicode.IsControl) {
		return text
	}
	return ansi.SetHyperlink(u.String()) + text + ansi.ResetHyperlink()
}

var (
	markedLink        = regexp.MustCompile("(?s)\u2063(.*?)\u2064")
	markedCode        = regexp.MustCompile("(?s)\u2062(.*?)\u2061")
	codeSpan          = regexp.MustCompile("`([^`\n]+)`")
	pathText          = regexp.MustCompile(`^[\w./~+@:-]+$`)
	lineSuffix        = regexp.MustCompile(`(:\d+){1,2}$`)
	linkBase          string // the directory relative paths in backticks are resolved against
	hyperlinkSequence = regexp.MustCompile("\x1b\\]8;[^\x1b\x07]*(?:\x07|\x1b\\\\)")
)

// Glamour's legacy reflow breaks OSC 8 sequences. Add hyperlinks only after
// rendering, using zero-width markers around URL text from the link formatter.
func markdownLinks(body string) string {
	body = markedLink.ReplaceAllStringFunc(body, func(marked string) string {
		label := strings.TrimSuffix(strings.TrimPrefix(marked, "\u2063"), "\u2064")
		target := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, ansi.Strip(label))
		return terminalLink(target, label, false)
	})
	body = markedCode.ReplaceAllStringFunc(body, func(marked string) string {
		label := strings.TrimSuffix(strings.TrimPrefix(marked, "\u2062"), "\u2061")
		return terminalLink(codeTarget(strings.Join(strings.Fields(ansi.Strip(label)), "")), label, true)
	})
	return strings.NewReplacer("\u2063", "", "\u2064", "", "\u2062", "", "\u2061", "").Replace(body)
}

// codeTarget is the link for the text of an inline code span: an http(s) URL, or the
// file:// URL of an existing file or directory (relative to linkBase, with an optional
// :line:column suffix). It is empty for any other text.
func codeTarget(text string) string {
	if len(text) > 500 || !pathText.MatchString(text) {
		return ""
	}
	if u, err := url.Parse(text); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		return u.String()
	}
	path := lineSuffix.ReplaceAllString(text, "")
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, rest)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(linkBase, path)
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// markCodeTargets wraps inline code spans holding a URL or an existing path in
// invisible markers that markdownLinks turns into hyperlinks after rendering.
func markCodeTargets(text string) string {
	lines, fence := strings.Split(text, "\n"), false
	for i, line := range lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
		} else if !fence && strings.Contains(line, "`") {
			lines[i] = codeSpan.ReplaceAllStringFunc(line, func(span string) string {
				if inner := span[1 : len(span)-1]; codeTarget(strings.TrimSpace(inner)) != "" {
					return "`\u2062" + inner + "\u2061`"
				}
				return span
			})
		}
	}
	return strings.Join(lines, "\n")
}

// linkAt is the hyperlink target under display column col of a rendered row, or "".
func linkAt(row string, col int) string {
	link, x, last := "", 0, 0
	for _, loc := range hyperlinkSequence.FindAllStringIndex(row, -1) {
		width := ansi.StringWidth(row[last:loc[0]])
		if col < x+width {
			return link
		}
		x, last = x+width, loc[1]
		_, uri, _ := strings.Cut(strings.TrimPrefix(row[loc[0]:loc[1]], "\x1b]8;"), ";")
		link = strings.TrimSuffix(strings.TrimSuffix(uri, "\x07"), "\x1b\\")
	}
	if col < x+ansi.StringWidth(row[last:]) {
		return link
	}
	return ""
}

type linkOpened struct {
	target string
	err    error
}

// openLink hands a clicked link to the system opener.
func openLink(target string) tea.Cmd {
	return func() tea.Msg {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if u, err := url.Parse(target); err == nil && u.Scheme == "file" {
			target = u.Path
		}
		cmd := exec.Command(opener, target)
		err := cmd.Start()
		if err == nil {
			go func() { _ = cmd.Wait() }()
		}
		return linkOpened{target, err}
	}
}

// Close links on each row so truncation, padding and viewport clipping cannot
// leak a link onto adjacent UI elements. Reopen wrapped links on the next row.
func linkedRows(body string, width int) []string {
	rows := strings.Split(body, "\n")
	active := ""
	for i, row := range rows {
		prefix := active
		for _, seq := range hyperlinkSequence.FindAllString(row, -1) {
			if seq == ansi.ResetHyperlink() {
				active = ""
			} else {
				active = seq
			}
		}
		rows[i] = ansi.Truncate(prefix+row, max(1, width), "…")
		if prefix != "" || strings.Contains(row, "\x1b]8;") {
			rows[i] += ansi.ResetHyperlink()
		}
	}
	return rows
}

var (
	mdKey      any
	mdRenderer *glamour.TermRenderer
	mdStable   struct {
		key any
		out string
	}
	urlToken = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+`)
)

func renderMarkdown(text string, width int, streaming bool) (string, bool) {
	width = max(1, width)
	if key := [3]any{width, lipgloss.ColorProfile(), markdownStyle}; mdKey != key {
		mdKey = key
		mdRenderer, _ = glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(markdownStyle)), glamour.WithWordWrap(width), glamour.WithColorProfile(lipgloss.ColorProfile()), glamour.WithInlineTableLinks(true))
	}
	if mdRenderer == nil {
		return "", false
	}
	text = markCodeTargets(strings.NewReplacer("\u2063", "", "\u2064", "", "\u2062", "", "\u2061", "").Replace(text))
	if split := stableSplit(text); streaming && split > 0 {
		if key := [2]any{mdKey, text[:split]}; mdStable.key != key {
			out, ok := renderGlamour(text[:split])
			if !ok {
				return "", false
			}
			mdStable.key, mdStable.out = key, out
		}
		tail, ok := renderGlamour(text[split:])
		if !ok {
			return "", false
		}
		return markdownLinks(mdStable.out + "\n\n" + tail), true
	}
	out, ok := renderGlamour(text)
	return markdownLinks(out), ok
}

func renderGlamour(text string) (string, bool) {
	protected := protectHyphens(text)
	rendered, err := mdRenderer.Render(protected)
	if err != nil {
		return "", false
	}
	if protected != text {
		rendered = strings.ReplaceAll(rendered, "\u2011", "-")
	}
	return strings.TrimSpace(rendered), true
}

func stableSplit(text string) int {
	split, fence, offset := 0, false, 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence = !fence
		case trimmed == "" && !fence && strings.HasSuffix(line, "\n") && offset > 0:
			split = offset + len(line)
		}
		offset += len(line)
	}
	if strings.TrimSpace(text[split:]) == "" {
		return 0
	}
	return split
}

// protectHyphens turns hyphens inside text into non-breaking hyphens. Glamour's
// word wrap does not count a hyphen's width, so lines with hyphens overflow and are
// broken again in the middle of a word. URLs and Markdown syntax keep their hyphens.
func protectHyphens(text string) string {
	if !strings.Contains(text, "-") || strings.Contains(text, "\u2011") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body := strings.TrimLeft(line, " \t>")
		if strings.Trim(body, "-|:+ \t") == "" {
			continue
		}
		head := len(line) - len(body)
		if strings.HasPrefix(body, "- ") {
			head++
		}
		var b strings.Builder
		b.WriteString(line[:head])
		rest, last := line[head:], 0
		for _, m := range urlToken.FindAllStringIndex(rest, -1) {
			b.WriteString(strings.ReplaceAll(rest[last:m[0]], "-", "\u2011"))
			b.WriteString(rest[m[0]:m[1]])
			last = m[1]
		}
		b.WriteString(strings.ReplaceAll(rest[last:], "-", "\u2011"))
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}
