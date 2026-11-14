package luart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Defaults are the values of zel.defaults; later layers override earlier ones.
type Defaults struct {
	Provider, Model, Effort string
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
	completions     []completion
}

// Source names one init.lua to load: layers load in the order given.
type Source struct{ Name, Path string }

// Config is the snapshot of all init.lua layers plus the long-lived Lua state
// where their hooks and function bodies run.
type Config struct {
	env         Env
	sem         chan struct{}   // one token: holding it means using the config vm
	shutdown    context.Context // ends every call of this snapshot when it is closed
	stop        context.CancelFunc
	v           *vm
	layers      []*Layer
	fns         map[string]*FnDef
	completions map[string]completion
	hooks       map[string][]hook
	loading     *Layer
	LoadErrs    []string
	closed      bool // set by Close: a closed snapshot runs no hooks or functions
}

var hookEvents = map[string]bool{"session_start": true, "session_end": true, "message": true, "turn_end": true, "subagent_start": true, "subagent_end": true}

// jobFns are the lifecycle functions of both the tasks and the agents namespace.
var jobFns = []string{"spawn", "get", "list", "status", "wait", "wait_all", "result", "tail", "stop"}

var builtinFns = map[string][]string{
	"":   {"sh", "sleep", "view", "slice"},
	"fs": {"read", "write", "edit", "patch", "list", "glob", "grep", "exists", "tmp"}, "web": {"request", "get", "post"},
	"json": {"encode", "decode"}, "tasks": jobFns, "agents": append([]string{"run"}, jobFns...),
	"state": {"get", "set", "list"}, "log": {"info", "warn", "notice"}, "session": {"id", "cwd", "root", "parent", "depth", "model", "effort", "rename", "models"},
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
	c := &Config{env: env, sem: make(chan struct{}, 1), fns: map[string]*FnDef{}, completions: map[string]completion{}, hooks: map[string][]hook{}}
	c.shutdown, c.stop = context.WithCancel(context.Background())
	c.v = newVM(env, c, roleConfig)
	c.v.L.PreloadModule("zel", c.zelModule)
	for _, src := range sources {
		if err := c.loadLayer(src); err != nil {
			msg := fmt.Sprintf("%s skipped: it did not finish within 30 s, so the layers after it are skipped too", src.Path)
			c.LoadErrs = append(c.LoadErrs, msg)
			c.env.Notice(msg)
			break
		}
	}
	return c
}

// Close frees the config VM. It never waits for a call: the calls still running end
// with the snapshot's shutdown context, and the VM is freed by the last of them to
// leave, or at once when none runs. A closed snapshot runs no hooks or functions.
// Close is idempotent.
func (c *Config) Close() {
	c.stop()
	select {
	case c.sem <- struct{}{}:
		c.free()
		c.unlock()
	default:
		go func() {
			c.lock()
			c.free()
			c.unlock()
		}()
	}
}

// free closes the VM; the caller holds the lock.
func (c *Config) free() {
	if !c.closed {
		c.closed = true
		c.v.close()
	}
}

func (c *Config) lock()   { c.sem <- struct{}{} }
func (c *Config) unlock() { <-c.sem }

// lockCtx takes the config VM unless ctx ends first, so a caller waiting behind a
// long call gives up with its own budget. A closed snapshot fails with errClosed.
func (c *Config) lockCtx(ctx context.Context) error {
	if c.shutdown.Err() != nil {
		return errClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.sem <- struct{}{}:
		if c.shutdown.Err() != nil || c.closed {
			c.unlock()
			return errClosed
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run executes fn, which uses the config VM, on a goroutine that holds the VM lock
// until fn returns. fn gets a context that ends with ctx, with the snapshot's
// shutdown, and when run returns. run waits for fn or for ctx, whichever ends first,
// so no caller is held past its own deadline. When ctx ends first, the context of
// fn is cancelled: cooperative Lua stops at its next instruction, and a host call
// that never returns holds only that goroutine, with the lock, until it does. The
// VM is then freed by the last call to leave it. run returns fn's error when fn
// completed, and the context's error otherwise. fn's results may be read only when
// run returned nil.
func (c *Config) run(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx, done := c.scope(ctx)
	defer done()
	tracker := &activityTracker{}
	defer tracker.closeAll()
	ctx = withActivityTracker(ctx, tracker)
	if err := c.lockCtx(ctx); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() {
		defer c.unlock()
		finished <- fn(ctx)
	}()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// scope returns ctx, which also ends when the snapshot is closed. Call done when
// the call is over.
func (c *Config) scope(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.shutdown, cancel)
	return ctx, func() { stop(); cancel() }
}

// errClosed is what calls into a snapshot that was reloaded away return.
var errClosed = errors.New("init.lua was reloaded")

// loadLayer runs one init.lua file in the config VM. A layer that fails to load is
// skipped with a notice. An error return means the file did not finish within its
// 30 s: the VM is still held by it, so the caller stops loading layers.
func (c *Config) loadLayer(src Source) error {
	if _, err := os.Stat(src.Path); err != nil {
		return nil
	}
	layer := &Layer{Name: src.Name, Path: src.Path, Dir: filepath.Dir(filepath.Dir(src.Path))}
	if filepath.Base(filepath.Dir(src.Path)) != ".zel" {
		layer.Dir = filepath.Dir(src.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var loadErr error
	err := c.run(ctx, func(ctx context.Context) error {
		c.loading = layer
		defer func() { c.loading = nil }()
		c.v.ctx = ctx
		c.v.L.SetContext(ctx)
		defer c.v.L.RemoveContext()
		fn, err := c.v.L.LoadFile(src.Path)
		if err == nil {
			c.v.L.Push(fn)
			err = c.v.L.PCall(0, 0, nil)
		}
		loadErr = err
		return nil
	})
	if err != nil {
		return err
	}
	if loadErr != nil {
		msg := fmt.Sprintf("%s skipped: %s", src.Path, errorMessage(loadErr))
		c.LoadErrs = append(c.LoadErrs, msg)
		c.env.Notice(msg)
		return nil
	}
	c.layers = append(c.layers, layer)
	for _, f := range layer.fns {
		if old, dup := c.fns[f.Name]; dup {
			c.env.Log("warn", fmt.Sprintf("%s: function %s overrides the one from %s", src.Name, f.Name, old.Layer))
		}
		c.fns[f.Name] = f
	}
	for _, p := range layer.completions {
		if old, dup := c.completions[p.Name]; dup {
			c.env.Log("warn", fmt.Sprintf("%s: completion #%s overrides the one from %s", src.Name, p.Name, old.Layer))
		}
		c.completions[p.Name] = p
	}
	for _, h := range layer.hooks {
		c.hooks[h.event] = append(c.hooks[h.event], h)
	}
	return c.run(ctx, func(ctx context.Context) error {
		c.installFns(c.v)
		return nil
	})
}

// Defaults merges zel.defaults of all layers. The layers are final once LoadConfig
// returns, so reading them needs no lock and never waits behind a call.
func (c *Config) Defaults() Defaults {
	var d Defaults
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
	}
	return d
}

// Fns lists the effective (merged) plugin functions in layer order.
func (c *Config) Fns() []*FnDef {
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
			data, err := readPlain(path)
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
	t.RawSetString("complete", L.NewFunction(func(L *lua.LState) int {
		l := layer("complete")
		name := L.CheckString(1)
		if !promptName.MatchString(name) {
			fail("complete", "name %q must be 1 to 64 letters, digits, - or _", name)
		}
		l.completions = append(l.completions, completion{Name: name, Layer: l.Name, fn: L.CheckFunction(2)})
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
		impl := v.hostFn(def.Name, func(L *lua.LState) int { return c.call(v, def, L) })
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
	if v.role == roleConfig {
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
	var out struct{ res any }
	err := c.run(ctx, func(ctx context.Context) error {
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
			return err
		}
		res := L.Get(-1)
		L.Pop(1)
		g, err := toGo(res)
		out.res = g
		return err
	})
	if err != nil {
		return nil, err
	}
	return out.res, nil
}

// ---- hooks ----------------------------------------------------------------

// emit runs the handlers of an event in order. fold applies one handler's
// returned table to ev and reports whether to stop the chain.
func (c *Config) emit(ctx context.Context, event string, ev map[string]any, fold func(ret map[string]any) bool) {
	// The hooks are final once LoadConfig returns, so listing them needs no lock.
	for _, h := range c.hooks[event] {
		ret, err := c.runHook(ctx, h, ev)
		if err != nil {
			c.env.Log("warn", hookFailure(event, h.layer, err))
			continue
		}
		if ret != nil && fold != nil && fold(ret) {
			return
		}
	}
}

func (c *Config) runHook(ctx context.Context, h hook, ev map[string]any) (map[string]any, error) {
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out struct{ ret map[string]any }
	err := c.run(hctx, func(ctx context.Context) error {
		L := c.v.L
		prev := c.v.ctx
		c.v.ctx = ctx
		L.SetContext(ctx)
		defer func() { c.v.ctx = prev; L.RemoveContext() }()
		hc := L.NewTable()
		hc.RawSetString("session", L.GetGlobal("session"))
		hc.RawSetString("layer", lua.LString(h.layer))
		if err := L.CallByParam(lua.P{Fn: h.fn, NRet: 1, Protect: true}, toLua(L, ev), hc); err != nil {
			return err
		}
		res := L.Get(-1)
		L.Pop(1)
		ret, err := hookResult(res)
		out.ret = ret
		return err
	})
	if err != nil {
		return nil, err
	}
	return out.ret, nil
}

// hookResult is what one hook returned as the map its event folds: a table as is,
// a string as {__string = text}, anything else as nil.
func hookResult(res lua.LValue) (map[string]any, error) {
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

// hookFailure is the warning for a hook that raised an error or ended early: a
// context ended it, or a reload closed its snapshot.
func hookFailure(event, layer string, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosed) {
		return fmt.Sprintf("hook %s (%s) stopped early: %s", event, layer, errorMessage(err))
	}
	return fmt.Sprintf("hook %s (%s): %s", event, layer, errorMessage(err))
}

// notifyText is what one hook asked to tell the agent: a returned string or
// {notify = "text"}.
func notifyText(ret map[string]any) string {
	if s, ok := ret["__string"].(string); ok {
		return strings.TrimSpace(s)
	}
	s, _ := ret["notify"].(string)
	return strings.TrimSpace(s)
}

// collect runs the hooks of an event and joins the notification texts they returned.
func (c *Config) collect(ctx context.Context, event string, ev map[string]any) string {
	var texts []string
	c.emit(ctx, event, ev, func(ret map[string]any) bool {
		if s := notifyText(ret); s != "" {
			texts = append(texts, s)
		}
		return false
	})
	return strings.Join(texts, "\n")
}

// SessionStart runs the session_start hooks; returned text becomes a notification.
func (c *Config) SessionStart(ctx context.Context) string {
	return c.collect(ctx, "session_start", map[string]any{})
}

// SessionEndTimeout is the budget of one session's shutdown. Running subagents are
// cancelled first, their end hooks run next, and session_end runs last with what is
// left, so leaving a session takes at most this long.
const SessionEndTimeout = 5 * time.Second

// SessionEnd runs the session_end hooks when the session is left (reason:
// "quit", "new" or "resume"). ctx is the shutdown budget: a hook that cannot
// start or finish within it is skipped.
func (c *Config) SessionEnd(ctx context.Context, reason string) {
	c.emit(ctx, "session_end", map[string]any{"reason": reason}, nil)
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

// Notice lets message hooks observe a notification without rewriting it.
func (c *Config) Notice(ctx context.Context, text string) {
	c.emit(ctx, "message", map[string]any{"text": text, "notice": true}, nil)
}

// TurnEnd runs the turn_end hooks; returned text becomes a notification
// ("" lets the turn end).
func (c *Config) TurnEnd(ctx context.Context, answer string) string {
	return c.collect(ctx, "turn_end", map[string]any{"answer": answer})
}

// Subagent runs the subagent_start or subagent_end hooks; they return nothing the agent sees.
func (c *Config) Subagent(ctx context.Context, event string, info map[string]any) {
	c.emit(ctx, event, info, nil)
}
