package luart

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// slashGlob implements the common path glob grammar, including a recursive **.
func slashGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); {
		r, n := utf8.DecodeRuneInString(pattern[i:])
		i += n
		switch r {
		case '*':
			if i < len(pattern) && pattern[i] == '*' && (i == 1 || pattern[i-2] == '/') && (i+1 == len(pattern) || pattern[i+1] == '/') {
				i++
				if i < len(pattern) {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '\\':
			if i == len(pattern) {
				b.WriteString(`\\`)
				continue
			}
			_, n := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(regexp.QuoteMeta(pattern[i : i+n]))
			i += n
		case '[':
			start := i
			if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
				i++
			}
			if i < len(pattern) && pattern[i] == ']' {
				i++
			}
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				i = start
				continue
			}
			end += i
			b.WriteByte('[')
			if pattern[start] == '!' {
				b.WriteByte('^')
				start++
			}
			if pattern[start] == ']' {
				b.WriteByte('\\')
			}
			b.WriteString(pattern[start : end+1])
			i = end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

func (v *vm) fsGlob(L *lua.LState) int {
	const op = "glob"
	pattern := filepath.ToSlash(textArg(L, op, 1))
	root := v.env.Info().Cwd
	absolute := filepath.IsAbs(pattern)
	if i := strings.IndexAny(pattern, "*?[\\"); i >= 0 {
		if j := strings.LastIndexByte(pattern[:i], '/'); j >= 0 {
			root, pattern = v.resolve(pattern[:j+1]), pattern[j+1:]
		}
	} else {
		path := v.resolve(pattern)
		if _, err := os.Stat(path); err == nil {
			L.Push(toLua(L, []string{v.displayPath(path, absolute)}))
			return 1
		} else if !errors.Is(err, fs.ErrNotExist) {
			raiseErr(L, op, err)
		}
		L.Push(L.NewTable())
		return 1
	}
	re, err := slashGlob(pattern)
	if err != nil {
		raise(L, op, EINVAL, "%v", err)
	}
	out := L.NewTable()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if v.ctx.Err() != nil {
			return filepath.SkipAll
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel != "." && re.MatchString(filepath.ToSlash(rel)) {
			out.Append(lua.LString(v.displayPath(path, absolute)))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		raiseErr(L, op, err)
	}
	L.Push(out)
	return 1
}

func (v *vm) displayPath(path string, absolute bool) string {
	if !absolute {
		if rel, err := filepath.Rel(v.env.Info().Cwd, path); err == nil {
			path = rel
		}
	}
	return filepath.ToSlash(path)
}

type ignoreRule struct {
	base                       string
	re                         *regexp.Regexp
	dirOnly, basename, include bool
}

func readIgnore(dir string) []ignoreRule {
	f, _, err := openPlain(filepath.Join(dir, ".gitignore"), os.O_RDONLY, 0, false)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil || len(data) > maxReadBytes {
		return nil
	}
	var out []ignoreRule
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") {
			prefix := line[:len(line)-1]
			if (len(prefix)-len(strings.TrimRight(prefix, `\`)))%2 == 1 {
				break
			}
			line = prefix
		}
		if line == "" || line[0] == '#' {
			continue
		}
		r := ignoreRule{base: dir, include: line[0] == '!'}
		if r.include {
			line = line[1:]
		}
		r.dirOnly = strings.HasSuffix(line, "/")
		if r.dirOnly {
			line = line[:len(line)-1]
		}
		r.basename = !strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		if r.re, err = slashGlob(line); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func ignored(rules []ignoreRule, path string, dir bool) bool {
	skip := false
	for _, r := range rules {
		if r.dirOnly && !dir {
			continue
		}
		rel, err := filepath.Rel(r.base, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if r.basename {
			rel = filepath.Base(path)
		}
		if r.re.MatchString(filepath.ToSlash(rel)) {
			skip = !r.include
		}
	}
	return skip
}

// Seed rules from the cwd (or enclosing Git root) through the parent of an
// explicit search path. Check each directory before reading its ignore file:
// Git does not re-include files underneath an excluded parent directory.
func parentIgnores(cwd, root string) ([]ignoreRule, bool) {
	stop := cwd
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			stop = dir
			break
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	var dirs []string
	for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == stop {
			break
		}
		if dir == filepath.Dir(dir) {
			return nil, false
		}
	}
	var rules []ignoreRule
	for i := len(dirs) - 1; i >= 0; i-- {
		if ignored(rules, dirs[i], true) {
			return nil, true
		}
		rules = append(rules, readIgnore(dirs[i])...)
	}
	return rules, false
}

func (v *vm) fsGrep(L *lua.LState) int {
	const op = "grep"
	pattern := textArg(L, op, 1)
	opts := opts(L, op, 2)
	if boolOpt(L, op, opts.RawGetString("fixed")) {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		raise(L, op, EINVAL, "%v", err)
	}
	limit := 200
	if max := opts.RawGetString("max"); max != lua.LNil {
		limit = intArg(L, op, max)
	}
	if limit < 0 {
		raise(L, op, EINVAL, "max must not be negative")
	}
	var glob *regexp.Regexp
	basename := false
	if val := opts.RawGetString("glob"); val != lua.LNil {
		p := asString(L, op, val)
		basename = !strings.Contains(p, "/") && p != "**"
		glob, err = slashGlob(p)
		if err != nil {
			raise(L, op, EINVAL, "%v", err)
		}
	}
	cwd := v.env.Info().Cwd
	root := cwd
	if val := opts.RawGetString("path"); val != lua.LNil {
		root = v.resolve(asString(L, op, val))
	}
	info, err := os.Stat(root)
	if err != nil {
		raiseErr(L, op, err)
	}
	if err := checkPlain(info, true); err != nil {
		fileError(L, op, err)
	}
	out := L.NewTable()
	rules, skip := parentIgnores(cwd, root)
	var rows []grepHit
	for p := root; p != filepath.Dir(p); p = filepath.Dir(p) {
		if filepath.Base(p) == ".git" {
			skip = true
			break
		}
	}
	var walk func(string, []ignoreRule) error
	walk = func(path string, rules []ignoreRule) error {
		if len(rows) >= limit || v.ctx.Err() != nil {
			return nil
		}
		if filepath.Base(path) == ".git" {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if path == root && info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(path)
			if err != nil {
				return err
			}
		}
		if ignored(rules, path, info.IsDir()) {
			return nil
		}
		if info.IsDir() {
			rules = append(rules, readIgnore(path)...)
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := walk(filepath.Join(path, e.Name()), rules); err != nil {
					return err
				}
				if len(rows) >= limit || v.ctx.Err() != nil {
					break
				}
			}
		} else if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			if basename || rel == "." {
				rel = filepath.Base(path)
			}
			if glob == nil || glob.MatchString(filepath.ToSlash(rel)) {
				hits, err := v.grepFile(path, re, limit-len(rows))
				if err != nil {
					return err
				}
				rows = append(rows, hits...)
			}
		}
		return nil
	}
	if !skip {
		if err := walk(root, rules); err != nil {
			fileError(L, op, err)
		}
	}
	for _, r := range rows {
		out.Append(toLua(L, map[string]any{"file": r.file, "line": r.line, "text": r.text}))
	}
	L.Push(out)
	return 1
}

type grepHit struct {
	file, text string
	line       int
}

func (v *vm) grepFile(path string, re *regexp.Regexp, limit int) ([]grepHit, error) {
	f, _, err := openPlain(path, os.O_RDONLY, 0, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	file := v.displayPath(path, false)
	var rows []grepHit
	var line []byte
	n := 0
	for v.ctx.Err() == nil {
		part, err := r.ReadSlice('\n')
		if bytes.IndexByte(part, 0) >= 0 {
			return nil, nil
		}
		if len(rows) < limit {
			line = append(line, part...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		n++
		if len(line) > 0 {
			text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if re.Match(text) {
				rows = append(rows, grepHit{file, string(text), n})
			}
			line = line[:0]
		}
		if err == io.EOF {
			break
		}
	}
	return rows, nil
}
