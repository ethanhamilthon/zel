package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

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
	text = strings.ReplaceAll(text, "\x1b[0m", "\x1b[0m\x1b[4m")
	return ansi.SetHyperlink(u.String()) + "\x1b[4m" + text + "\x1b[24m" + ansi.ResetHyperlink()
}

var (
	markedLink = regexp.MustCompile("(?s)\u2063(.*?)\u2064")
	markedCode = regexp.MustCompile("(?s)\u2062(.*?)\u2061")
	codeSpan   = regexp.MustCompile("`([^`\n]+)`")
	pathText   = regexp.MustCompile(`^[\pL\pN_ ./~+@:%?=&,#-]+$`)
	lineSuffix = regexp.MustCompile(`(:\d+){1,2}$`)
	linkBase   string // the directory relative paths in backticks are resolved against

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
		target := codeTarget(strings.TrimSpace(ansi.Strip(label)))
		if target == "" {
			target = codeTarget(strings.Join(strings.Fields(ansi.Strip(label)), ""))
		}
		return terminalLink(target, label, true)
	})
	return strings.NewReplacer("\u2063", "", "\u2064", "", "\u2062", "", "\u2061", "").Replace(body)
}

// codeTarget is the link for the text of an inline code span: an http(s) URL, or the
// file:// URL of an existing file or directory (relative to linkBase, with an optional
// :line:column suffix). It is empty for any other text.
type pathResult struct {
	target string
	at     time.Time
}

var pathTargets = map[[2]string]pathResult{}

func codeTarget(text string) string {
	key := [2]string{linkBase, text}
	if cached, ok := pathTargets[key]; ok && time.Since(cached.at) < 5*time.Second {
		return cached.target
	}
	target := resolveCodeTarget(text)
	if len(pathTargets) >= 4096 {
		clear(pathTargets)
	}
	pathTargets[key] = pathResult{target, time.Now()}
	return target
}

func resolveCodeTarget(text string) string {
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
	render := func(body string) (string, bool) {
		return renderMarkdownStyle(markPlainTargets(cleanLinkMarkers(body)), width, false, markdownStyle)
	}
	if split := stableSplit(text); streaming && split > 0 {
		key := [5]any{width, lipgloss.ColorProfile(), markdownStyle, linkBase, text[:split]}
		if mdStable.key != key {
			out, ok := render(text[:split])
			if !ok {
				return "", false
			}
			mdStable.key, mdStable.out = key, out
		}
		tail, ok := render(text[split:])
		return mdStable.out + "\n\n" + tail, ok
	}
	return render(text)
}

func renderMarkdownStyle(text string, width int, streaming bool, style string) (string, bool) {
	width = max(1, width)
	if key := [3]any{width, lipgloss.ColorProfile(), style}; mdKey != key {
		mdKey = key
		mdRenderer, _ = glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(style)), glamour.WithWordWrap(width), glamour.WithColorProfile(lipgloss.ColorProfile()), glamour.WithInlineTableLinks(true))
	}
	if mdRenderer == nil {
		return "", false
	}
	text = markCodeTargets(text)

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
	for offset < len(text) {
		end := strings.IndexByte(text[offset:], '\n')
		if end < 0 {
			break
		}
		end += offset + 1
		line := strings.TrimSpace(text[offset:end])
		switch {
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			fence = !fence
		case line == "" && !fence && offset > 0:
			split = end
		}
		offset = end
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

// Protect inline code and Markdown links from the plain-text scanner. Fenced
// code is untouched, and punctuation next to a path is not part of its target.
var plainTargetToken = regexp.MustCompile("`+[^`]*`+|!?\\[[^\\]]*\\]\\([^\\n]*?\\)|[^\\s<>`\\[\\]()\"'\\x{2061}-\\x{2064}]+")

func markPlainTargets(text string) string {
	lines, fence := strings.Split(text, "\n"), ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if fence == "" {
				fence = trimmed[:3]
			} else if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if fence != "" || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		lines[i] = plainTargetToken.ReplaceAllStringFunc(line, func(token string) string {
			if strings.ContainsAny(token, "`[]") {
				return token
			}
			label := strings.TrimRight(token, ".,;!?")
			// Avoid turning ordinary words (even if a same-named file exists)
			// into links; plain paths must look like paths.
			if !strings.ContainsAny(label, "/.") || codeTarget(label) == "" {
				return token
			}
			// Escape Markdown punctuation without changing the displayed text.
			escaped := strings.NewReplacer("\\", "\\\\", "_", "\\_", "*", "\\*", "#", "\\#").Replace(label)
			return "\u2062" + escaped + "\u2061" + token[len(label):]
		})
	}
	return strings.Join(lines, "\n")
}

func cleanLinkMarkers(text string) string {
	return strings.NewReplacer("\u2063", "", "\u2064", "", "\u2062", "", "\u2061", "").Replace(text)
}
