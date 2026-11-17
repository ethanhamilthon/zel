package tui

import (
	"context"
	"strings"
	"time"
	"zel/internal/core"
)

type fileResult struct {
	root  string
	files []string
}

func fuzzyMatches(text, query string) bool {
	text = strings.ToLower(text)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		remaining := []rune(word)
		at := 0
		for _, r := range text {
			if at < len(remaining) && r == remaining[at] {
				at++
			}
		}
		if at < len(remaining) {
			return false
		}
	}
	return true
}
func (m *app) resetCompletions() { m.completions = list{}; m.completionKind = 0 }
func (m *app) updateCompletions() {
	if m.menu != nil || m.choice != nil || m.input.text == m.completionHidden {
		m.resetCompletions()
		return
	}
	text, pos := m.input.text, m.input.pos
	var items []item
	kind := byte(0)
	start, end := 0, 0
	if a, b, q, ok := core.PromptAt([]rune(text), runeOffset(text, pos)); ok {
		kind = '#'
		start = byteOffset(text, a)
		end = byteOffset(text, b)
		for _, name := range m.ctl.PromptNames() {
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(q)) {
				items = append(items, item{label: "#" + name, detail: firstLine(m.ctl.PromptPreview(name)), id: "#" + name})
			}
		}
	} else if token, ok := findFileToken(text, runeOffset(text, pos)); ok {
		kind = '@'
		start = byteOffset(text, token.start)
		end = byteOffset(text, token.end)
		root, err := fileCompletionRoot(m.cwd, token.query)
		if err == nil {
			if root != m.fileRoot {
				m.fileRoot = root
				m.fileIndex = nil
				m.fileAt = time.Time{}
			}
			if !m.fileLoading && time.Since(m.fileAt) > 5*time.Second {
				m.fileLoading = true
				m.work("files", func() (any, error) {
					ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
					defer cancel()
					paths, err := scanProjectFiles(ctx, root)
					return fileResult{root, paths}, err
				})
			}
			for _, path := range filterFileCompletions(m.fileIndex, token.query) {
				items = append(items, item{label: path, id: quotePath(path)})
			}
		}
	}
	if kind != m.completionKind || start != m.completionStart {
		m.completions.selected = 0
	}
	m.completionKind = kind
	m.completionStart = start
	m.completionEnd = end
	m.completions.items = items
	m.completions.clamp()
}
func (m *app) pickCompletion() { m.completePath(true) }

func (m *app) completePath(descend bool) {
	it, ok := m.completions.current()
	if !ok {
		return
	}
	text := it.id
	suffix := " "
	if descend && m.completionKind == '@' && strings.HasSuffix(it.label, "/") {
		suffix = ""
	}
	m.input.text = m.input.text[:m.completionStart] + text + suffix + m.input.text[m.completionEnd:]
	m.input.pos = m.completionStart + len(text+suffix)
	if suffix != "" {
		m.input.tokens = append(m.input.tokens, text)
	}
	m.composerChanged()
	m.resetCompletions()
	m.completionHidden = ""
	m.updateCompletions()
}
