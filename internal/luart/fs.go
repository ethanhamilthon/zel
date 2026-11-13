package luart

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

const maxReadBytes = 16 << 20

func (v *vm) fsErr(L *lua.LState, fn, path string, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		raise(L, fn, ENOENT, "%s", path)
	default:
		raise(L, fn, EFAIL, "%s: %v", path, err)
	}
}

func (v *vm) fsRead(L *lua.LState) int {
	path := L.CheckString(1)
	o := opts(L, 2)
	abs := v.resolve(path)
	info, err := os.Stat(abs)
	if err != nil {
		v.fsErr(L, "fs.read", path, err)
	}
	if info.IsDir() {
		raise(L, "fs.read", EFAIL, "%s is a directory", path)
	}
	from, hasFrom := optNumber(o, "from")
	count, hasLines := optNumber(o, "lines")
	if !hasFrom && !hasLines && info.Size() > maxReadBytes {
		raise(L, "fs.read", ELIMIT, "%s is %d bytes; read it in parts with {from=, lines=}", path, info.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		v.fsErr(L, "fs.read", path, err)
	}
	if hasFrom || hasLines {
		start := 1
		if hasFrom {
			start = max(int(from), 1)
		}
		lines := strings.SplitAfter(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if start > len(lines) {
			lines = nil
		} else {
			lines = lines[start-1:]
		}
		if hasLines && int(count) >= 0 && int(count) < len(lines) {
			lines = lines[:int(count)]
		}
		data = []byte(strings.Join(lines, ""))
	}
	L.Push(lua.LString(string(data)))
	return 1
}

func (v *vm) fsWrite(L *lua.LState) int {
	path, content := L.CheckString(1), L.CheckString(2)
	abs := v.resolve(path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		v.fsErr(L, "fs.write", path, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if optBool(opts(L, 3), "append") {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.OpenFile(abs, flags, mode)
	if err != nil {
		v.fsErr(L, "fs.write", path, err)
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		v.fsErr(L, "fs.write", path, err)
	}
	return 0
}

func (v *vm) fsEdit(L *lua.LState) int {
	path, old, repl := L.CheckString(1), L.CheckString(2), L.CheckString(3)
	all := optBool(opts(L, 4), "all")
	if old == "" {
		raise(L, "fs.edit", EINVAL, "old text is empty")
	}
	abs := v.resolve(path)
	data, err := os.ReadFile(abs)
	if err != nil {
		v.fsErr(L, "fs.edit", path, err)
	}
	text := string(data)
	n := strings.Count(text, old)
	switch {
	case n == 0:
		raise(L, "fs.edit", ENOMATCH, "no match for the old text in %s", path)
	case n > 1 && !all:
		raise(L, "fs.edit", EAMBIG, "%d matches in %s; add context or pass {all=true}", n, path)
	}
	text = strings.ReplaceAll(text, old, repl)
	info, _ := os.Stat(abs)
	if err := os.WriteFile(abs, []byte(text), info.Mode().Perm()); err != nil {
		v.fsErr(L, "fs.edit", path, err)
	}
	L.Push(lua.LNumber(n))
	return 1
}

func (v *vm) fsList(L *lua.LState) int {
	path := L.CheckString(1)
	entries, err := os.ReadDir(v.resolve(path))
	if err != nil {
		v.fsErr(L, "fs.list", path, err)
	}
	out := L.NewTable()
	for _, e := range entries {
		row := L.NewTable()
		row.RawSetString("name", lua.LString(e.Name()))
		row.RawSetString("dir", lua.LBool(e.IsDir()))
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		row.RawSetString("size", lua.LNumber(size))
		out.Append(row)
	}
	L.Push(out)
	return 1
}

func (v *vm) fsExists(L *lua.LState) int {
	info, err := os.Stat(v.resolve(L.CheckString(1)))
	switch {
	case err != nil:
		L.Push(lua.LFalse)
		L.Push(lua.LNil)
	case info.IsDir():
		L.Push(lua.LTrue)
		L.Push(lua.LString("dir"))
	default:
		L.Push(lua.LTrue)
		L.Push(lua.LString("file"))
	}
	return 2
}

func (v *vm) fsTmp(L *lua.LState) int {
	name := "tmp"
	if L.GetTop() >= 1 && L.Get(1) != lua.LNil {
		name = filepath.Base(L.CheckString(1))
	}
	dir := v.env.Info().TmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		raise(L, "fs.tmp", EFAIL, "%v", err)
	}
	unique, err := os.MkdirTemp(dir, "t")
	if err != nil {
		raise(L, "fs.tmp", EFAIL, "%v", err)
	}
	L.Push(lua.LString(filepath.Join(unique, name)))
	return 1
}

// globRegexp turns "internal/**/*.go" into a regexp over slash-separated paths.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
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
		case '{':
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				b.WriteString(`\{`)
				continue
			}
			alts := strings.Split(pattern[i+1:i+end], ",")
			for j := range alts {
				alts[j] = regexp.QuoteMeta(alts[j])
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// walkFiles visits regular files under root (skipping .git), paths relative to root.
func walkFiles(root string, visit func(rel string) bool) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !visit(filepath.ToSlash(rel)) {
			return filepath.SkipAll
		}
		return nil
	})
}

func (v *vm) fsGlob(L *lua.LState) int {
	pattern := L.CheckString(1)
	re, err := globRegexp(filepath.ToSlash(pattern))
	if err != nil {
		raise(L, "fs.glob", EINVAL, "%v", err)
	}
	cwd := v.env.Info().Cwd
	var found []string
	walkFiles(cwd, func(rel string) bool {
		if re.MatchString(rel) {
			found = append(found, rel)
		}
		return len(found) < 10000
	})
	sort.Strings(found)
	L.Push(toLua(L, found))
	return 1
}

func (v *vm) fsGrep(L *lua.LState) int {
	pattern := L.CheckString(1)
	o := opts(L, 2)
	fixed := optBool(o, "fixed")
	limit := 200
	if n, ok := optNumber(o, "max"); ok && n > 0 {
		limit = int(n)
	}
	path, _ := optString(o, "path")
	glob, _ := optString(o, "glob")
	root := v.env.Info().Cwd
	if path != "" {
		root = v.resolve(path)
	}
	if !fixed {
		if _, err := regexp.Compile(pattern); err != nil {
			raise(L, "fs.grep", EINVAL, "bad pattern: %v", err)
		}
	}
	rows, ok := v.rgGrep(root, pattern, glob, fixed, limit)
	if !ok {
		rows = goGrep(root, pattern, glob, fixed, limit)
	}
	out := L.NewTable()
	for _, r := range rows {
		row := L.NewTable()
		row.RawSetString("file", lua.LString(r.file))
		row.RawSetString("line", lua.LNumber(r.line))
		row.RawSetString("text", lua.LString(r.text))
		out.Append(row)
	}
	L.Push(out)
	return 1
}

type grepRow struct {
	file string
	line int
	text string
}

var rgLine = regexp.MustCompile(`^(.*?):(\d+):(.*)$`)

func (v *vm) rgGrep(root, pattern, glob string, fixed bool, limit int) ([]grepRow, bool) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		return nil, false
	}
	args := []string{"--line-number", "--no-heading", "--with-filename", "--color", "never", "--max-columns", "400"}
	if fixed {
		args = append(args, "-F")
	}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	args = append(args, "-e", pattern, "--", ".")
	cmd := exec.CommandContext(v.ctx, rg, args...)
	cmd.Dir = root
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return nil, false
	}
	var rows []grepRow
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() && len(rows) < limit {
		m := rgLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		rows = append(rows, grepRow{strings.TrimPrefix(m[1], "./"), n, m[3]})
	}
	return rows, true
}

func goGrep(root, pattern, glob string, fixed bool, limit int) []grepRow {
	if fixed {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	var globRe *regexp.Regexp
	if glob != "" {
		if !strings.Contains(glob, "/") {
			glob = "**/" + glob
		}
		globRe, _ = globRegexp(glob)
	}
	var rows []grepRow
	info, err := os.Stat(root)
	if err == nil && !info.IsDir() {
		return grepFile(rows, filepath.Base(root), root, re, limit)
	}
	walkFiles(root, func(rel string) bool {
		if globRe != nil && !globRe.MatchString(rel) {
			return true
		}
		rows = grepFile(rows, rel, filepath.Join(root, rel), re, limit)
		return len(rows) < limit
	})
	return rows
}

func grepFile(rows []grepRow, rel, abs string, re *regexp.Regexp, limit int) []grepRow {
	data, err := os.ReadFile(abs)
	if err != nil || len(data) > 2<<20 || bytes.IndexByte(data, 0) >= 0 {
		return rows
	}
	for i, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) {
			rows = append(rows, grepRow{rel, i + 1, line})
			if len(rows) >= limit {
				break
			}
		}
	}
	return rows
}
