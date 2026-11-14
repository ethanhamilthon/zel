package luart

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// BuiltinPrompt is the fixed part of the system prompt: the Lua tool and the host API.
const BuiltinPrompt = `# Lua
One tool: lua (raw Lua). Each call has a fixed 30 s timeout that cannot be changed: run longer work with tasks.spawn/agents.spawn and check it in later calls.
The VM persists between calls: globals and functions stay until the session changes, init.lua reloads or a call times out; the next output then starts with "[lua: fresh VM ...]". state.* outlives the VM.
Output = print() + return (JSON), never truncated: filter in Lua and print only what you need, e.g. local a = fs.read("bigfile.md"); print(slice(a, 100, 10)).
Errors raise "fn: CODE: msg" (pcall); sh/web/agents.run return ok=false instead. Everything blocks.
You wake up only for user messages and notifications like <notification>t1 finished</notification>; they are short, fetch details yourself (tasks.result(id)).
## built-in
sh(cmd, o?) -> {ok,code,out,timed_out,ms}  -- o: timeout,cwd,env,stdin,check
fs.read(path) -> string; slice(s, from, count?) -> string  -- lines from..from+count-1; from<0 counts from the end
fs.write(path, s, o?)  -- o: append
fs.edit(path, old, new, o?) -> n  -- exact match; o: all
fs.patch(path, {{old, new}, ...}) -> n  -- several exact edits, all or nothing; an edit may set all=true
fs.list(dir) -> {name,dir,size}[]; fs.glob(pat) -> string[]; fs.exists(path) -> bool,kind; fs.tmp(name?) -> path
fs.grep(re, o?) -> {file,line,text}[]  -- o: path,glob,fixed,max
web.get(url, o?) / web.post(url, o?) / web.request{url,method,headers,body,json,timeout} -> {ok,status,headers,body,error}
json.encode(v, o?) / json.decode(s)
tasks.spawn(fn, o?) -> id  -- fn runs in its own VM in the background, no time limit; o: name,timeout
--   fn gets copies of what it captures (locals, tables, functions); changes stay inside and globals of this VM are not there
--   result: {ok,value,error,out,ms,timed_out}; value = fn's return value, out = what it printed
agents.spawn{prompt,model,effort,provider,cwd,timeout} -> id; agents.run{...} -> {ok,answer,session,error}
tasks.* and agents.* both have: get(id) list() status(id) wait(id, timeout?) wait_all(ids, timeout?) result(id) tail(id, n?) stop(id)
state.get(k, scope?) / state.set(k, v, scope?) / state.list(prefix?, scope?)  -- scope: "session"|"project"; JSON values
view(path, ...); session.{id,cwd,root,depth}; log.info/notice(msg); sleep(s)`

// Builtin is BuiltinPrompt for the given agent depth: subagents (depth > 0)
// cannot start agents, so their prompt leaves agents.* out.
func Builtin(depth int) string {
	if depth == 0 {
		return BuiltinPrompt
	}
	r := strings.NewReplacer(
		"run longer work with tasks.spawn/agents.spawn", "run longer work with tasks.spawn",
		"agents.spawn{prompt,model,effort,provider,cwd,timeout} -> id; agents.run{...} -> {ok,answer,session,error}\n", "",
		"tasks.* and agents.* both have:", "tasks.* also has:",
		"sh/web/agents.run return", "sh/web return",
	)
	return r.Replace(BuiltinPrompt) + "\nYou are a subagent: agents.* is not available."
}

// Prompt renders the plugin-provided part of the system prompt: instructions and
// dynamic pieces of every layer, in layer order.
func (c *Config) Prompt(ctx context.Context) string {
	c.mu.Lock()
	layers := append([]*Layer(nil), c.layers...)
	c.mu.Unlock()
	var parts []string
	for _, l := range layers {
		for _, text := range l.instructions {
			if t := strings.TrimSpace(text); t != "" {
				parts = append(parts, t)
			}
		}
		for _, fn := range l.prompts {
			if t := strings.TrimSpace(c.renderPrompt(ctx, l, fn)); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func (c *Config) renderPrompt(ctx context.Context, l *Layer, fn *lua.LFunction) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	L := c.v.L
	pctx, cancel := context.WithTimeout(ctx, 60e9)
	defer cancel()
	prev := c.v.ctx
	c.v.ctx = pctx
	L.SetContext(pctx)
	defer func() { c.v.ctx = prev; L.RemoveContext() }()
	arg := L.NewTable()
	arg.RawSetString("session", L.GetGlobal("session"))
	arg.RawSetString("layer", lua.LString(l.Name))
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, arg); err != nil {
		c.env.Notice(fmt.Sprintf("%s: zel.prompt failed: %s", l.Name, errorMessage(err)))
		return ""
	}
	res := L.Get(-1)
	L.Pop(1)
	if res == lua.LNil {
		return ""
	}
	return luaString(L, res)
}

// FunctionDocs renders the plugin functions grouped by layer ("## name (path)").
func (c *Config) FunctionDocs() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	home, _ := os.UserHomeDir()
	var b strings.Builder
	for _, l := range c.layers {
		var lines []string
		for _, f := range l.fns {
			if f.Hidden || c.fns[f.Name] != f {
				continue
			}
			lines = append(lines, renderFn(f)...)
		}
		if len(lines) == 0 {
			continue
		}
		path := l.Path
		if home != "" && strings.HasPrefix(path, home+"/") {
			path = "~" + strings.TrimPrefix(path, home)
		}
		fmt.Fprintf(&b, "\n## %s (%s)\n%s", l.Name, path, strings.Join(lines, "\n"))
	}
	return b.String()
}

// SystemPrompt is the whole prompt: layer instructions, then the Lua API.
func (c *Config) SystemPrompt(ctx context.Context) string {
	text := Builtin(c.env.Info().Depth) + c.FunctionDocs()
	if own := c.Prompt(ctx); own != "" {
		return own + "\n\n" + text
	}
	return text
}

func renderFn(f *FnDef) []string {
	var params []string
	for _, p := range f.Params {
		s := p.Name
		if p.typ != nil && p.typ.Optional {
			s += "?"
		}
		typ := p.Type
		if p.typ != nil {
			typ = p.typ.Raw
		}
		s += ": " + typ
		if p.HasDefault {
			data, _ := json.Marshal(p.Default)
			s += "=" + string(data)
		}
		params = append(params, s)
	}
	head := fmt.Sprintf("%s(%s)", f.Name, strings.Join(params, ", "))
	if f.Returns != "" {
		head += " -> " + f.Returns
	}
	lines := []string{head + "  -- " + f.Summary}
	for _, p := range f.Params {
		if p.Desc != "" {
			lines = append(lines, fmt.Sprintf("--   %s: %s", p.Name, p.Desc))
		}
	}
	for _, e := range f.Examples {
		lines = append(lines, "--   e.g. "+e)
	}
	return lines
}
