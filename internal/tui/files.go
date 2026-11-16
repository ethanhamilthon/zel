package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"zel/internal/core"
)

type fileToken struct {
	start, end int
	query      string
}

const fileMaxCandidates = 100

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) >= 0
}

func quotePath(p string) string {
	if !strings.ContainsAny(p, " \t\n\"'\\") {
		return "@" + p
	}
	return "@\"" + strings.ReplaceAll(strings.ReplaceAll(p, "\\", "\\\\"), "\"", "\\\"") + "\""
}

func unescapeQuoted(s string) string {
	var sb strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) {
			i++
		}
		sb.WriteRune(runes[i])
	}
	return sb.String()
}

func findFileToken(text string, cursor int) (fileToken, bool) {
	runes := []rune(text)
	if cursor < 0 || cursor > len(runes) {
		return fileToken{}, false
	}
	for at := cursor - 1; at >= 0 && runes[at] != '\n'; at-- {
		if runes[at] == '@' {
			return parseFileToken(runes, at, cursor)
		}
	}
	return fileToken{}, false
}

func parseFileToken(runes []rune, at, cursor int) (fileToken, bool) {
	if at > 0 && (unicode.IsLetter(runes[at-1]) || unicode.IsDigit(runes[at-1]) || strings.ContainsRune("._%+-\\/:", runes[at-1])) {
		return fileToken{}, false
	}
	lineStart := at
	for lineStart > 0 && runes[lineStart-1] != '\n' {
		lineStart--
	}
	lp := string(runes[lineStart:at])
	if sp := strings.LastIndexByte(lp, ' '); sp >= 0 {
		lp = lp[sp+1:]
	}
	if strings.Contains(lp, "://") {
		return fileToken{}, false
	}
	var tokenEnd int
	var query string
	if at+1 < len(runes) && (runes[at+1] == '"' || runes[at+1] == '\'') {
		quote := runes[at+1]
		qEnd := at + 2
		for qEnd < len(runes) && runes[qEnd] != '\n' {
			if runes[qEnd] == '\\' && qEnd+1 < len(runes) && runes[qEnd+1] != '\n' {
				qEnd += 2
				continue
			}
			if runes[qEnd] == quote {
				break
			}
			qEnd++
		}
		tokenEnd = qEnd
		if qEnd < len(runes) && runes[qEnd] == quote {
			tokenEnd++
		}
		if cursor < at || cursor > tokenEnd {
			return fileToken{}, false
		}
		if end := min(cursor, qEnd); end > at+2 {
			query = unescapeQuoted(string(runes[at+2 : end]))
		}
	} else {
		tokenEnd = at + 1
		for tokenEnd < len(runes) && !unicode.IsSpace(runes[tokenEnd]) && !strings.ContainsRune(")]}>\\\";,`", runes[tokenEnd]) {
			tokenEnd++
		}
		if cursor < at || cursor > tokenEnd {
			return fileToken{}, false
		}
		query = string(runes[at+1 : cursor])
	}
	if strings.ContainsRune(query, '@') || hasControl(query) {
		return fileToken{}, false
	}
	return fileToken{start: at, end: tokenEnd, query: query}, true
}

func scanProjectFiles(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hasControl(e.Name()) {
			continue
		}
		name, isDir := e.Name(), e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(root, name)); err == nil {
				isDir = info.IsDir()
			}
		}
		if isDir {
			name += "/"
		}
		paths = append(paths, name)
	}
	return paths, nil
}

func fileQueryParts(query string) (string, string) {
	if query == "~" {
		query = "~/"
	}
	return filepath.Split(query)
}

func fileCompletionRoot(cwd, query string) (string, error) {
	dir, _ := fileQueryParts(query)
	resolved, err := core.ResolvePath(dir)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(resolved) {
		return filepath.Clean(resolved), nil
	}
	return filepath.Join(cwd, resolved), nil
}

func filterFileCompletions(indexed []string, query string) []string {
	prefix, query := fileQueryParts(query)
	qLower := strings.ToLower(query)
	var cat0, cat1, cat2 []string
	for _, p := range indexed {
		if strings.Contains(strings.TrimSuffix(p, "/"), "/") {
			continue
		}
		pLower := strings.ToLower(p)
		if strings.HasPrefix(pLower, qLower) {
			cat0 = append(cat0, prefix+p)
		} else if strings.Contains(pLower, qLower) {
			cat1 = append(cat1, prefix+p)
		} else if fuzzyMatches(pLower, qLower) {
			cat2 = append(cat2, prefix+p)
		}
	}
	slices.Sort(cat0)
	slices.Sort(cat1)
	slices.Sort(cat2)
	res := append(append(cat0, cat1...), cat2...)
	return res[:min(fileMaxCandidates, len(res))]
}
