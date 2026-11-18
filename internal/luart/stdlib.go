package luart

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	lua "github.com/yuin/gopher-lua"
	"zel/internal/shell"
)

// The VMs use the Lua standard library with two changes, both in Go:
//   - nothing reaches Zel's terminal: io.write, io.stdout and io.stderr go to a
//     discarded sink, io.stdin is empty, os.exit is gone;
//   - io.popen and os.execute run their commands hardened: no TTY (stdin
//     /dev/null, stderr /dev/null, os.execute stdout /dev/null), in their own
//     process group, in the session start folder with the session environment,
//     and the whole group is killed when the VM's call is cancelled or the VM ends.

const fileClass = "FILE*"

// file is the FILE* of the VMs: a regular file, a pipe to a child, or a sink.
type file struct {
	mu     sync.Mutex
	name   string
	f      *os.File
	r      *bufio.Reader
	w      io.Writer
	bw     *bufio.Writer
	proc   *child
	closed bool
	std    bool // io.stdin/stdout/stderr: close is refused
}

// child is a process started by io.popen: Wait is done by its own goroutine.
type child struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// procs are the live children of one VM.
type procs struct {
	mu   sync.Mutex
	life context.Context
	stop context.CancelFunc
	live map[*child]struct{}
}

func newProcs() *procs {
	life, stop := context.WithCancel(context.Background())
	return &procs{life: life, stop: stop, live: map[*child]struct{}{}}
}

// killAll kills the process groups of every live child.
func (p *procs) killAll() {
	p.mu.Lock()
	for c := range p.live {
		c.cancel()
	}
	p.mu.Unlock()
}

// close kills the children for good: the VM ends.
func (p *procs) close() {
	p.stop()
	p.killAll()
}

// start launches script in the shell. in/out select the pipes: a pipe end is
// returned for each requested direction; the others are /dev/null.
func (v *vm) start(script string, wantIn, wantOut bool) (*child, io.WriteCloser, io.ReadCloser, error) {
	p := v.procs
	if err := p.life.Err(); err != nil {
		return nil, nil, nil, errors.New("the VM has ended")
	}
	ctx, cancel := context.WithCancel(p.life)
	cmd := shell.Command(ctx, v.env.Info().Cwd, v.childEnv(), script)
	var closeAfter []*os.File
	var inW, outR *os.File
	fail := func(err error) (*child, io.WriteCloser, io.ReadCloser, error) {
		cancel()
		for _, f := range closeAfter {
			f.Close()
		}
		if inW != nil {
			inW.Close()
		}
		if outR != nil {
			outR.Close()
		}
		return nil, nil, nil, err
	}
	if wantIn {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		cmd.Stdin, inW = r, w
		closeAfter = append(closeAfter, r)
	}
	if wantOut {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		cmd.Stdout, outR = w, r
		closeAfter = append(closeAfter, w)
	}
	// Left nil, Stdin/Stdout/Stderr of the child are /dev/null.
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	for _, f := range closeAfter {
		f.Close()
	}
	c := &child{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	p.mu.Lock()
	p.live[c] = struct{}{}
	p.mu.Unlock()
	go func() {
		c.err = shell.Reap(cmd)
		cancel()
		p.mu.Lock()
		delete(p.live, c)
		p.mu.Unlock()
		close(c.done)
	}()
	var w io.WriteCloser
	var r io.ReadCloser
	if inW != nil {
		w = inW
	}
	if outR != nil {
		r = outR
	}
	return c, w, r, nil
}

// childEnv is the environment of the children: the session's, then settings.env.
func (v *vm) childEnv() map[string]string {
	env := map[string]string{}
	for _, entry := range v.env.ShellEnv() {
		if k, val, ok := strings.Cut(entry, "="); ok {
			env[k] = val
		}
	}
	if v.settingsEnv != nil {
		for k, val := range v.settingsEnv() {
			env[k] = val
		}
	}
	return env
}

// status is the result of a finished child as Lua 5.2 io.close reports it:
// true|nil, "exit"|"signal", code.
func (c *child) status() (bool, string, int) {
	var exit *exec.ExitError
	switch {
	case c.err == nil:
		return true, "exit", 0
	case errors.As(c.err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return false, "signal", int(ws.Signal())
		}
		return false, "exit", exit.ExitCode()
	}
	return false, "exit", 1
}

func (v *vm) installStdlib() {
	L := v.L
	L.SetGlobal("print", lua.LNil)
	if osLib, ok := L.GetGlobal("os").(*lua.LTable); ok {
		osLib.RawSetString("exit", lua.LNil)
		// os.getenv sees the session environment (ZEL_MODE, settings.env), then Zel's own.
		osLib.RawSetString("getenv", L.NewFunction(func(L *lua.LState) int {
			name := L.CheckString(1)
			if val, ok := v.childEnv()[name]; ok {
				L.Push(lua.LString(val))
			} else if val, ok := os.LookupEnv(name); ok {
				L.Push(lua.LString(val))
			} else {
				L.Push(lua.LNil)
			}
			return 1
		}))
		osLib.RawSetString("execute", L.NewFunction(v.osExecute))
	}
	mt := L.NewTypeMetatable(fileClass)
	methods := L.NewTable()
	L.SetFuncs(methods, map[string]lua.LGFunction{
		"read": fileRead, "lines": fileLines, "write": fileWrite, "close": fileClose,
		"flush": fileFlush, "seek": fileSeek, "setvbuf": fileSetvbuf,
	})
	mt.RawSetString("__index", methods)
	mt.RawSetString("__tostring", L.NewFunction(func(L *lua.LState) int {
		f := checkFile(L, 1)
		if f.closed {
			L.Push(lua.LString("file (closed)"))
		} else {
			L.Push(lua.LString("file (" + f.name + ")"))
		}
		return 1
	}))
	stdin := v.newFile(&file{name: "stdin", r: bufio.NewReader(strings.NewReader("")), std: true})
	stdout := v.newFile(&file{name: "stdout", w: io.Discard, std: true})
	stderr := v.newFile(&file{name: "stderr", w: io.Discard, std: true})
	state := &ioState{v: v, in: stdin, out: stdout}
	ioLib := L.NewTable()
	L.SetFuncs(ioLib, map[string]lua.LGFunction{
		"open": v.ioOpen, "popen": v.ioPopen, "close": state.close, "type": ioType,
		"read": state.read, "write": state.write, "lines": state.lines,
		"input": state.input, "output": state.output, "flush": state.flush, "tmpfile": v.ioTmpfile,
	})
	ioLib.RawSetString("stdin", stdin)
	ioLib.RawSetString("stdout", stdout)
	ioLib.RawSetString("stderr", stderr)
	L.SetGlobal("io", ioLib)
	if loaded, ok := L.GetField(L.GetGlobal("package"), "loaded").(*lua.LTable); ok {
		loaded.RawSetString("io", ioLib)
	}
}

func (v *vm) newFile(f *file) *lua.LUserData {
	ud := v.L.NewUserData()
	ud.Value = f
	v.L.SetMetatable(ud, v.L.GetTypeMetatable(fileClass))
	return ud
}

func checkFile(L *lua.LState, n int) *file {
	if ud, ok := L.Get(n).(*lua.LUserData); ok {
		if f, ok := ud.Value.(*file); ok {
			return f
		}
	}
	L.ArgError(n, "file expected")
	return nil
}

func openFile(L *lua.LState, n int) *file {
	f := checkFile(L, n)
	if f.closed {
		L.ArgError(n, "file is closed")
	}
	return f
}

func pushErr(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	code := 1
	var errno syscall.Errno
	if errors.As(err, &errno) {
		code = int(errno)
	}
	L.Push(lua.LNumber(code))
	return 3
}

type ioState struct {
	v       *vm
	in, out *lua.LUserData
}

// resolve makes a relative path relative to the session start folder.
func (v *vm) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(v.env.Info().Cwd, path)
}

func ioType(L *lua.LState) int {
	L.CheckAny(1)
	if ud, ok := L.Get(1).(*lua.LUserData); ok {
		if f, ok := ud.Value.(*file); ok {
			if f.closed {
				L.Push(lua.LString("closed file"))
			} else {
				L.Push(lua.LString("file"))
			}
			return 1
		}
	}
	L.Push(lua.LNil)
	return 1
}

func (v *vm) ioOpen(L *lua.LState) int {
	path, mode := L.CheckString(1), L.OptString(2, "r")
	mode = strings.ReplaceAll(mode, "b", "")
	flags := map[string]int{
		"r": os.O_RDONLY, "r+": os.O_RDWR, "w": os.O_WRONLY | os.O_CREATE | os.O_TRUNC,
		"w+": os.O_RDWR | os.O_CREATE | os.O_TRUNC, "a": os.O_WRONLY | os.O_CREATE | os.O_APPEND,
		"a+": os.O_RDWR | os.O_CREATE | os.O_APPEND,
	}
	flag, ok := flags[mode]
	if !ok {
		L.ArgError(2, "invalid mode")
	}
	path = v.resolve(path)
	f, err := os.OpenFile(path, flag, 0666)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(path + ": " + errText(err)))
		_, code := errnoOf(err)
		L.Push(lua.LNumber(code))
		return 3
	}
	L.Push(v.newFile(fromOS(f, path, mode)))
	return 1
}

func errnoOf(err error) (string, int) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error(), int(errno)
	}
	return err.Error(), 1
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	text, _ := errnoOf(err)
	return text
}

func fromOS(f *os.File, name, mode string) *file {
	out := &file{name: name, f: f}
	if mode == "r" || strings.Contains(mode, "+") {
		out.r = bufio.NewReader(f)
	}
	if mode != "r" {
		out.bw = bufio.NewWriter(f)
		out.w = out.bw
	}
	return out
}

func (v *vm) ioTmpfile(L *lua.LState) int {
	f, err := os.CreateTemp("", "zel-lua-")
	if err != nil {
		return pushErr(L, err)
	}
	os.Remove(f.Name())
	L.Push(v.newFile(fromOS(f, f.Name(), "w+")))
	return 1
}

func (v *vm) ioPopen(L *lua.LState) int {
	script, mode := L.CheckString(1), L.OptString(2, "r")
	if mode != "r" && mode != "w" {
		L.ArgError(2, "invalid mode")
	}
	c, w, r, err := v.start(script, mode == "w", mode == "r")
	if err != nil {
		return pushErr(L, err)
	}
	f := &file{name: "process", proc: c}
	if r != nil {
		f.f = r.(*os.File)
		f.r = bufio.NewReader(r)
	}
	if w != nil {
		f.f = w.(*os.File)
		f.w = w
	}
	L.Push(v.newFile(f))
	return 1
}

func (v *vm) osExecute(L *lua.LState) int {
	if L.GetTop() == 0 || L.Get(1) == lua.LNil {
		L.Push(lua.LNumber(1)) // a shell is available
		return 1
	}
	c, _, _, err := v.start(L.CheckString(1), false, false)
	if err != nil {
		L.Push(lua.LNumber(127))
		return 1
	}
	<-c.done
	_, how, code := c.status()
	if how == "signal" {
		code += 128
	}
	L.Push(lua.LNumber(code))
	return 1
}

func (s *ioState) input(L *lua.LState) int  { return setDefault(L, &s.in, "r") }
func (s *ioState) output(L *lua.LState) int { return setDefault(L, &s.out, "w") }

func setDefault(L *lua.LState, slot **lua.LUserData, mode string) int {
	switch arg := L.Get(1).(type) {
	case *lua.LNilType:
	case lua.LString:
		L.ArgError(1, "io.input/io.output of a path is not supported; use io.open")
	case *lua.LUserData:
		checkFile(L, 1)
		*slot = arg
	}
	L.Push(*slot)
	return 1
}

func (s *ioState) read(L *lua.LState) int {
	return readFormats(L, s.in.Value.(*file), 1)
}
func (s *ioState) write(L *lua.LState) int {
	return writeValues(L, s.out, s.out.Value.(*file), 1)
}
func (s *ioState) flush(L *lua.LState) int { L.Push(s.out); return 1 }
func (s *ioState) close(L *lua.LState) int {
	if L.GetTop() == 0 {
		L.Push(s.out)
	}
	return fileClose(L)
}
func (s *ioState) lines(L *lua.LState) int {
	if L.GetTop() == 0 || L.Get(1) == lua.LNil {
		return linesIter(L, s.in.Value.(*file))
	}
	path := L.CheckString(1)
	f, err := os.Open(s.v.resolve(path))
	if err != nil {
		L.RaiseError("%s: %s", path, errText(err))
	}
	return linesIter(L, fromOS(f, path, "r"))
}

func fileRead(L *lua.LState) int { return readFormats(L, openFile(L, 1), 2) }

func fileLines(L *lua.LState) int {
	return linesIter(L, openFile(L, 1))
}

func linesIter(L *lua.LState, f *file) int {
	var formats []lua.LValue
	for i := 2; i <= L.GetTop(); i++ {
		formats = append(formats, L.Get(i))
	}
	L.Push(L.NewFunction(func(L *lua.LState) int {
		if f.closed {
			L.RaiseError("file is already closed")
		}
		base := L.GetTop()
		for _, fm := range formats {
			L.Push(fm)
		}
		if len(formats) == 0 {
			L.Push(lua.LString("*l"))
		}
		n := readFormats(L, f, base+1)
		if n > 0 && L.Get(-n) == lua.LNil {
			return 1
		}
		return n
	}))
	return 1
}

// readFormats reads by the formats at idx..top: "*n", "*l", "*L", "*a" or a count.
func readFormats(L *lua.LState, f *file, idx int) int {
	if f.r == nil {
		return pushErr(L, errors.New("file not readable"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bw != nil {
		f.bw.Flush()
	}
	top := L.GetTop()
	if top < idx {
		L.Push(lua.LString("*l"))
		top++
	}
	n := 0
	for i := idx; i <= top; i++ {
		var val lua.LValue = lua.LNil
		switch fm := L.Get(i).(type) {
		case lua.LNumber:
			val = readCount(f.r, int(fm))
		case lua.LString:
			switch strings.TrimPrefix(string(fm), "*") {
			case "a":
				data, _ := io.ReadAll(f.r)
				val = lua.LString(data)
			case "l", "L":
				line, err := f.r.ReadString('\n')
				if err != nil && line == "" {
					break
				}
				if strings.HasPrefix(string(fm), "*L") || string(fm) == "L" {
					val = lua.LString(line)
				} else {
					val = lua.LString(strings.TrimSuffix(line, "\n"))
				}
			case "n":
				var num float64
				if _, err := fmt.Fscan(f.r, &num); err == nil {
					val = lua.LNumber(num)
				}
			default:
				L.ArgError(i, "invalid format")
			}
		default:
			L.ArgError(i, "invalid format")
		}
		L.Push(val)
		n++
		if val == lua.LNil {
			break
		}
	}
	return n
}

func readCount(r *bufio.Reader, n int) lua.LValue {
	if n == 0 {
		if _, err := r.Peek(1); err != nil {
			return lua.LNil
		}
		return lua.LString("")
	}
	buf := make([]byte, n)
	got, _ := io.ReadFull(r, buf)
	if got == 0 {
		return lua.LNil
	}
	return lua.LString(buf[:got])
}

func fileWrite(L *lua.LState) int {
	f := openFile(L, 1)
	return writeValues(L, L.Get(1), f, 2)
}

func writeValues(L *lua.LState, self lua.LValue, f *file, idx int) int {
	if f.closed {
		L.RaiseError("file is closed")
	}
	if f.w == nil {
		return pushErr(L, errors.New("file not writable"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := idx; i <= L.GetTop(); i++ {
		var s string
		switch x := L.Get(i).(type) {
		case lua.LString:
			s = string(x)
		case lua.LNumber:
			s = x.String()
		default:
			L.ArgError(i, "string expected, got "+L.Get(i).Type().String())
		}
		if f.r != nil && f.f != nil && f.bw != nil {
			// A read-write file: drop read-ahead so the write lands at the logical position.
			if n := f.r.Buffered(); n > 0 {
				f.f.Seek(-int64(n), io.SeekCurrent)
				f.r.Reset(f.f)
			}
		}
		if _, err := io.WriteString(f.w, s); err != nil {
			return pushErr(L, err)
		}
	}
	L.Push(self)
	return 1
}

func fileFlush(L *lua.LState) int {
	f := openFile(L, 1)
	if f.bw != nil {
		if err := f.bw.Flush(); err != nil {
			return pushErr(L, err)
		}
	}
	L.Push(L.Get(1))
	return 1
}

func fileSetvbuf(L *lua.LState) int { openFile(L, 1); L.Push(lua.LTrue); return 1 }

func fileSeek(L *lua.LState) int {
	f := openFile(L, 1)
	if f.f == nil || f.proc != nil {
		return pushErr(L, errors.New("cannot seek this file"))
	}
	whence := map[string]int{"set": io.SeekStart, "cur": io.SeekCurrent, "end": io.SeekEnd}[L.OptString(2, "cur")]
	off := int64(L.OptInt(3, 0))
	if f.bw != nil {
		f.bw.Flush()
	}
	if f.r != nil && whence == io.SeekCurrent {
		off -= int64(f.r.Buffered())
	}
	pos, err := f.f.Seek(off, whence)
	if err != nil {
		return pushErr(L, err)
	}
	if f.r != nil {
		f.r.Reset(f.f)
	}
	L.Push(lua.LNumber(pos))
	return 1
}

func fileClose(L *lua.LState) int {
	f := openFile(L, 1)
	if f.std {
		return pushErr(L, errors.New("cannot close a standard file"))
	}
	f.closed = true
	var err error
	if f.bw != nil {
		err = f.bw.Flush()
	}
	if f.f != nil {
		err = errors.Join(err, f.f.Close())
	}
	if f.proc != nil {
		<-f.proc.done
		ok, how, code := f.proc.status()
		if ok {
			L.Push(lua.LTrue)
		} else {
			L.Push(lua.LNil)
		}
		L.Push(lua.LString(how))
		L.Push(lua.LNumber(code))
		return 3
	}
	if err != nil {
		return pushErr(L, err)
	}
	L.Push(lua.LTrue)
	return 1
}
