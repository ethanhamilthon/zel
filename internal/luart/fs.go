package luart

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"zel/internal/edit"

	lua "github.com/yuin/gopher-lua"
)

const maxReadBytes = 16 << 20

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

func fileError(L *lua.LState, fn string, err error) {
	if errors.Is(err, errNotRegular) || errors.Is(err, errIsDir) {
		raise(L, fn, EINVAL, "%v", err)
	}
	raiseErr(L, fn, err)
}

func (v *vm) fsRead(L *lua.LState) int {
	p := v.resolve(textArg(L, "read", 1))
	f, info, err := openPlain(p, os.O_RDONLY, 0, false)
	if err != nil {
		fileError(L, "read", err)
	}
	defer f.Close()
	if info.Size() > maxReadBytes {
		raise(L, "read", ELIMIT, "%s exceeds 16 MiB", p)
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		if err := v.env.Attach(p); err != nil {
			raiseErr(L, "read", err)
		}
		L.Push(lua.LNil)
	default:
		data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if err != nil {
			fileError(L, "read", err)
		}
		if len(data) > maxReadBytes {
			raise(L, "read", ELIMIT, "%s exceeds 16 MiB", p)
		}
		L.Push(lua.LString(data))
	}
	return 1
}

func slice(L *lua.LState) int {
	s := textArg(L, "slice", 1)
	from := intArg(L, "slice", L.Get(2))
	if from == 0 {
		raise(L, "slice", EINVAL, "lines are 1-based")
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := from - 1
	if from < 0 {
		start = max(0, len(lines)+from)
	}
	start = min(start, len(lines))
	count := len(lines) - start
	if L.Get(3) != lua.LNil {
		n := intArg(L, "slice", L.Get(3))
		if n < 0 {
			raise(L, "slice", EINVAL, "count must not be negative")
		}
		count = min(n, count)
	}
	L.Push(lua.LString(strings.Join(lines[start:start+count], "")))
	return 1
}

func (v *vm) fsWrite(L *lua.LState) int {
	path := v.resolve(textArg(L, "write", 1))
	data := textArg(L, "write", 2)
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if boolOpt(L, "write", opts(L, "write", 3).RawGetString("append")) {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		fileError(L, "write", err)
	}
	f, _, err := openPlain(path, flags, 0644, false)
	if err != nil {
		fileError(L, "write", err)
	}
	_, err = f.WriteString(data)
	if err := errors.Join(err, f.Close()); err != nil {
		fileError(L, "write", err)
	}
	return 0
}

func (v *vm) fsEdit(L *lua.LState) int {
	const fn = "edit"
	path := v.resolve(textArg(L, fn, 1))
	var blocks []edit.Block
	if t, ok := L.Get(2).(*lua.LTable); ok {
		if !isArray(t) || t.Len() == 0 {
			raise(L, fn, EINVAL, "expected a nonempty list of edits")
		}
		for i := 1; i <= t.Len(); i++ {
			row := asTable(L, fn, t.RawGetInt(i))
			blocks = append(blocks, edit.Block{Old: asString(L, fn, row.RawGetInt(1)), New: asString(L, fn, row.RawGetInt(2)), All: boolOpt(L, fn, row.RawGetInt(3))})
		}
	} else {
		blocks = []edit.Block{{Old: textArg(L, fn, 2), New: textArg(L, fn, 3), All: boolOpt(L, fn, opts(L, fn, 4).RawGetString("all"))}}
	}
	for _, block := range blocks {
		if block.Old == "" {
			raise(L, fn, EINVAL, "old text is empty")
		}
	}
	f, info, err := openPlain(path, os.O_RDONLY, 0, false)
	if err != nil {
		fileError(L, fn, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	f.Close()
	if err != nil {
		fileError(L, fn, err)
	}
	if len(data) > maxReadBytes {
		raise(L, fn, ELIMIT, "%s exceeds 16 MiB", path)
	}
	text, n, err := edit.Apply(string(data), blocks, false)
	if err != nil {
		raiseErr(L, fn, err)
	}
	// Edit the target of a symlink, without replacing the symlink itself.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		fileError(L, fn, err)
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".zel-edit-*")
	if err != nil {
		fileError(L, fn, err)
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(info.Mode().Perm()); err == nil {
		_, err = temp.WriteString(text)
	}
	if err = errors.Join(err, temp.Close()); err == nil {
		err = os.Rename(temp.Name(), target)
	}
	if err != nil {
		fileError(L, fn, err)
	}
	L.Push(lua.LNumber(n))
	return 1
}

func (v *vm) fsList(L *lua.LState) int {
	dir := v.env.Info().Cwd
	if L.Get(1) != lua.LNil {
		dir = v.resolve(textArg(L, "list", 1))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fileError(L, "list", err)
	}
	out := L.CreateTable(len(entries), 0)
	for _, e := range entries {
		var size int64
		if i, err := e.Info(); err == nil {
			size = i.Size()
		}
		out.Append(toLua(L, map[string]any{"name": e.Name(), "dir": e.IsDir(), "size": size}))
	}
	L.Push(out)
	return 1
}

func (v *vm) fsExists(L *lua.LState) int {
	info, err := os.Stat(v.resolve(textArg(L, "exists", 1)))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			raiseErr(L, "exists", err)
		}
		L.Push(lua.LFalse)
		L.Push(lua.LNil)
	} else {
		kind := "file"
		if info.IsDir() {
			kind = "dir"
		}
		L.Push(lua.LTrue)
		L.Push(lua.LString(kind))
	}
	return 2
}
