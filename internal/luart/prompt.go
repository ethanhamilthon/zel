package luart

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// builtinHead is the fixed part of the system prompt before the host API.
const builtinHead = `# Lua
One tool: lua (raw Lua). Each call has a fixed 30 s timeout that cannot be changed: run longer work with tasks.spawn and check it in later calls.
The VM persists between calls: globals and functions stay until the session changes, init.lua reloads or a call times out; the next output then starts with "[lua: fresh VM ...]". Keep what must outlive it in files.
Output = print() + return (JSON), never truncated: filter in Lua and print only what you need, e.g. local a = read("bigfile.md"); print(slice(a, 100, 10)).
Errors raise "fn: CODE: msg" (pcall); sh and agent return ok=false instead. Everything blocks except tasks.spawn.
You wake up only for user messages and notifications like <notification>t1 finished</notification>; they are short, fetch details yourself (tasks.get(id)).`

// builtinAPI lists the host functions.
const builtinAPI = `## built-in
sh(cmd, o?) -> {ok,code,out,timed_out,ms}  -- o: timeout,cwd,env,stdin,check; also for http (curl), json (jq) and waiting (sleep)
read(path) -> string; slice(s, from, count?) -> string  -- lines from..from+count-1; from<0 counts from the end
write(path, s, o?)  -- o: append
edit(path, old, new, o?) -> n  -- exact match; o: all
patch(path, {{old, new}, ...}) -> n  -- several exact edits, all or nothing; an edit may set all=true
list(dir) -> {name,dir,size}[]; glob(pat) -> string[]; exists(path) -> bool,kind; tmp(name?) -> path
grep(re, o?) -> {file,line,text}[]  -- o: path,glob,fixed,max
tasks.spawn(name, fn, o?) -> id  -- fn runs in its own VM in the background, no time limit; name shows in the UI; o: timeout
--   fn gets copies of what it captures (locals, tables, functions); changes stay inside and globals of this VM are not there
--   stopped when the session changes (new/resume) or init.lua reloads
tasks.list() -> {id,name,state}[]; tasks.get(id) -> {id,name,state,result?}; tasks.tail(id, n?) -> string; tasks.stop(id)
tasks.wait(id, timeout?) -> result | nil,"timeout"; tasks.wait({id, ...}, timeout?) -> results[] (nil where not finished)
--   result: {ok,value,error,out,ms,timed_out}; value = fn's return value, out = what it printed
agent{prompt,model?,effort?,provider?,cwd?,timeout?} -> {ok,answer,session,error}  -- a subagent; blocks until it answers
--   in the background: tasks.spawn("review", function() return agent{prompt = "..."} end)
view(path, ...)  -- attach images to the next request; log(msg)  -- write to Tracing only
session.{id,cwd,root,parent,depth,model,effort}; session.rename(name)
models() -> models, failures  -- models: {provider,model,context_window}[] (the model menu's list); failures: {provider: error}
docs(topic?) -> string  -- how zel works (Lua API, init.lua, hooks, tasks); docs() lists the topics, call it when unsure how something works
A #name in a user message arrives already expanded.`

// BuiltinPrompt is the fixed part of the system prompt, for the main agent.
const BuiltinPrompt = builtinHead + "\n" + builtinAPI

// Builtin is BuiltinPrompt for the given agent depth: subagents (depth > 0)
// cannot start agents, so their prompt leaves agent{} out.
func Builtin(depth int) string {
	if depth == 0 {
		return BuiltinPrompt
	}
	r := strings.NewReplacer(
		"agent{prompt,model?,effort?,provider?,cwd?,timeout?} -> {ok,answer,session,error}  -- a subagent; blocks until it answers\n", "",
		"--   in the background: tasks.spawn(\"review\", function() return agent{prompt = \"...\"} end)\n", "",
		"sh and agent return", "sh returns",
	)
	return r.Replace(BuiltinPrompt) + "\nYou are a subagent: agent() is not available."
}

// Prompt renders the instructions of every layer, in layer order, leaving out
// the ones for the other kind of agent (only = "main" or "sub").
func (c *Config) Prompt(ctx context.Context) string {
	var parts []string
	who := "main"
	if c.env.Info().Depth > 0 {
		who = "sub"
	}
	for _, l := range c.layers { // final once LoadConfig returns
		for _, in := range l.instructions {
			if in.only != "" && in.only != who {
				continue // not for this agent; its function does not run either
			}
			text := in.text
			if in.fn != nil {
				text = c.renderPrompt(ctx, l, in.fn)
			}
			if t := strings.TrimSpace(text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func (c *Config) renderPrompt(ctx context.Context, l *Layer, fn *lua.LFunction) string {
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var text string
	err := c.call1(pctx, fn, nil, func(L *lua.LState, res lua.LValue) error {
		if res != lua.LNil {
			text = luaString(L, res)
		}
		return nil
	})
	if err != nil {
		if !errorsIsStop(err) {
			c.env.Notice(fmt.Sprintf("%s: instructions failed: %s", l.Name, errorMessage(err)))
		}
		return ""
	}
	return text
}

// ToolDocs renders the tools grouped by layer ("## tools (path)").
func (c *Config) ToolDocs() string {
	home, _ := os.UserHomeDir()
	var b strings.Builder
	for _, l := range c.layers {
		var lines []string
		for _, t := range l.tools {
			if c.tools[t.Name] == t {
				lines = append(lines, renderTool(t)...)
			}
		}
		if len(lines) == 0 {
			continue
		}
		path := l.Path
		if home != "" && strings.HasPrefix(path, home+"/") {
			path = "~" + strings.TrimPrefix(path, home)
		}
		fmt.Fprintf(&b, "\n## tools (%s)\n%s", path, strings.Join(lines, "\n"))
	}
	return b.String()
}

// SystemPrompt is the whole prompt: layer instructions, then the Lua API and the tools.
func (c *Config) SystemPrompt(ctx context.Context) string {
	text := Builtin(c.env.Info().Depth) + c.ToolDocs()
	if own := c.Prompt(ctx); own != "" {
		return own + "\n\n" + text
	}
	return text
}

// renderTool is name{...} and the first line of desc, then the other lines as comments.
func renderTool(t *Tool) []string {
	desc := strings.Split(t.Desc, "\n")
	lines := []string{t.Name + "{...}  -- " + strings.TrimSpace(desc[0])}
	for _, d := range desc[1:] {
		if d = strings.TrimSpace(d); d != "" {
			lines = append(lines, "--   "+d)
		}
	}
	return lines
}
