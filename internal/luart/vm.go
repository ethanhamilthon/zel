package luart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// maxOutput is the safety cap of one call's output. The model is expected to
// filter and slice in Lua; the cap only protects memory and the context.
const maxOutput = 1 << 20

// callTimeout is the hard, non-configurable deadline of every Lua tool call,
// including the blocking host calls made from it. It is a variable only so
// tests can shorten it.
var callTimeout = 30 * time.Second

// role is what a Lua state is for. Every role gets its own state; nothing is
// shared between them except through state.* and the host.
type role int

const (
	roleConfig role = iota // init.lua: hooks, zel.prompt and plugin function bodies
	roleMain               // the session's tool calls; globals survive between calls
	roleTask               // one tasks.spawn(fn) on its own goroutine
)

// vm is one Lua state with the host API installed.
type vm struct {
	L    *lua.LState
	env  Env
	cfg  *Config
	ctx  context.Context
	out  io.Writer
	buf  strings.Builder
	role role
}

func newVM(env Env, cfg *Config, r role) *vm {
	v := &vm{L: lua.NewState(), env: env, cfg: cfg, ctx: context.Background(), role: r}
	v.out = &v.buf
	v.installHost()
	if cfg != nil {
		cfg.installFns(v)
	}
	return v
}

func (v *vm) close() { v.L.Close() }

// agentsAllowed reports whether agents.* works here: subagents cannot start
// subagents of their own.
func (v *vm) agentsAllowed() bool { return v.env.Info().Depth == 0 }

// ctxErr raises the Lua error that matches why ctx ended.
func (v *vm) ctxErr(fn string) {
	err := v.ctx.Err()
	if err == nil {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		raise(v.L, fn, ETIMEOUT, "the call exceeded its timeout")
	}
	raise(v.L, fn, ECANCELED, "the call was cancelled")
}

// Result is what the model sees after a call.
type Result struct {
	Output  string
	IsError bool
}

// Runner executes the tool calls of one session in its main VM. The VM lives
// across calls, so globals and functions defined by one call are there in the
// next. It is created on first use and thrown away by Reset or a timeout; the
// next call then starts clean and its output says so once.
type Runner struct {
	Env Env
	Cfg *Config // optional: plugins and hooks

	mu   sync.Mutex
	v    *vm
	cfg  *Config // the config the current VM was built with
	note string  // why the VM was reset; reported with the next call
}

// Reset drops the main VM. A non-empty reason is reported to the model with
// the next call's output.
func (r *Runner) Reset(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.v != nil {
		r.v.close()
		r.v = nil
	}
	if reason != "" {
		r.note = reason
	}
}

// Close frees the main VM.
func (r *Runner) Close() { r.Reset("") }

// Run executes one tool call: raw Lua code.
func (r *Runner) Run(ctx context.Context, code string) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.v != nil && r.cfg != r.Cfg {
		r.v.close()
		r.v = nil
		r.note = "init.lua was reloaded"
	}
	if r.v == nil {
		r.v, r.cfg = newVM(r.Env, r.Cfg, roleMain), r.Cfg
	}
	note := r.note
	r.note = ""
	res, timedOut := r.run(ctx, code)
	if timedOut {
		r.v.close()
		r.v = nil
		r.note = "the previous call hit the " + callTimeout.String() + " timeout"
	}
	if note != "" {
		res.Output = "[lua: fresh VM, " + note + ": globals and functions from earlier calls are gone]\n" + res.Output
	}
	return res
}

func (r *Runner) run(parent context.Context, code string) (Result, bool) {
	ctx, cancel := context.WithTimeout(parent, callTimeout)
	defer cancel()
	v := r.v
	v.ctx = ctx
	v.buf.Reset()
	v.installSession()
	v.L.SetContext(ctx)
	defer func() {
		v.L.RemoveContext()
		v.L.SetTop(0)
		v.ctx = context.Background()
	}()
	timedOut := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil }

	fn, err := v.L.Load(strings.NewReader(code), "lua")
	if err != nil {
		return Result{Output: "error: " + err.Error(), IsError: true}, false
	}
	v.L.Push(fn)
	err = v.L.PCall(0, lua.MultRet, nil)
	if err != nil {
		msg := errorMessage(err)
		switch {
		case timedOut():
			msg = fmt.Sprintf("lua: %s: the call exceeded its fixed timeout of %s; use tasks.spawn for long work", ETIMEOUT, callTimeout)
		case parent.Err() != nil:
			msg = "lua: " + ECANCELED + ": the call was cancelled"
		}
		text := v.buf.String() + "error: " + msg
		if trace := traceback(err); trace != "" {
			text += "\n" + trace
		}
		return Result{Output: truncate(text, maxOutput), IsError: true}, timedOut()
	}
	var parts []string
	if s := strings.TrimRight(v.buf.String(), "\n"); s != "" {
		parts = append(parts, s)
	}
	for i := 1; i <= v.L.GetTop(); i++ {
		parts = append(parts, renderValue(v.L.Get(i)))
	}
	return Result{Output: truncate(strings.Join(parts, "\n"), maxOutput)}, false
}

func (v *vm) print(L *lua.LState) int {
	var line strings.Builder
	for i := 1; i <= L.GetTop(); i++ {
		if i > 1 {
			line.WriteByte('\t')
		}
		line.WriteString(luaString(L, L.Get(i)))
	}
	line.WriteByte('\n')
	_, _ = io.WriteString(v.out, line.String())
	return 0
}

// luaString is tostring() without calling metamethods that may fail.
func luaString(L *lua.LState, v lua.LValue) string {
	if t, ok := v.(*lua.LTable); ok {
		if g, err := toGo(t); err == nil {
			data, _ := json.Marshal(g)
			return string(data)
		}
	}
	return L.ToStringMeta(v).String()
}

func renderValue(v lua.LValue) string {
	switch x := v.(type) {
	case lua.LString:
		return string(x)
	case *lua.LNilType:
		return "nil"
	case *lua.LTable:
		g, err := toGo(x)
		if err == nil {
			data, _ := json.Marshal(g)
			return string(data)
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

func traceback(err error) string {
	var api *lua.ApiError
	if !errors.As(err, &api) || api.StackTrace == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(api.StackTrace), "\n")
	if len(lines) > 7 {
		lines = append(lines[:6], "\t...")
	}
	if len(lines) > 0 && strings.HasPrefix(lines[0], "stack traceback") {
		return strings.Join(lines, "\n")
	}
	return "stack traceback:\n" + strings.Join(lines, "\n")
}

// truncate keeps the head and the tail of s within limit bytes.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	half := limit / 2
	head, tail := strings.ToValidUTF8(s[:half], ""), strings.ToValidUTF8(s[len(s)-(limit-half):], "")
	return head + fmt.Sprintf("\n[output truncated: %d of %d bytes omitted (safety cap %d bytes); filter or slice in Lua]\n", len(s)-len(head)-len(tail), len(s), limit) + tail
}

// resolve makes p absolute against the session directory and expands "~/".
func (v *vm) resolve(p string) string {
	info := v.env.Info()
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(info.Cwd, p)
}
