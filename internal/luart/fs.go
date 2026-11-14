package luart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

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

// Refusals of openPlain: a directory where a file is wanted, and a named pipe,
// device, socket or other special file, which may wait for a peer that never comes.
var (
	errIsDir      = errors.New("is a directory")
	errNotRegular = errors.New("not a regular file: named pipes, devices and sockets are refused")
)

// checkPlain accepts a regular file, and a directory when dirOK.
func checkPlain(info os.FileInfo, dirOK bool) error {
	switch {
	case info.IsDir():
		if dirOK {
			return nil
		}
		return errIsDir
	case info.Mode().IsRegular():
		return nil
	}
	return errNotRegular
}

// openPlain opens abs for a host file operation without ever waiting on a special
// file: opening or reading a named pipe or device waits for a peer that may never
// come. A stat refuses known special files; O_NONBLOCK makes the open itself return
// at once; the type is checked again on the open descriptor, so a path replaced
// between the two checks is refused too.
func openPlain(abs string, flag int, perm os.FileMode, dirOK bool) (*os.File, os.FileInfo, error) {
	if info, err := os.Stat(abs); err == nil {
		if err := checkPlain(info, dirOK); err != nil {
			return nil, nil, err
		}
	}
	f, err := os.OpenFile(abs, flag|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkPlain(info, dirOK)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// readPlain reads a whole regular file; special files are refused.
func readPlain(path string) ([]byte, error) {
	f, _, err := openPlain(path, os.O_RDONLY, 0, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// openFile opens path for a host operation and raises the Lua error when it cannot.
func (v *vm) openFile(L *lua.LState, fn, path string, flag int, perm os.FileMode, dirOK bool) (*os.File, os.FileInfo) {
	f, info, err := openPlain(v.resolve(path), flag, perm, dirOK)
	switch {
	case errors.Is(err, errIsDir):
		raise(L, fn, EFAIL, "%s is a directory", path)
	case errors.Is(err, errNotRegular):
		raise(L, fn, EINVAL, "%s: %v", path, err)
	case err != nil:
		v.fsErr(L, fn, path, err)
	}
	return f, info
}

func (v *vm) fsRead(L *lua.LState) int {
	path := L.CheckString(1)
	f, info := v.openFile(L, "fs.read", path, os.O_RDONLY, 0, false)
	defer f.Close()
	if info.Size() > maxReadBytes {
		raise(L, "fs.read", ELIMIT, "%s is %d bytes, over the %d MiB safety cap; use sh (head, sed -n) for parts", path, info.Size(), maxReadBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		v.fsErr(L, "fs.read", path, err)
	}
	if len(data) > maxReadBytes {
		raise(L, "fs.read", ELIMIT, "%s grew past the %d MiB safety cap while it was read", path, maxReadBytes>>20)
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
	f, _ := v.openFile(L, "fs.write", path, flags, mode, false)
	_, err := f.WriteString(content)
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
	text := v.readForEdit(L, "fs.edit", path)
	text, n := replaceOnce(L, "fs.edit", path, "", text, old, repl, all)
	v.writeEdited(L, "fs.edit", path, text)
	L.Push(lua.LNumber(n))
	return 1
}

// fsPatch applies a list of exact {old, new} edits to one file atomically:
// every edit must match, otherwise the file stays untouched.
func (v *vm) fsPatch(L *lua.LState) int {
	path := L.CheckString(1)
	edits := L.CheckTable(2)
	if edits.Len() == 0 {
		raise(L, "fs.patch", EINVAL, "no edits")
	}
	text := v.readForEdit(L, "fs.patch", path)
	total := 0
	for i := 1; i <= edits.Len(); i++ {
		e, ok := edits.RawGetInt(i).(*lua.LTable)
		if !ok {
			raise(L, "fs.patch", EINVAL, "edit #%d must be {old, new} or {old=, new=}", i)
		}
		old, okOld := optString(e, "old")
		repl, okNew := optString(e, "new")
		if !okOld {
			old, okOld = lua.LVAsString(e.RawGetInt(1)), e.RawGetInt(1).Type() == lua.LTString
		}
		if !okNew {
			repl, okNew = lua.LVAsString(e.RawGetInt(2)), e.RawGetInt(2).Type() == lua.LTString
		}
		if !okOld || !okNew {
			raise(L, "fs.patch", EINVAL, "edit #%d must be {old, new} or {old=, new=}", i)
		}
		var n int
		text, n = replaceOnce(L, "fs.patch", path, fmt.Sprintf("edit #%d: ", i), text, old, repl, optBool(e, "all"))
		total += n
	}
	v.writeEdited(L, "fs.patch", path, text)
	L.Push(lua.LNumber(total))
	return 1
}

// readForEdit reads the whole file an edit changes.
func (v *vm) readForEdit(L *lua.LState, fn, path string) string {
	f, _ := v.openFile(L, fn, path, os.O_RDONLY, 0, false)
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		v.fsErr(L, fn, path, err)
	}
	return string(data)
}

// writeEdited writes back a file that readForEdit read. It never creates the file:
// the path is opened for writing as an existing regular file.
func (v *vm) writeEdited(L *lua.LState, fn, path, text string) {
	f, _ := v.openFile(L, fn, path, os.O_WRONLY|os.O_TRUNC, 0, false)
	_, err := f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		v.fsErr(L, fn, path, err)
	}
}

// replaceOnce replaces old with repl in text; old must match exactly once
// unless all is set.
func replaceOnce(L *lua.LState, fn, path, prefix, text, old, repl string, all bool) (string, int) {
	if old == "" {
		raise(L, fn, EINVAL, "%sold text is empty", prefix)
	}
	n := strings.Count(text, old)
	switch {
	case n == 0:
		raise(L, fn, ENOMATCH, "%sno match for the old text in %s", prefix, path)
	case n > 1 && !all:
		raise(L, fn, EAMBIG, "%s%d matches in %s; add context or pass all=true", prefix, n, path)
	}
	return strings.ReplaceAll(text, old, repl), n
}

func (v *vm) fsList(L *lua.LState) int {
	path := L.CheckString(1)
	f, info := v.openFile(L, "fs.list", path, os.O_RDONLY, 0, true)
	defer f.Close()
	if !info.IsDir() {
		raise(L, "fs.list", EFAIL, "%s is not a directory", path)
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		v.fsErr(L, "fs.list", path, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
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
	if info, err := os.Stat(root); err == nil {
		if err := checkPlain(info, true); err != nil {
			raise(L, "fs.grep", EINVAL, "%s: %v", path, err)
		}
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
	f, _, err := openPlain(abs, os.O_RDONLY, 0, false)
	if err != nil {
		return rows
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
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
