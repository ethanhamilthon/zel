package luart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Limits of one call.
const (
	DefaultTimeout     = 120 * time.Second
	MaxTimeout         = 600 * time.Second
	DefaultOutputLimit = 8192
)

// vm is one Lua state with the host API installed. Agent calls get a fresh vm;
// the long-lived config vm runs init.lua, hooks and plugin function bodies.
type vm struct {
	L       *lua.LState
	env     Env
	cfg     *Config
	ctx     context.Context
	out     strings.Builder
	noHooks bool // the config vm never runs hooks, so hooks cannot recurse
	direct  bool // plugin functions are called in this vm without the bridge
	mt      *lua.LTable
}

func newVM(env Env, cfg *Config, config bool) *vm {
	v := &vm{L: lua.NewState(), env: env, cfg: cfg, ctx: context.Background(), noHooks: config, direct: config}
	v.installHost()
	if cfg != nil {
		cfg.installFns(v)
	}
	return v
}

func (v *vm) close() { v.L.Close() }

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

// Call is a parsed tool input: Lua code and the options of its pragma line.
type Call struct {
	Code    string
	Timeout time.Duration // zero: the default
}

// ParseCall splits the optional first line "--! timeout=N" from the code.
func ParseCall(input string) (Call, error) {
	call := Call{Code: input}
	first, rest, _ := strings.Cut(input, "\n")
	trimmed := strings.TrimSpace(first)
	if !strings.HasPrefix(trimmed, "--!") {
		return call, nil
	}
	for _, field := range strings.Fields(strings.TrimPrefix(trimmed, "--!")) {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "timeout":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > int(MaxTimeout/time.Second) {
				return call, fmt.Errorf("lua: %s: timeout must be 1..%d seconds", EINVAL, int(MaxTimeout/time.Second))
			}
			call.Timeout = time.Duration(n) * time.Second
		default:
			return call, fmt.Errorf("lua: %s: unknown option %q in the --! line", EINVAL, key)
		}
	}
	call.Code = rest
	return call, nil
}

// Result is what the model sees after a call.
type Result struct {
	Output  string
	IsError bool
}

// Runner executes tool calls.
type Runner struct {
	Env Env
	Cfg *Config // optional: plugins and hooks
}

// Run executes one tool call in a fresh Lua state.
func (r *Runner) Run(ctx context.Context, input string) Result {
	call, err := ParseCall(input)
	if err != nil {
		return Result{Output: err.Error(), IsError: true}
	}
	defaults := Defaults{}
	if r.Cfg != nil {
		defaults = r.Cfg.Defaults()
	}
	timeout := call.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
		if defaults.Timeout > 0 {
			timeout = min(time.Duration(defaults.Timeout)*time.Second, MaxTimeout)
		}
	}
	limit := DefaultOutputLimit
	if defaults.OutputLimit > 0 {
		limit = defaults.OutputLimit
	}
	if r.Cfg != nil {
		args, deny := r.Cfg.BeforeCall(ctx, "lua", []any{call.Code})
		if deny != "" {
			return Result{Output: "lua: " + EDENIED + ": " + deny, IsError: true}
		}
		if len(args) > 0 {
			if code, ok := args[0].(string); ok {
				call.Code = code
			}
		}
	}
	started := time.Now()
	res := r.run(ctx, call, timeout, limit)
	if r.Cfg != nil {
		var callErr string
		if res.IsError {
			callErr = res.Output
		}
		if out, ok := r.Cfg.AfterCall(ctx, "lua", []any{call.Code}, res.Output, callErr, time.Since(started)); ok {
			if text, isText := out.(string); isText {
				res.Output = truncate(text, limit)
			}
		}
	}
	return res
}

func (r *Runner) run(parent context.Context, call Call, timeout time.Duration, limit int) Result {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	v := newVM(r.Env, r.Cfg, false)
	defer v.close()
	v.ctx = ctx
	v.L.SetContext(ctx)
	v.L.SetGlobal("print", v.L.NewFunction(v.print))

	fn, err := v.L.Load(strings.NewReader(call.Code), "lua")
	if err != nil {
		return Result{Output: "error: " + err.Error(), IsError: true}
	}
	v.L.Push(fn)
	err = v.L.PCall(0, lua.MultRet, nil)
	if err != nil {
		msg := errorMessage(err)
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil:
			msg = fmt.Sprintf("lua: %s: the call exceeded its timeout of %s", ETIMEOUT, timeout)
		case parent.Err() != nil:
			msg = "lua: " + ECANCELED + ": the call was cancelled"
		}
		text := v.out.String() + "error: " + msg
		if trace := traceback(err); trace != "" {
			text += "\n" + trace
		}
		return Result{Output: truncate(text, limit), IsError: true}
	}
	var parts []string
	if s := strings.TrimRight(v.out.String(), "\n"); s != "" {
		parts = append(parts, s)
	}
	for i := 1; i <= v.L.GetTop(); i++ {
		parts = append(parts, renderValue(v.L.Get(i)))
	}
	return Result{Output: truncate(strings.Join(parts, "\n"), limit)}
}

func (v *vm) print(L *lua.LState) int {
	n := L.GetTop()
	for i := 1; i <= n; i++ {
		if i > 1 {
			v.out.WriteByte('\t')
		}
		v.out.WriteString(luaString(L, L.Get(i)))
	}
	v.out.WriteByte('\n')
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
	return head + fmt.Sprintf("\n[output truncated: %d of %d bytes omitted (limit %d); filter in Lua]\n", len(s)-len(head)-len(tail), len(s), limit) + tail
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
