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
One tool: lua (raw Lua; optional first line --! timeout=N). Fresh env per call; only state.* persists. Output = print() + return (JSON), 8 KB head/tail: filter in Lua.
Errors raise "fn: CODE: msg" (pcall); sh/web/tasks.run/agents.run/h:wait return ok=false instead. Everything blocks.
tasks.spawn/agents.spawn → Handle; unwaited results arrive later as <task-result>/<agent-result>.
## built-in
sh(cmd, o?) -> {ok,code,out,timed_out,ms}  -- o: timeout,cwd,env,stdin,check
fs.read(path, o?) -> string  -- o: from,lines
fs.write(path, s, o?)  -- o: append
fs.edit(path, old, new, o?) -> n  -- exact match; o: all
fs.list(dir) -> {name,dir,size}[]; fs.glob(pat) -> string[]; fs.exists(path) -> bool,kind; fs.tmp(name?) -> path
fs.grep(re, o?) -> {file,line,text}[]  -- o: path,glob,fixed,max
web.get(url, o?) / web.post(url, o?) / web.request{url,method,headers,body,json,timeout} -> {ok,status,headers,body,error}
json.encode(v, o?) / json.decode(s)
tasks.spawn(cmd, o?) -> H; tasks.run(cmd, o?) -> {ok,code,out,log,timed_out}; tasks.get(id); tasks.list()
agents.spawn{prompt,model,effort,timeout} -> H; agents.run{...} -> {ok,answer,session,error}; agents.get(id); agents.list()
H: h.id h:status() h:wait(timeout?) h:result() h:tail(n?) h:stop(); wait_all(hs, timeout?) -> results[]
state.get(k, scope?) / state.set(k, v, scope?) / state.list(prefix?, scope?)  -- scope: "session"|"project"; JSON values
view(path, ...); session.{id,cwd,root,depth}; log.info/notice(msg); sleep(s)`

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
	text := BuiltinPrompt + c.FunctionDocs()
	if own := c.Prompt(ctx); own != "" {
		return own + "\n\n" + text
	}
	return text
}

// BuiltinOnlyPrompt is the system prompt without any init.lua.
func BuiltinOnlyPrompt() string { return BuiltinPrompt }

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
