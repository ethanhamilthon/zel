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
)

const maxOutput = 1 << 20

type vm struct {
	L          *lua.LState
	ctx        context.Context
	out        io.Writer
	env        Env
	cfg        *Config
	api        *lua.LTable
	loadingEnv func() map[string]string
}

func newVM(env Env, cfg *Config, globals bool) *vm {
	v := &vm{L: lua.NewState(), env: env, cfg: cfg, out: io.Discard}
	v.install(globals)
	v.installCoroutine()
	return v
}

func (v *vm) use(ctx context.Context, out io.Writer) func() {
	old := v.out
	v.ctx, v.out = ctx, out
	v.L.SetContext(ctx)
	return func() {
		v.L.SetTop(0)
		v.L.RemoveContext()
		v.ctx, v.out = nil, old
	}
}

func (v *vm) print(L *lua.LState) int {
	for i := 1; i <= L.GetTop(); i++ {
		if i > 1 {
			io.WriteString(v.out, "\t")
		}
		io.WriteString(v.out, renderValue(L.Get(i)))
	}
	io.WriteString(v.out, "\n")
	return 0
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

// Results start on a new line after print output, without an extra blank line.
func (v *vm) results(out io.Writer) {
	for i := 1; i <= v.L.GetTop(); i++ {
		if i > 1 {
			io.WriteString(out, "\n")
		}
		io.WriteString(out, renderValue(v.L.Get(i)))
	}
}

type Result struct {
	Out                 string
	OK, TimedOut, Reset bool
	Ms                  int64
}

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
		a.v.L.Close()
		a.v = nil
	}
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
		a.v = newVM(a.env, a.cfg, true)
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
			go func() { <-done; v.L.Close() }()
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
	defer v.use(ctx, out)()
	fn, err := v.L.Load(strings.NewReader(code), "lua")
	if err != nil {
		return err
	}
	v.L.Push(fn)
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
