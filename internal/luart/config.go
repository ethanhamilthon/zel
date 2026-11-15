package luart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Tool is a function an init.lua layer gives the agent: tools = {name = {desc, fn}}.
// The agent calls it with one table of arguments.
type Tool struct {
	Name, Desc, Layer string
	fn                *lua.LFunction
}

type hook struct {
	event, layer string
	fn           *lua.LFunction
}

// instruction is one piece of the prompt: static text, or a function that returns it.
// only limits it to the main agent ("main") or to subagents ("sub"); "" is both.
type instruction struct {
	text string
	fn   *lua.LFunction
	only string
}

// Layer is the result of one init.lua file.
type Layer struct {
	Name, Path   string
	instructions []instruction
	tools        []*Tool
	hooks        []hook
	completions  []completion
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
	tools       map[string]*Tool
	completions map[string]completion
	hooks       map[string][]hook
	LoadErrs    []string
	closed      bool // set by Close: a closed snapshot runs no hooks or functions
}

var hookEvents = map[string]bool{"session_start": true, "session_end": true, "message": true, "turn_end": true}

// builtinNames are the globals of the host API; a tool cannot take their names.
var builtinNames = map[string]bool{
	"print": true, "sh": true, "view": true, "slice": true, "read": true, "write": true, "edit": true, "patch": true,
	"list": true, "glob": true, "grep": true, "exists": true, "tmp": true, "log": true, "models": true,
	"tasks": true, "agent": true, "session": true,
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// isBuiltin reports whether a tool name would replace part of the host API: a
// host global, or a function inside the tasks or session table.
func isBuiltin(name string) bool {
	ns, _, dotted := strings.Cut(name, ".")
	if !dotted {
		return builtinNames[name]
	}
	return ns == "tasks" || ns == "session"
}

// LoadConfig runs the given init.lua files in order. A layer that fails to load
// is skipped and reported through env.Notice.
func LoadConfig(env Env, sources []Source) *Config {
	c := &Config{env: env, sem: make(chan struct{}, 1), tools: map[string]*Tool{}, completions: map[string]completion{}, hooks: map[string][]hook{}}
	c.shutdown, c.stop = context.WithCancel(context.Background())
	c.v = newVM(env, c, roleConfig)
	c.v.L.PreloadModule("zel", removedZel)
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

// loadLayer runs one init.lua file in the config VM and reads the table it
// returns. A layer that fails to load is skipped with a notice. An error return
// means the file did not finish within its 30 s: the VM is still held by it, so
// the caller stops loading layers.
func (c *Config) loadLayer(src Source) error {
	if _, err := os.Stat(src.Path); err != nil {
		return nil
	}
	layer := &Layer{Name: src.Name, Path: src.Path}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var loadErr error
	err := c.run(ctx, func(ctx context.Context) error {
		L := c.v.L
		c.v.ctx = ctx
		L.SetContext(ctx)
		defer L.RemoveContext()
		fn, err := L.LoadFile(src.Path)
		if err == nil {
			L.Push(fn)
			err = L.PCall(0, 1, nil)
		}
		if err == nil {
			ret := L.Get(-1)
			L.Pop(1)
			err = layer.read(ret, "init.lua")
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
	for _, t := range layer.tools {
		if old, dup := c.tools[t.Name]; dup {
			c.env.Log("warn", fmt.Sprintf("%s: tool %s overrides the one from %s", src.Name, t.Name, old.Layer))
		}
		c.tools[t.Name] = t
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
		c.installTools(c.v)
		return nil
	})
}

// Tools lists the effective (merged) tools in layer order.
func (c *Config) Tools() []*Tool {
	var out []*Tool
	for _, l := range c.layers {
		for _, t := range l.tools {
			if c.tools[t.Name] == t {
				out = append(out, t)
			}
		}
	}
	return out
}

// HasHook reports whether any hook listens to the event.
func (c *Config) HasHook(event string) bool { return len(c.hooks[event]) > 0 }

// ---- what init.lua returns ------------------------------------------------

// removedZel answers require("zel"), which 0.6 removed.
func removedZel(L *lua.LState) int {
	L.RaiseError(`require("zel") was removed in zel 0.6: init.lua returns a table {instructions, tools, hooks, completions}`)
	return 0
}

// partKeys are the keys of a table init.lua returns.
var partKeys = "instructions, tools, hooks, completions"

// read adds what init.lua returned: nil (or true, what require gives for a
// module that returns nothing), a part {instructions, tools, hooks,
// completions}, or a list of those, nested as deep as needed. where names the
// value in errors.
func (l *Layer) read(v lua.LValue, where string) error {
	switch x := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		if x {
			return nil
		}
	case *lua.LTable:
		if isList(x) {
			for i := 1; i <= x.Len(); i++ {
				if err := l.read(x.RawGetInt(i), fmt.Sprintf("%s[%d]", where, i)); err != nil {
					return err
				}
			}
			return nil
		}
		return l.readPart(x, where)
	}
	return fmt.Errorf("%s must return a table {%s} or a list of them, got %s", where, partKeys, v.Type())
}

// isList reports whether t has only the keys 1..n.
func isList(t *lua.LTable) bool {
	n, list := t.Len(), true
	if n == 0 {
		return false
	}
	t.ForEach(func(k, _ lua.LValue) {
		if i, ok := k.(lua.LNumber); !ok || float64(i) != float64(int(i)) || int(i) < 1 || int(i) > n {
			list = false
		}
	})
	return list
}

// sortedFields lists the string keys of t in order and fails on any other key.
func sortedFields(t *lua.LTable, where string) ([]string, error) {
	var keys []string
	var bad error
	t.ForEach(func(k, _ lua.LValue) {
		s, ok := k.(lua.LString)
		if !ok && bad == nil {
			bad = fmt.Errorf("%s: keys must be names, got %s", where, k.Type())
		}
		keys = append(keys, string(s))
	})
	sort.Strings(keys)
	return keys, bad
}

func (l *Layer) readPart(t *lua.LTable, where string) error {
	keys, err := sortedFields(t, where)
	if err != nil {
		return err
	}
	for _, key := range keys {
		v := t.RawGetString(key)
		var err error
		switch key {
		case "instructions":
			err = l.readInstructions(v, where+".instructions")
		case "tools":
			err = l.readTools(v, where+".tools")
		case "hooks":
			err = l.readHooks(v, where+".hooks")
		case "completions":
			err = l.readCompletions(v, where+".completions")
		case "defaults":
			err = fmt.Errorf("%s.defaults was removed in zel 0.6: pick the model and effort in the UI (Ctrl+M)", where)
		default:
			err = fmt.Errorf("%s: unknown key %q (expected %s)", where, key, partKeys)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

const instructionsShape = "text, a function, {text or function, only = \"main\" | \"sub\"} or a list of them"

func (l *Layer) readInstructions(v lua.LValue, where string) error {
	return l.readInstruction(v, where, "")
}

// readInstruction reads one entry; only comes from an enclosing {..., only = ...}.
func (l *Layer) readInstruction(v lua.LValue, where, only string) error {
	switch x := v.(type) {
	case lua.LString:
		l.instructions = append(l.instructions, instruction{text: string(x), only: only})
	case *lua.LFunction:
		l.instructions = append(l.instructions, instruction{fn: x, only: only})
	case *lua.LTable:
		if o := x.RawGetString("only"); o != lua.LNil {
			if only != "" {
				return fmt.Errorf("%s: only is already set by an enclosing entry", where)
			}
			s, ok := o.(lua.LString)
			if !ok || (s != "main" && s != "sub") {
				return fmt.Errorf("%s.only must be \"main\" (the main agent) or \"sub\" (subagents)", where)
			}
			only = string(s)
			var bad error
			x.ForEach(func(k, _ lua.LValue) {
				if _, num := k.(lua.LNumber); !num && k.String() != "only" && bad == nil {
					bad = fmt.Errorf("%s: unknown key %q (expected only)", where, k.String())
				}
			})
			if bad != nil {
				return bad
			}
		} else if x.Len() > 0 && !isList(x) {
			return fmt.Errorf("%s must be %s", where, instructionsShape)
		}
		for i := 1; i <= x.Len(); i++ {
			if err := l.readInstruction(x.RawGetInt(i), fmt.Sprintf("%s[%d]", where, i), only); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s must be %s, got %s", where, instructionsShape, v.Type())
	}
	return nil
}

func (l *Layer) readTools(v lua.LValue, where string) error {
	t, ok := v.(*lua.LTable)
	if !ok {
		return fmt.Errorf("%s must be a table {name = {desc = \"...\", fn = function(args) end}}, got %s", where, v.Type())
	}
	names, err := sortedFields(t, where)
	if err != nil {
		return err
	}
	for _, name := range names {
		at := where + "." + name
		if !nameRe.MatchString(name) {
			return fmt.Errorf("%s: a tool name is \"name\" or \"ns.name\"", at)
		}
		if isBuiltin(name) {
			return fmt.Errorf("%s: %s is built in and cannot be replaced", at, name)
		}
		def, ok := t.RawGetString(name).(*lua.LTable)
		if !ok {
			return fmt.Errorf("%s must be {desc = \"...\", fn = function(args) end}", at)
		}
		fields, err := sortedFields(def, at)
		if err != nil {
			return err
		}
		tool := &Tool{Name: name, Layer: l.Name}
		for _, f := range fields {
			switch f {
			case "desc":
				s, ok := def.RawGetString(f).(lua.LString)
				if !ok {
					return fmt.Errorf("%s.desc must be text", at)
				}
				tool.Desc = strings.TrimSpace(string(s))
			case "fn":
				fn, ok := def.RawGetString(f).(*lua.LFunction)
				if !ok {
					return fmt.Errorf("%s.fn must be a function", at)
				}
				tool.fn = fn
			default:
				return fmt.Errorf("%s: unknown key %q (expected desc, fn)", at, f)
			}
		}
		if tool.Desc == "" {
			return fmt.Errorf("%s.desc is required: say what it does, its arguments and what it returns", at)
		}
		if tool.fn == nil {
			return fmt.Errorf("%s.fn is required", at)
		}
		l.tools = append(l.tools, tool)
	}
	return nil
}

func (l *Layer) readHooks(v lua.LValue, where string) error {
	t, ok := v.(*lua.LTable)
	if !ok {
		return fmt.Errorf("%s must be a table {event = function}, got %s", where, v.Type())
	}
	events, err := sortedFields(t, where)
	if err != nil {
		return err
	}
	for _, event := range events {
		if !hookEvents[event] {
			return fmt.Errorf("%s: unknown hook %q (expected session_start, message, turn_end, session_end)", where, event)
		}
		fn, ok := t.RawGetString(event).(*lua.LFunction)
		if !ok {
			return fmt.Errorf("%s.%s must be a function", where, event)
		}
		l.hooks = append(l.hooks, hook{event: event, layer: l.Name, fn: fn})
	}
	return nil
}

func (l *Layer) readCompletions(v lua.LValue, where string) error {
	t, ok := v.(*lua.LTable)
	if !ok {
		return fmt.Errorf("%s must be a table {name = function}, got %s", where, v.Type())
	}
	names, err := sortedFields(t, where)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !promptName.MatchString(name) {
			return fmt.Errorf("%s: name %q must be 1 to 64 letters, digits, - or _", where, name)
		}
		fn, ok := t.RawGetString(name).(*lua.LFunction)
		if !ok {
			return fmt.Errorf("%s.%s must be a function", where, name)
		}
		l.completions = append(l.completions, completion{Name: name, Layer: l.Name, fn: fn})
	}
	return nil
}

// ---- tools in a vm --------------------------------------------------------

// installTools makes the tools callable as globals of v.
func (c *Config) installTools(v *vm) {
	for _, tool := range c.tools {
		tool := tool
		ns, name, dotted := strings.Cut(tool.Name, ".")
		impl := v.hostFn(tool.Name, func(L *lua.LState) int { return c.call(v, tool, L) })
		if !dotted {
			v.L.SetGlobal(tool.Name, impl)
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

// call runs a tool with its one table of arguments (nil when called without).
func (c *Config) call(v *vm, tool *Tool, L *lua.LState) int {
	arg := L.Get(1)
	if _, ok := arg.(*lua.LTable); !ok && arg != lua.LNil {
		raise(L, tool.Name, EINVAL, "takes one table of arguments: %s{...}, got %s", tool.Name, arg.Type())
	}
	if L.GetTop() > 1 {
		raise(L, tool.Name, EINVAL, "takes one table of arguments: %s{...}", tool.Name)
	}
	if v.role == roleConfig {
		L.Push(tool.fn)
		L.Push(arg)
		L.Call(1, 1)
		return 1
	}
	g, err := toGo(arg)
	if err != nil {
		raise(L, tool.Name, EINVAL, "arguments: %v", err)
	}
	res, err := c.invoke(v.ctx, tool, g)
	if err != nil {
		if v.ctx.Err() != nil {
			v.ctxErr(tool.Name)
		}
		msg := errorMessage(err)
		if !errCodeRe.MatchString(msg) {
			msg = tool.Name + ": " + EFAIL + ": " + msg
		}
		L.Error(lua.LString(msg), 0)
	}
	L.Push(toLua(L, res))
	return 1
}

var errCodeRe = regexp.MustCompile(`^[\w.:]+: E[A-Z]+: `)

// invoke runs a tool's function in the config VM; arguments and result are copied.
func (c *Config) invoke(ctx context.Context, tool *Tool, arg any) (any, error) {
	var out struct{ res any }
	err := c.call1(ctx, tool.fn, []any{arg}, func(L *lua.LState, res lua.LValue) error {
		g, err := toGo(res)
		out.res = g
		return err
	})
	if err != nil {
		return nil, err
	}
	return out.res, nil
}

// call1 calls fn in the config VM with args and hands its first result to read,
// which runs while the VM is still held.
func (c *Config) call1(ctx context.Context, fn *lua.LFunction, args []any, read func(L *lua.LState, res lua.LValue) error) error {
	return c.run(ctx, func(ctx context.Context) error {
		L := c.v.L
		prev := c.v.ctx
		c.v.ctx = ctx
		L.SetContext(ctx)
		defer func() { c.v.ctx = prev; L.RemoveContext() }()
		luaArgs := make([]lua.LValue, len(args))
		for i, a := range args {
			luaArgs[i] = toLua(L, a)
		}
		if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, luaArgs...); err != nil {
			return err
		}
		res := L.Get(-1)
		L.Pop(1)
		return read(L, res)
	})
}

// ---- hooks ----------------------------------------------------------------

// emit runs the handlers of an event in order with args. Each one's returned
// text goes to fold ("" when it returned no text); args may change between calls.
func (c *Config) emit(ctx context.Context, event string, args func() []any, fold func(text string, isText bool)) {
	// The hooks are final once LoadConfig returns, so listing them needs no lock.
	for _, h := range c.hooks[event] {
		text, isText, err := c.runHook(ctx, h, args())
		if err != nil {
			c.env.Log("warn", hookFailure(event, h.layer, err))
			continue
		}
		if fold != nil {
			fold(text, isText)
		}
	}
}

func (c *Config) runHook(ctx context.Context, h hook, args []any) (text string, isText bool, err error) {
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err = c.call1(hctx, h.fn, args, func(L *lua.LState, res lua.LValue) error {
		if s, ok := res.(lua.LString); ok {
			text, isText = string(s), true
		}
		return nil
	})
	return text, isText, err
}

// hookFailure is the warning for a hook that raised an error or ended early: a
// context ended it, or a reload closed its snapshot.
func hookFailure(event, layer string, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosed) {
		return fmt.Sprintf("hook %s (%s) stopped early: %s", event, layer, errorMessage(err))
	}
	return fmt.Sprintf("hook %s (%s): %s", event, layer, errorMessage(err))
}

// errorsIsStop reports whether err is a call that a context or a reload ended.
func errorsIsStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosed)
}

// collect runs the hooks of an event and joins the texts they returned.
func (c *Config) collect(ctx context.Context, event string, args ...any) string {
	var texts []string
	c.emit(ctx, event, func() []any { return args }, func(text string, _ bool) {
		if s := strings.TrimSpace(text); s != "" {
			texts = append(texts, s)
		}
	})
	return strings.Join(texts, "\n")
}

// SessionStart runs the session_start hooks; returned text becomes a notification.
func (c *Config) SessionStart(ctx context.Context) string {
	return c.collect(ctx, "session_start")
}

// SessionEndTimeout is the budget for stopping jobs and running the session_end
// hooks when a session is left. It does not bound DB or config preparation/saving
// before shutdown, so NewSession, Resume, or save-on-quit can take longer.
const SessionEndTimeout = 5 * time.Second

// SessionEnd runs the session_end hooks when the session is left (reason:
// "quit", "new" or "resume"). ctx is the shutdown budget: a hook that cannot
// start or finish within it is skipped.
func (c *Config) SessionEnd(ctx context.Context, reason string) {
	c.emit(ctx, "session_end", func() []any { return []any{reason} }, nil)
}

// Message lets message hooks rewrite the user's text: each one gets the text the
// previous one returned.
func (c *Config) Message(ctx context.Context, text string) string {
	c.emit(ctx, "message", func() []any { return []any{text, false} }, func(s string, isText bool) {
		if isText {
			text = s
		}
	})
	return text
}

// Notice lets message hooks observe a notification without rewriting it.
func (c *Config) Notice(ctx context.Context, text string) {
	c.emit(ctx, "message", func() []any { return []any{text, true} }, nil)
}

// RunningTask is one background task still running when a turn ends.
type RunningTask struct {
	ID, Name string
	Ms       int64
}

// TurnEnd runs the turn_end hooks with the answer and the tasks still running;
// returned text becomes a notification ("" lets the turn end).
func (c *Config) TurnEnd(ctx context.Context, answer string, running []RunningTask) string {
	list := make([]any, 0, len(running))
	for _, t := range running {
		list = append(list, map[string]any{"id": t.ID, "name": t.Name, "ms": t.Ms})
	}
	return c.collect(ctx, "turn_end", answer, list)
}
