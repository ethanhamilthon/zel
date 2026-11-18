package luart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

const maxOutput = 1 << 20

// vm is one Lua state: the standard library (hardened, see stdlib.go) and
// whatever the caller puts in it. It has no host API.
type vm struct {
	L           *lua.LState
	ctx         context.Context
	env         Env
	procs       *procs
	settingsEnv func() map[string]string // settings.env, added to the children's environment
}

// newVM makes a VM with the standard library. path is the package.path.
func newVM(env Env, path string) *vm {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenPackage}, {lua.BaseLibName, lua.OpenBase}, {lua.TabLibName, lua.OpenTable},
		{lua.OsLibName, lua.OpenOs}, {lua.StringLibName, lua.OpenString}, {lua.MathLibName, lua.OpenMath},
		{lua.DebugLibName, lua.OpenDebug}, {lua.ChannelLibName, lua.OpenChannel}, {lua.CoroutineLibName, lua.OpenCoroutine},
	} {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	v := &vm{L: L, env: env, procs: newProcs()}
	v.installStdlib()
	v.installCoroutine()
	if path != "" {
		L.GetGlobal("package").(*lua.LTable).RawSetString("path", lua.LString(path))
	}
	return v
}

// close ends the VM: its children are killed.
func (v *vm) close() {
	v.procs.close()
	v.L.Close()
}

// logPrint gives print to the VMs that have no user to talk to (config, hooks,
// commands): its text goes to the Tracing log, never to the terminal.
func (v *vm) logPrint() {
	v.L.SetGlobal("print", v.L.NewFunction(func(L *lua.LState) int {
		parts := make([]string, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, renderValue(L.Get(i)))
		}
		v.env.Log("print", strings.Join(parts, "\t"))
		return 0
	}))
}

// use binds the VM to ctx for one call. Cancelling ctx kills the children of
// io.popen and os.execute, so a blocked read returns.
func (v *vm) use(ctx context.Context) func() {
	v.ctx = ctx
	v.L.SetContext(ctx)
	stop := context.AfterFunc(ctx, v.procs.killAll)
	return func() {
		stop()
		v.L.SetTop(0)
		v.L.RemoveContext()
		v.ctx = nil
	}
}

func renderValue(v lua.LValue) string {
	if t, ok := v.(*lua.LTable); ok {
		if g, err := toGo(t); err == nil {
			if b, err := json.Marshal(g); err == nil {
				return string(b)
			}
		}
	}
	return v.String()
}

func errorMessage(err error) string {
	var api *lua.ApiError
	if errors.As(err, &api) && api.Object != nil {
		return api.Object.String()
	}
	return err.Error()
}

// compile is the result rule of the lua tool: the values of `return ...`; when
// the last statement is a call, its values (it becomes a return); when the
// whole code is an expression, its value.
func compile(code string) (*lua.FunctionProto, error) {
	chunk, err := parse.Parse(strings.NewReader(code), "lua")
	if err != nil {
		expr, err2 := parse.Parse(strings.NewReader("return "+code), "lua")
		if err2 != nil {
			return nil, err
		}
		chunk = expr
	} else if n := len(chunk); n > 0 {
		if call, ok := chunk[n-1].(*ast.FuncCallStmt); ok {
			ret := &ast.ReturnStmt{Exprs: []ast.Expr{call.Expr}}
			ret.SetLine(call.Line())
			ret.SetLastLine(call.LastLine())
			chunk[n-1] = ret
		}
	}
	return lua.Compile(chunk, "lua")
}

// results writes the values on the stack, one per line, without nils.
func (v *vm) results(out io.Writer) {
	first := true
	for i := 1; i <= v.L.GetTop(); i++ {
		val := v.L.Get(i)
		if val == lua.LNil {
			continue
		}
		if !first {
			io.WriteString(out, "\n")
		}
		first = false
		io.WriteString(out, renderValue(val))
	}
}

type Result struct {
	Out                 string
	OK, TimedOut, Reset bool
	Ms                  int64
}

// Agent is the session VM of the lua tool. It is created on the first call
// with the functions of the config copied from the config VM.
type Agent struct {
	mu            sync.Mutex
	env           Env
	cfg           *Config
	v             *vm
	fresh, closed bool
}

func NewAgent(env Env, cfg *Config) *Agent { return &Agent{env: env, cfg: cfg} }

func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drop()
	a.fresh = true
}

func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drop()
	a.closed = true
}

func (a *Agent) drop() {
	if a.v != nil {
		a.v.close()
		a.v = nil
	}
}

// newVM builds the session VM: the standard library and the functions of the
// config as globals, copied in one pass so shared upvalues stay shared.
func (a *Agent) newVM(ctx context.Context) (*vm, error) {
	if a.cfg == nil {
		return newVM(a.env, ""), nil
	}
	v := newVM(a.env, a.cfg.pkgPath)
	v.settingsEnv = a.cfg.settingsEnv
	if err := a.cfg.copyFunctions(ctx, v); err != nil {
		v.close()
		return nil, err
	}
	return v, nil
}

// A native Lua library call may not observe cancellation. Detach its state
// on timeout and let its goroutine close it if it eventually returns.
func (a *Agent) Run(parent context.Context, code string, timeout time.Duration) Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return Result{Out: "error: lua: ECANCELED: agent is closed"}
	}
	start := time.Now()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if a.v == nil {
		v, err := a.newVM(ctx)
		if err != nil {
			return Result{Out: "error: lua: EFAIL: " + err.Error(), Ms: time.Since(start).Milliseconds()}
		}
		a.v = v
	}
	v := a.v
	out := newOutput()
	if a.fresh {
		io.WriteString(out, "[lua: fresh VM]\n")
		a.fresh = false
	}
	done := make(chan error, 1)
	go func() { done <- v.exec(ctx, code, out) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case err = <-done:
		case <-timer.C:
			a.v = nil
			go func() { <-done; v.close() }()
		}
	}
	ctxErr := ctx.Err()
	r := Result{OK: err == nil && ctxErr == nil, Ms: time.Since(start).Milliseconds()}
	var suffix string
	switch ctxErr {
	case context.DeadlineExceeded:
		r.TimedOut, r.Reset = true, true
		a.drop()
		a.fresh = true
		suffix = fmt.Sprintf("[timeout after %s; VM reset]", timeout)
	case context.Canceled:
		suffix = "error: lua: ECANCELED: call cancelled"
		if a.v == nil {
			a.fresh, r.Reset = true, true
		}
	default:
		if err != nil {
			suffix = "error: " + errorMessage(err)
		}
	}
	r.Out = out.finish(suffix)
	return r
}

func (v *vm) exec(ctx context.Context, code string, out io.Writer) error {
	defer v.use(ctx)()
	proto, err := compile(code)
	if err != nil {
		return err
	}
	v.L.Push(v.L.NewFunctionFromProto(proto))
	if err := v.L.PCall(0, lua.MultRet, nil); err != nil {
		return err
	}
	if ctx.Err() == nil {
		v.results(out)
	}
	return nil
}

// gopher-lua threads inherit the context in effect at creation time. Rebind
// when resuming so a suspended coroutine can continue on a later call.
func (v *vm) installCoroutine() {
	co := v.L.GetGlobal("coroutine").(*lua.LTable)
	create, resume := co.RawGetString("create"), co.RawGetString("resume")
	call := func(L *lua.LState, fn lua.LValue) int {
		n := L.GetTop()
		L.Insert(fn, 1)
		L.Call(n, lua.MultRet)
		return L.GetTop()
	}
	resumeCurrent := v.L.NewFunction(func(L *lua.LState) int {
		L.CheckThread(1).SetContext(v.ctx)
		return call(L, resume)
	})
	co.RawSetString("resume", resumeCurrent)
	co.RawSetString("wrap", v.L.NewFunction(func(L *lua.LState) int {
		L.SetTop(1)
		call(L, create)
		thread := L.Get(1)
		L.Push(L.NewFunction(func(L *lua.LState) int {
			L.Insert(thread, 1)
			call(L, resumeCurrent)
			if L.Get(1) == lua.LFalse {
				L.Error(L.Get(2), 0)
			}
			L.Remove(1)
			return L.GetTop()
		}))
		return 1
	}))
}
