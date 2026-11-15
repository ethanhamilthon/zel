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

	// Guarded by mu; used by main VMs only (see Runner).
	mu      sync.Mutex
	busy    bool // a goroutine runs Lua in L
	retired bool // L is freed once nothing runs in it
	freed   bool // L is closed
}

func newVM(env Env, cfg *Config, r role) *vm {
	v := &vm{L: lua.NewState(), env: env, cfg: cfg, ctx: context.Background(), role: r}
	v.out = &v.buf
	v.installHost()
	if cfg != nil {
		cfg.installTools(v)
	}
	return v
}

// close frees the Lua state of a VM that no goroutine runs in: the caller holds
// the config lock, or the VM was never used.
func (v *vm) close() {
	v.mu.Lock()
	v.freed = true
	v.mu.Unlock()
	v.L.Close()
}

// agentsAllowed reports whether new_agent{} works here: subagents cannot start
// subagents of their own.
func (v *vm) agentsAllowed() bool { return v.env.Info().Mode != "agent" }

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
// next. It is created on first use and dropped by Reset, a timeout or a call that
// did not return; the next call then starts clean and its output says so once.
//
// The Lua of a call runs on its own goroutine. Run returns when that Lua ends, or
// when the call's deadline or the caller's cancellation comes first, even if a host
// operation in it never returns. Such a VM is dropped at once and freed when its
// Lua returns, so neither a stuck call nor Reset ever waits for it.
type Runner struct {
	Env Env
	Cfg *Config // optional: plugins and hooks

	calls sync.Mutex // one call at a time; released as soon as Run returns
	mu    sync.Mutex // guards v, cfg and note
	v     *vm        // nil before the first call and after a reset
	cfg   *Config    // the config v was built with
	note  string     // why the VM was reset; reported with the next call
}

// Reset drops the main VM. A non-empty reason is reported to the model with the
// next call's output. A call still running in the VM is not waited for; the VM is
// freed when that call's Lua returns.
func (r *Runner) Reset(reason string) {
	r.mu.Lock()
	old := r.v
	r.v = nil
	if reason != "" {
		r.note = reason
	}
	r.mu.Unlock()
	old.retire()
}

// Close frees the main VM.
func (r *Runner) Close() { r.Reset("") }

// Run executes one tool call: raw Lua code.
func (r *Runner) Run(ctx context.Context, code string) Result {
	r.calls.Lock()
	defer r.calls.Unlock()
	r.mu.Lock()
	var stale *vm
	if r.v != nil && r.cfg != r.Cfg {
		stale, r.v = r.v, nil
		r.note = "init.lua was reloaded"
	}
	if r.v == nil {
		r.v, r.cfg = newVM(r.Env, r.Cfg, roleMain), r.Cfg
	}
	v := r.v
	v.enter()
	note := r.note
	r.note = ""
	r.mu.Unlock()
	stale.retire()

	res, drop := r.run(ctx, v, code)
	if drop != "" {
		r.mu.Lock()
		if r.v == v {
			r.v, r.note = nil, drop
		}
		r.mu.Unlock()
		v.retire()
	}
	if note != "" {
		res.Output = "[lua: fresh VM, " + note + ": globals and functions from earlier calls are gone]\n" + res.Output
	}
	return res
}

// abandonGrace is how long a call that hit its deadline or was cancelled may still
// take to unwind. Lua that stops on its own does so within it and keeps its result;
// a host operation that never returns is abandoned after it.
var abandonGrace = 500 * time.Millisecond

// run executes code in v on a goroutine and waits for it. drop is the reason to
// stop using v when the call timed out or was cancelled before its Lua returned;
// it is empty when v may be used again.
func (r *Runner) run(parent context.Context, v *vm, code string) (res Result, drop string) {
	ctx, cancel := withCallBudget(parent, callTimeout)
	defer cancel()
	tracker := &activityTracker{}
	defer tracker.closeAll()
	ctx = withActivityTracker(ctx, tracker)
	type outcome struct {
		res      Result
		timedOut bool
	}
	ch := make(chan outcome, 1)
	go func() {
		res, timedOut := v.exec(ctx, parent, code)
		v.leave()
		ch <- outcome{res, timedOut}
	}()
	finished := func(o outcome) (Result, string) {
		if o.timedOut {
			return o.res, "the previous call hit the " + callTimeout.String() + " timeout"
		}
		return o.res, ""
	}
	select {
	case o := <-ch:
		return finished(o)
	case <-ctx.Done():
	}
	grace := time.NewTimer(abandonGrace)
	defer grace.Stop()
	select {
	case o := <-ch:
		return finished(o)
	case <-grace.C:
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		return Result{Output: timeoutMessage(), IsError: true}, "the previous call hit the " + callTimeout.String() + " timeout"
	}
	return Result{Output: "lua: " + ECANCELED + ": the call was cancelled", IsError: true}, "the previous call was cancelled and did not stop"
}

func timeoutMessage() string {
	return fmt.Sprintf("lua: %s: the call exceeded its fixed timeout of %s; use tasks.spawn for long work", ETIMEOUT, callTimeout)
}

// exec runs one call's Lua in v. Only the call's goroutine touches v while it runs.
// timedOut reports that the call's own deadline ended it.
func (v *vm) exec(ctx, parent context.Context, code string) (res Result, timedOut bool) {
	v.ctx = ctx
	v.buf.Reset()
	v.installSession()
	v.L.SetContext(ctx)
	defer func() {
		v.L.RemoveContext()
		v.L.SetTop(0)
		v.ctx = context.Background()
	}()
	expired := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil }

	fn, err := v.L.Load(strings.NewReader(code), "lua")
	if err != nil {
		return Result{Output: "error: " + err.Error(), IsError: true}, false
	}
	v.L.Push(fn)
	err = v.L.PCall(0, lua.MultRet, nil)
	if err != nil {
		msg := errorMessage(err)
		switch {
		case expired():
			msg = timeoutMessage()
		case parent.Err() != nil:
			msg = "lua: " + ECANCELED + ": the call was cancelled"
		}
		text := v.buf.String() + "error: " + msg
		if trace := traceback(err); trace != "" {
			text += "\n" + trace
		}
		return Result{Output: truncate(text, maxOutput), IsError: true}, expired()
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

// enter marks v as running a call. The caller holds the Runner's mutex and has just
// taken v from it, so v is not retired.
func (v *vm) enter() {
	v.mu.Lock()
	v.busy = true
	v.mu.Unlock()
}

// leave marks the end of a call's Lua; it frees v if v was retired meanwhile.
func (v *vm) leave() {
	v.mu.Lock()
	v.busy = false
	free := v.retired && !v.freed
	v.freed = v.freed || free
	v.mu.Unlock()
	if free {
		v.L.Close()
	}
}

// retire stops using v. It is freed now when no call runs in it, or by that call's
// goroutine when its Lua returns. Retiring twice is harmless.
func (v *vm) retire() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.retired = true
	free := !v.busy && !v.freed
	v.freed = v.freed || free
	v.mu.Unlock()
	if free {
		v.L.Close()
	}
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
