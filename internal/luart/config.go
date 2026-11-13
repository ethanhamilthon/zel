package luart

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Defaults are the values of zel.defaults; later layers override earlier ones.
type Defaults struct {
	Provider, Model, Effort string
	Timeout, OutputLimit    int
}

// Param is one positional parameter of a plugin function.
type Param struct {
	Name, Type, Desc string
	Default          any
	HasDefault       bool
	typ              *Type
}

// FnDef is a function registered with zel.fn.
type FnDef struct {
	Name, Summary, Returns string
	Params                 []Param
	Examples               []string
	Hidden                 bool
	Layer                  string
	run                    *lua.LFunction
}

type hook struct {
	event, layer string
	fn           *lua.LFunction
}

// Layer is the result of one init.lua file.
type Layer struct {
	Name, Path, Dir string
	instructions    []string
	prompts         []*lua.LFunction
	defaults        Defaults
	fns             []*FnDef
	hooks           []hook
}

// Source names one init.lua to load: layers load in the order given.
type Source struct{ Name, Path string }

// Config is the snapshot of all init.lua layers plus the long-lived Lua state
// where their hooks and function bodies run.
type Config struct {
	env      Env
	mu       sync.Mutex // guards the config vm
	v        *vm
	layers   []*Layer
	fns      map[string]*FnDef
	hooks    map[string][]hook
	loading  *Layer
	LoadErrs []string
}

var hookEvents = map[string]bool{"session_start": true, "message": true, "before_call": true, "after_call": true, "turn_end": true, "bg_done": true}

var builtinFns = map[string][]string{
	"":   {"sh", "sleep", "view", "wait_all"},
	"fs": {"read", "write", "edit", "list", "glob", "grep", "exists", "tmp"}, "web": {"request", "get", "post"},
	"json": {"encode", "decode"}, "tasks": {"spawn", "run", "get", "list"}, "agents": {"spawn", "run", "get", "list"},
	"state": {"get", "set", "list"}, "log": {"info", "warn", "notice"}, "session": {"id", "cwd", "root", "parent", "depth", "model", "effort", "rename"},
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

func isBuiltin(name string) bool {
	ns, fn, dotted := strings.Cut(name, ".")
	if !dotted {
		ns, fn = "", name
		if _, isNS := builtinFns[name]; isNS && name != "" {
			return true
		}
	}
	for _, b := range builtinFns[ns] {
		if b == fn {
			return true
		}
	}
	return false
}

// LoadConfig runs the given init.lua files in order. A layer that fails to load
// is skipped and reported through env.Notice.
func LoadConfig(env Env, sources []Source) *Config {
	c := &Config{env: env, fns: map[string]*FnDef{}, hooks: map[string][]hook{}}
	c.v = newVM(env, c, true)
	c.v.L.PreloadModule("zel", c.zelModule)
	for _, src := range sources {
		c.loadLayer(src)
	}
	return c
}

// Close frees the config vm.
func (c *Config) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v.close()
}

func (c *Config) loadLayer(src Source) {
	if _, err := os.Stat(src.Path); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	layer := &Layer{Name: src.Name, Path: src.Path, Dir: filepath.Dir(filepath.Dir(src.Path))}
	if filepath.Base(filepath.Dir(src.Path)) != ".zel" {
		layer.Dir = filepath.Dir(src.Path)
	}
	c.loading = layer
	defer func() { c.loading = nil }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.v.ctx = ctx
	c.v.L.SetContext(ctx)
	defer c.v.L.RemoveContext()
	fn, err := c.v.L.LoadFile(src.Path)
	if err == nil {
		c.v.L.Push(fn)
		err = c.v.L.PCall(0, 0, nil)
	}
	if err != nil {
		msg := fmt.Sprintf("%s skipped: %s", src.Path, errorMessage(err))
		c.LoadErrs = append(c.LoadErrs, msg)
		c.env.Notice(msg)
		return
	}
	c.layers = append(c.layers, layer)
	for _, f := range layer.fns {
		if old, dup := c.fns[f.Name]; dup {
			c.env.Log("warn", fmt.Sprintf("%s: function %s overrides the one from %s", src.Name, f.Name, old.Layer))
		}
		c.fns[f.Name] = f
	}
	for _, h := range layer.hooks {
		c.hooks[h.event] = append(c.hooks[h.event], h)
	}
	c.installFns(c.v)
}

// Defaults merges zel.defaults of all layers.
func (c *Config) Defaults() Defaults {
	var d Defaults
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.layers {
		if l.defaults.Provider != "" {
			d.Provider = l.defaults.Provider
		}
		if l.defaults.Model != "" {
			d.Model = l.defaults.Model
		}
		if l.defaults.Effort != "" {
			d.Effort = l.defaults.Effort
		}
		if l.defaults.Timeout > 0 {
			d.Timeout = l.defaults.Timeout
		}
		if l.defaults.OutputLimit > 0 {
			d.OutputLimit = l.defaults.OutputLimit
		}
	}
	return d
}

// Fns lists the effective (merged) plugin functions in layer order.
func (c *Config) Fns() []*FnDef {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*FnDef
	for _, l := range c.layers {
		for _, f := range l.fns {
			if c.fns[f.Name] == f {
				out = append(out, f)
			}
		}
	}
	return out
}

// HasCallHooks reports whether before_call or after_call hooks exist.
func (c *Config) HasCallHooks() bool {
	return len(c.hooks["before_call"])+len(c.hooks["after_call"]) > 0
}

// HasHook reports whether any hook listens to the event.
func (c *Config) HasHook(event string) bool { return len(c.hooks[event]) > 0 }

// ---- the zel module -------------------------------------------------------

func (c *Config) zelModule(L *lua.LState) int {
	t := L.NewTable()
	fail := func(fn string, format string, args ...any) { raise(L, "zel."+fn, EINVAL, format, args...) }
	layer := func(fn string) *Layer {
		if c.loading == nil {
			fail(fn, "can only be called while init.lua loads")
		}
		return c.loading
	}
	t.RawSetString("instructions", L.NewFunction(func(L *lua.LState) int {
		l := layer("instructions")
		switch x := L.Get(1).(type) {
		case lua.LString:
			l.instructions = append(l.instructions, string(x))
		case *lua.LTable:
			file, ok := optString(x, "file")
			if !ok {
				fail("instructions", "expected text or {file = path}")
			}
			path := file
			if strings.HasPrefix(path, "~/") {
				home, _ := os.UserHomeDir()
				path = filepath.Join(home, path[2:])
			} else if !filepath.IsAbs(path) {
				path = filepath.Join(l.Dir, path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				c.env.Notice("zel.instructions: cannot read " + path)
				return 0
			}
			l.instructions = append(l.instructions, string(data))
		default:
			fail("instructions", "expected text or {file = path}")
		}
		return 0
	}))
	t.RawSetString("prompt", L.NewFunction(func(L *lua.LState) int {
		l := layer("prompt")
		l.prompts = append(l.prompts, L.CheckFunction(1))
		return 0
	}))
	t.RawSetString("defaults", L.NewFunction(func(L *lua.LState) int {
		l := layer("defaults")
		o := L.CheckTable(1)
		var err string
		o.ForEach(func(k, val lua.LValue) {
			key := k.String()
			switch key {
			case "provider", "model", "effort":
				s, ok := val.(lua.LString)
				if !ok {
					err = key + " must be a string"
					return
				}
				switch key {
				case "provider":
					l.defaults.Provider = string(s)
				case "model":
					l.defaults.Model = string(s)
				default:
					l.defaults.Effort = string(s)
				}
			case "timeout", "output_limit":
				n, ok := val.(lua.LNumber)
				if !ok || n <= 0 {
					err = key + " must be a positive number"
					return
				}
				if key == "timeout" {
					l.defaults.Timeout = int(n)
				} else {
					l.defaults.OutputLimit = int(n)
				}
			default:
				err = "unknown key " + key
			}
		})
		if err != "" {
			fail("defaults", "%s", err)
		}
		return 0
	}))
	t.RawSetString("on", L.NewFunction(func(L *lua.LState) int {
		l := layer("on")
		event := L.CheckString(1)
		if !hookEvents[event] {
			fail("on", "unknown event %q", event)
		}
		l.hooks = append(l.hooks, hook{event: event, layer: l.Name, fn: L.CheckFunction(2)})
		return 0
	}))
	t.RawSetString("fn", L.NewFunction(func(L *lua.LState) int {
		l := layer("fn")
		def, err := parseFn(L.CheckTable(1))
		if err != nil {
			fail("fn", "%v", err)
		}
		def.Layer = l.Name
		l.fns = append(l.fns, def)
		return 0
	}))
	L.SetGlobal("zel", t) // plugin functions named zel.* land in the same table
	L.Push(t)
	return 1
}

func parseFn(o *lua.LTable) (*FnDef, error) {
	def := &FnDef{}
	def.Name, _ = optString(o, "name")
	if !nameRe.MatchString(def.Name) {
		return nil, fmt.Errorf("name %q must be \"name\" or \"ns.name\"", def.Name)
	}
	if isBuiltin(def.Name) {
		return nil, fmt.Errorf("%s is built in and cannot be overridden", def.Name)
	}
	def.Summary, _ = optString(o, "summary")
	if def.Summary == "" {
		return nil, fmt.Errorf("%s: summary is required", def.Name)
	}
	def.Returns, _ = optString(o, "returns")
	def.Hidden = optBool(o, "hidden")
	run, ok := o.RawGetString("run").(*lua.LFunction)
	if !ok {
		return nil, fmt.Errorf("%s: run must be a function", def.Name)
	}
	def.run = run
	if ex, ok := o.RawGetString("examples").(*lua.LTable); ok {
		for i := 1; i <= ex.Len(); i++ {
			def.Examples = append(def.Examples, ex.RawGetInt(i).String())
		}
	}
	if ps, ok := o.RawGetString("params").(*lua.LTable); ok {
		for i := 1; i <= ps.Len(); i++ {
			row, ok := ps.RawGetInt(i).(*lua.LTable)
			if !ok {
				return nil, fmt.Errorf("%s: params[%d] must be {name, type, desc?}", def.Name, i)
			}
			var p Param
			p.Name = lua.LVAsString(row.RawGetInt(1))
			p.Type = lua.LVAsString(row.RawGetInt(2))
			p.Desc = lua.LVAsString(row.RawGetInt(3))
			if p.Name == "" || p.Type == "" {
				return nil, fmt.Errorf("%s: params[%d] needs a name and a type", def.Name, i)
			}
			typ, err := ParseType(p.Type)
			if err != nil {
				return nil, fmt.Errorf("%s: param %s: %v", def.Name, p.Name, err)
			}
			p.typ = typ
			if d := o2(row, "default"); d != lua.LNil {
				g, err := toGo(d)
				if err != nil {
					return nil, fmt.Errorf("%s: param %s default: %v", def.Name, p.Name, err)
				}
				p.Default, p.HasDefault = g, true
			}
			def.Params = append(def.Params, p)
		}
	}
	return def, nil
}

func o2(t *lua.LTable, key string) lua.LValue { return t.RawGetString(key) }

// ---- plugin functions in a vm ---------------------------------------------

// installFns makes the registered functions callable as globals of v.
func (c *Config) installFns(v *vm) {
	for _, def := range c.fns {
		def := def
		ns, name, dotted := strings.Cut(def.Name, ".")
		impl := v.L.NewFunction(v.wrap(def.Name, func(L *lua.LState) int { return c.call(v, def, L) }))
		if !dotted {
			v.L.SetGlobal(def.Name, impl)
			continue
		}
		tbl, ok := v.L.GetGlobal(ns).(*lua.LTable)
		if !ok {
			tbl = v.L.NewTable()
			v.L.SetGlobal(ns, tbl)
		}
		tbl.RawSetString(name, impl)
	}
}

// call validates the arguments, fills defaults and runs the function body.
func (c *Config) call(v *vm, def *FnDef, L *lua.LState) int {
	args := make([]lua.LValue, len(def.Params))
	for i, p := range def.Params {
		val := L.Get(i + 1)
		if val == lua.LNil && p.HasDefault {
			val = toLua(L, p.Default)
		}
		if msg := p.typ.Check(val); msg != "" {
			raise(L, def.Name, EINVAL, "arg #%d %s: %s", i+1, p.Name, msg)
		}
		args[i] = val
	}
	if v.direct {
		L.Push(def.run)
		for _, a := range args {
			L.Push(a)
		}
		L.Call(len(args), 1)
		res := L.Get(-1)
		L.Pop(1)
		L.Push(res)
		return 1
	}
	goArgs := make([]any, len(args))
	for i, a := range args {
		g, err := toGo(a)
		if err != nil {
			raise(L, def.Name, EINVAL, "arg #%d %s: %v", i+1, def.Params[i].Name, err)
		}
		goArgs[i] = g
	}
	res, err := c.invoke(v.ctx, def, goArgs)
	if err != nil {
		if v.ctx.Err() != nil {
			v.ctxErr(def.Name)
		}
		msg := errorMessage(err)
		if !errCodeRe.MatchString(msg) {
			msg = def.Name + ": " + EFAIL + ": " + msg
		}
		L.Error(lua.LString(msg), 0)
	}
	L.Push(toLua(L, res))
	return 1
}

var errCodeRe = regexp.MustCompile(`^[\w.:]+: E[A-Z]+: `)

func (c *Config) invoke(ctx context.Context, def *FnDef, args []any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	L := c.v.L
	prev := c.v.ctx
	c.v.ctx = ctx
	L.SetContext(ctx)
	defer func() { c.v.ctx = prev; L.RemoveContext() }()
	luaArgs := make([]lua.LValue, len(args))
	for i, a := range args {
		luaArgs[i] = toLua(L, a)
	}
	if err := L.CallByParam(lua.P{Fn: def.run, NRet: 1, Protect: true}, luaArgs...); err != nil {
		return nil, err
	}
	res := L.Get(-1)
	L.Pop(1)
	return toGo(res)
}

// ---- hooks ----------------------------------------------------------------

// emit runs the handlers of an event in order. fold applies one handler's
// returned table to ev and reports whether to stop the chain.
func (c *Config) emit(ctx context.Context, event string, ev map[string]any, fold func(ret map[string]any) bool) {
	c.mu.Lock()
	handlers := append([]hook(nil), c.hooks[event]...)
	c.mu.Unlock()
	for _, h := range handlers {
		ret, err := c.runHook(ctx, h, ev)
		if err != nil {
			c.env.Log("warn", fmt.Sprintf("hook %s (%s): %v", event, h.layer, errorMessage(err)))
			continue
		}
		if ret != nil && fold != nil && fold(ret) {
			return
		}
	}
}

func (c *Config) runHook(ctx context.Context, h hook, ev map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	L := c.v.L
	prev := c.v.ctx
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c.v.ctx = hctx
	L.SetContext(hctx)
	defer func() { c.v.ctx = prev; L.RemoveContext() }()
	hc := L.NewTable()
	hc.RawSetString("session", L.GetGlobal("session"))
	hc.RawSetString("layer", lua.LString(h.layer))
	if err := L.CallByParam(lua.P{Fn: h.fn, NRet: 1, Protect: true}, toLua(L, ev), hc); err != nil {
		return nil, err
	}
	res := L.Get(-1)
	L.Pop(1)
	t, ok := res.(*lua.LTable)
	if !ok {
		if s, isStr := res.(lua.LString); isStr {
			return map[string]any{"__string": string(s)}, nil
		}
		return nil, nil
	}
	g, err := toGo(t)
	if err != nil {
		return nil, err
	}
	m, _ := g.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// SessionStart runs the session_start hooks.
func (c *Config) SessionStart(ctx context.Context) {
	c.emit(ctx, "session_start", map[string]any{}, nil)
}

// Message lets message hooks rewrite the user's text.
func (c *Config) Message(ctx context.Context, text string) string {
	ev := map[string]any{"text": text}
	c.emit(ctx, "message", ev, func(ret map[string]any) bool {
		if s, ok := ret["__string"].(string); ok {
			ev["text"] = s
		}
		return false
	})
	return ev["text"].(string)
}

// BeforeCall returns replaced arguments (nil: unchanged) or a denial reason.
func (c *Config) BeforeCall(ctx context.Context, fn string, args []any) (newArgs []any, deny string) {
	if !c.HasHook("before_call") {
		return nil, ""
	}
	ev := map[string]any{"fn": fn, "args": args}
	c.emit(ctx, "before_call", ev, func(ret map[string]any) bool {
		if d, ok := ret["deny"].(string); ok && d != "" {
			deny = d
			return true
		}
		if a, ok := ret["args"].([]any); ok {
			newArgs = a
			ev["args"] = a
		}
		return false
	})
	return newArgs, deny
}

// AfterCall returns a replaced result when a hook supplied one.
func (c *Config) AfterCall(ctx context.Context, fn string, args []any, result any, errText string, took time.Duration) (any, bool) {
	if !c.HasHook("after_call") {
		return nil, false
	}
	ev := map[string]any{"fn": fn, "args": args, "result": result, "ms": took.Milliseconds()}
	if errText != "" {
		ev["err"] = errText
	}
	replaced := false
	c.emit(ctx, "after_call", ev, func(ret map[string]any) bool {
		if r, ok := ret["result"]; ok {
			ev["result"], replaced = r, true
		}
		return false
	})
	return ev["result"], replaced
}

// TurnEnd returns text to inject as a new user message ("" to let the turn end).
func (c *Config) TurnEnd(ctx context.Context, answer string) string {
	var inject []string
	c.emit(ctx, "turn_end", map[string]any{"answer": answer}, func(ret map[string]any) bool {
		if s, ok := ret["inject"].(string); ok && s != "" {
			inject = append(inject, s)
		}
		return false
	})
	return strings.Join(inject, "\n\n")
}

// BgDone lets hooks rewrite (text) or drop a finished background result.
func (c *Config) BgDone(ctx context.Context, kind, id string, result map[string]any) (text string, drop bool) {
	c.emit(ctx, "bg_done", map[string]any{"kind": kind, "id": id, "result": result}, func(ret map[string]any) bool {
		if d, ok := ret["drop"].(bool); ok && d {
			drop = true
			return true
		}
		if s, ok := ret["text"].(string); ok {
			text = s
		}
		return false
	})
	return text, drop
}
