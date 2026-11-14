# zel → Lua: API (final draft)

0.5 — breaking release: the bash tool and ZEL.md are removed.

Decided: Lua only (shell — only `sh()`), a persistent main VM per session (globals live between calls) + explicit `state`, everything blocks except
`tasks.spawn`/`agents.spawn`, subagents (`agents.*`) are core to 0.5, own functions are registered in `init.lua` and get into the system prompt by
themselves, full trust (no sandbox, trust prompts or policies). Runtime: gopher-lua (pure Go, `SetContext` for timeouts), the whole stdlib is open (`os`, `io` too), `package.path` += `~/.zel/lua/?.lua;.zel/lua/?.lua`.

## 1. Tool and call model
The model has exactly one tool, `lua`; the payload is raw Lua code (transport and options — below).
- The VM lives between calls (0.5.2): globals, functions and the `require` cache stay. The VM is reset on `new`, `resume`, reload
  and after a call timeout; the next output starts once with `[lua: fresh VM, <reason>: ...]`. `state.*` survives a reset.
- Four VM roles, isolated from each other: config VM (`init.lua`, hooks, plugin function bodies), main VM (model calls),
  task VM (fresh for each `tasks.spawn(fn)`, its own goroutine), subagent VM (its own main VM; `agents.*` is forbidden there → `EDENIED`).
- Output = `print()` + `return` (tables → compact JSON), not truncated (0.5.2; only a 1 MiB safety cap). Filter in Lua:
  `local a = fs.read("bigfile.md"); print(slice(a, 100, 10))`. `sh().out` inside Lua is complete (8 MiB cap).
- The call timeout is fixed: 30 s, not configurable (0.5.2; 10 s in 0.5.1, earlier 120 s and `--! timeout=N`). One deadline covers the whole script and nested blocking calls: `sh{timeout}` and `tasks.wait`/`agents.wait` are cut to what is left. Long work goes to `tasks.spawn`/`agents.spawn`, checked in later calls; at the deadline infinite loops die, background jobs live on.
- Only tasks and subagents run in the background; they outlive the end of a call. A result nobody collected arrives as a
  notification `<notification>t1 finished</notification>` — only a short text; the model fetches details itself (`tasks.result(id)`).
  The agent wakes up only for a user message or a notification; several in a row arrive as one message.
- The user writes during a call → the message is queued and reaches the model after the call ends. The call is not interrupted:
  there are no background tasks here, the agent literally waits for the result. Esc → cancel (`ECANCELED`); a spawned background job keeps running.
- The CLI `zel -p` stays for humans and is not in the prompt (`zel tasks` was removed in 0.5.2).

### Tool input format (universal)
- One canon: a call is Lua code text. There are no options; a first line `--!` is an ordinary Lua comment and does not change the deadline.
- The provider adapter picks the transport by capability: a free-form custom tool (raw text, no escaping) where available (Responses),
  otherwise a JSON function `{code: string}`. Both normalize into one `Call{code, opts}` before the runtime.
- A new provider is only a transport adapter; the prompt, hooks (`args = {code}`), logs and sessions do not depend on the format.

### Error model
- A normal failure is a value: `sh`, `web.*`, `agents.run` return `ok=false` (+`code`/`status`/`error`).
- Everything else (bad arguments, missing file, limits, `check=true`, call timeout) is a Lua `error()` with a string like
  `"<fn>: <CODE>: <msg>"`, caught by `pcall`. Codes: `EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`.
  Example: `local ok, err = pcall(fs.read, "x")` → `false, "fs.read: ENOENT: x"`; `err:find(": ENOENT:")`.
- An unhandled error → in the tool output: what was printed before it + `error: <msg>` + a short traceback, flag is_error.

## 2. Host API (full list)
```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}  -- bash -c, its own process group; out = stdout+stderr; timeout → 124
  -- o: timeout (s), cwd, env {K=V}, stdin, check (error EFAIL when code~=0)
-- files (paths relative to session.cwd)
fs.read(path) -> string                -- ENOENT; files > 16 MiB → ELIMIT
slice(s, from, count?) -> string       -- lines from..from+count-1 (1-based); from<0 — counted from the end
fs.write(path, s, o?) -> nil           -- o: append; parent folders are created
fs.edit(path, old, new, o?) -> n       -- o: all; ENOMATCH (0 matches), EAMBIG (>1 without all)
fs.patch(path, {{old, new}, ...}) -> n -- several exact edits atomically: all or none; {old=, new=, all=} also works
fs.list(dir) -> {name, dir, size}[]
fs.glob(pattern) -> string[]           -- "internal/**/*.go"
fs.grep(re, o?) -> {file, line, text}[] -- o: path, glob, fixed, max (default 200); rg, otherwise Go regexp
fs.exists(path) -> boolean, "file"|"dir"|nil
fs.tmp(name?) -> string                -- a unique path in the session tmp
-- network
web.request{url, method?, headers?, body?, json?, timeout?} -> {ok, status, headers, body, error?}
web.get(url, o?) / web.post(url, o?)   -- sugar; ok = 2xx; network error → ok=false, status=0
json.encode(v, o?) -> string; json.decode(s) -> value   -- o: indent; decode: EINVAL
-- background: tasks and agents are two independent namespaces; there is no shared Handle, a job is a string ID ("t1", "a1")
tasks.spawn(fn, o?) -> id              -- a Lua function in the background in its own VM, no 30 s limit; o: name, timeout (s)
  -- capture by copy: number/string/boolean upvalues, tables (deep) and functions (recursive) are copied into the new VM;
  -- userdata/coroutines → EINVAL at spawn. Changes inside are not visible outside; there are no main-VM globals.
  -- print() of a task goes to its log (tasks.tail), return — to result.value. Cancelled on quit/new/reload.
-- subagents — a full child Runtime in the same process (own session, own context), without `zel -p`
agents.spawn{prompt, model?, effort?, provider?, cwd?, timeout?} -> id
agents.run{...} -> AgentResult         -- spawn + wait
-- the same in tasks.* and agents.* (an ID from the other namespace → ENOENT)
X.list() -> rows                       -- tasks: {id, name, state}; agents: {id, model, state, prompt}
X.get(id) -> row|nil
X.status(id) -> "running"|"done"|"failed"|"stopped"|"timeout"
X.wait(id, timeout?) -> result | nil, "timeout"  -- a wait timeout does NOT kill the background job; the call deadline (30 s) → ETIMEOUT
X.wait_all(ids, timeout?) -> results[] -- by index; nil for those that did not finish in time; one shared deadline
X.result(id) -> result|nil             -- without blocking
X.tail(id, n?) -> string               -- last n lines of the log (for an agent — steps and the final answer)
X.stop(id) -> nil                      -- cancels the context (task and agent)
-- TaskResult  = {id, ok, value, error?, out, timed_out, ms}   value = the function's return, out = the last 64 KiB printed
-- AgentResult = {id, ok, answer, session, error?, ms}
-- A result is delivered exactly once: either wait/run takes it, or a notification <notification>t1 finished</notification> arrives.
-- state between calls
state.get(key, scope?) -> value|nil
state.set(key, value, scope?) -> nil   -- value=nil deletes
state.list(prefix?, scope?) -> string[]  -- keys, sorted
-- other
view(path, ...) -> nil                 -- pictures go into the next request
session.{id, cwd, root, parent, depth, model, effort}; session.rename(name)
session.models() -> models, failures  -- models: {provider, model, context_window}[] (the model menu's list; context_window 0 when unknown); failures: {provider = error}; listed once per session, failures cached with it; listed again after a provider change
log.info/warn(msg) -- Tracing only; log.notice(msg) -- a UI notification, not sent to the model; sleep(sec)
```

### state
- scope `"session"` (default): lives with the session, survives compaction, resume and a zel restart.
- scope `"project"`: key = `session.root` (git root, otherwise cwd), shared by all sessions and subagents of the project.
  A subagent does not inherit session state — pass it via the prompt or project scope.
- Values are JSON-compatible (no functions/cycles). Limits: key ≤ 256 B, value ≤ 64 KiB, scope ≤ 1 MiB → otherwise `ELIMIT`.
- Stored in `zel.db`; a write is atomic per key; the last writer wins.

## 3. Plugin API (`init.lua`)
`init.lua` runs once per session in a long-lived config VM: that is where hooks and plugin function bodies live. Agent code calls them
through a bridge; arguments and results are copied as JSON values. The whole host API is available in init.lua.
```lua
local zel = require("zel")
zel.instructions(text | {file = "AGENTS.md"})   -- static prompt text
zel.prompt(function(ctx) return "..." end)       -- dynamic part, computed at session start and on Reload
zel.defaults{provider=, model=, effort=}  -- CLI flags above
zel.fn{name, summary, params?, returns?, examples?, hidden?, run}  -- a function for the agent
zel.on(event, function(ev, ctx) end)             -- a hook
```

### Re-reading: snapshot + Reload
- Init.lua layers are read once at session start (and on resume) → a snapshot: instructions, prompt, defaults, fn, hooks.
  Editing the file does not affect a live session.
- Reload (menu Ctrl+K → Reload) — a new config VM, layers read again, the prompt is rebuilt. During a turn it applies after the turn ends. A load error → the old snapshot stays + `log.notice`.

### zel.fn
```lua
zel.fn{
  name     = "web.search",            -- "ns.name" or "name"; built-in names cannot be overridden
  summary  = "DuckDuckGo search via ddgs",
  params   = {{"q", "string"}, {"n", "integer?", "max results", default = 5}},  -- positional, in order
  returns  = "{title,url,body}[]",
  examples = {'web.search("gopher-lua context", 3)'},
  run      = function(q, n) ... end,  -- arguments are already checked, default filled in
}
```
Types: `string integer number boolean table any`, `T[]`, `{k:T}`, `"a"|"b"`, `?` = optional. The host checks arguments
before the call (`web.search: EINVAL: arg #1 q: expected string, got nil`), not the result. An `error()` from `run` goes to the agent
(without a code — prefix `<name>: EFAIL:`). `hidden = true` — it exists but is not rendered into the prompt.

### Prompt generation
Form: a compact signature + `--` doc lines (2–3 times cheaper than LuaLS `---@param`, the model reads it as Lua):
`name(p: T, p2?: T=default) -> R  -- summary`, then `--   p: desc` (only if present) and `--   e.g. …` for an example.
Groups: `## built-in` (fixed text from the binary), then `## <layer> (path)`. Replaces `bash.md`, rebuilt with the prompt.

## 4. Hooks
| event | ev | return |
|---|---|---|
| `session_start` | `{}` | nil or a string / `{notify = "text"}` → notification (wakes the agent) |
| `session_end` | `{reason}` (`quit`/`new`/`resume`) | nothing; 5 s for all hooks together |
| `message` | `{text, notice?}` | nil or a new user text string; `notice = true` marks a notification, whose text cannot be rewritten |
| `turn_end` | `{answer}` | nil or a string / `{notify = "text"}` → notification, the turn continues (at most 3 times in a row) |
| `subagent_start` | `{id, prompt, model, effort, provider, cwd, depth}` | nothing; runs in the background when the subagent begins |
| `subagent_end` | `{id, status, ok, answer, error?, session, model, effort, ms}` | nothing; once per subagent: `done`/`failed`/`stopped`/`timeout` |

Since 0.5.2 there are exactly four hooks: `before_call`, `after_call` and `bg_done` are removed (`zel.on` with them is a layer load error).
Everything hooks report to the agent arrives in one format `<notification>text</notification>`.
`ctx = {session, layer}`. An error in a hook → a warning in Tracing, counted as nil.

## 5. Layers and merging
Order: `~/.zel/init.lua` → nearest `.zel/init.lua` going up from cwd → `--config FILE` (`.lua` only).
- `instructions`/`prompt`: concatenated in order.
- `defaults`: a later one overrides; CLI flags override everything.
- `fn`: same name — the project one overrides the global one (warning in Tracing); built-in names are never overridden.
- hooks: all run, global ones first; `args`/`result` are passed along the chain; the first `deny` stops it.
- layer load error: the layer is skipped, `log.notice` in the UI, the session starts.

## 6. Examples

### 6.1 Global `~/.zel/init.lua` — instructions + defaults
```lua
local zel = require("zel")
zel.defaults{model = "sonnet-5.5", effort = "medium"}
zel.instructions [[
Read code first. Work read-only unless the request needs edits. Verify before the final answer.
Reply like an SMS: short, lowercase, user's language, no trailing dot.
Subagents: cheap work → haiku-5.5, real code → sonnet-5.5, review/hard → opus-5.5.
]]
zel.prompt(function() return "env: " .. sh("uname -srm").out .. "today: " .. os.date("%Y-%m-%d") end)
zel.on("session_start", function() sh("mkdir -p ~/.zel/cache") end)
```

### 6.2 Global plugin — documented `web.search` / `web.fetch`
The core has none of them (there is only raw `web.request/get/post`): search and fetch are a plugin in `~/.zel/init.lua`, like this one.
```lua
local zel = require("zel")
zel.fn{
  name = "web.search", summary = "DuckDuckGo search via ddgs",
  params = {{"q", "string"}, {"n", "integer?", default = 5}},
  returns = "{title,url,body}[]",
  examples = {'for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end'},
  run = function(q, n)
    local out = fs.tmp("ddgs.json")
    sh(("ddgs text -q %q -m %d -nc -o %s"):format(q, n, out), {check = true, timeout = 30})
    local hits = {}
    for i, h in ipairs(json.decode(fs.read(out))) do hits[i] = {title = h.title, url = h.href, body = h.body} end
    return hits
  end,
}
zel.fn{
  name = "web.fetch", summary = "page as markdown (trafilatura, fallback crwl)",
  params = {{"url", "string"}, {"max", "integer?", "cut to N bytes", default = 20000}},
  returns = "string",
  run = function(url, max)
    local r = sh(("trafilatura -u %q --markdown --no-comments --links"):format(url), {timeout = 40})
    if not r.ok or r.out == "" then r = sh(("crwl crawl %q -o markdown"):format(url), {check = true}) end
    return r.out:sub(1, max)
  end,
}
```
The generated system prompt section (in full, with the built-in part):
```text
# Lua
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
--   stopped when the session changes (new/resume) or init.lua reloads
agents.spawn{prompt,model,effort,provider,cwd,timeout} -> id; agents.run{...} -> {ok,answer,session,error}
tasks.* and agents.* both have: get(id) list() status(id) wait(id, timeout?) wait_all(ids, timeout?) result(id) tail(id, n?) stop(id)
state.get(k, scope?) / state.set(k, v, scope?) / state.list(prefix?, scope?)  -- scope: "session"|"project"; JSON values
view(path, ...); session.{id,cwd,root,parent,depth,model,effort}; session.rename(name); log.info/warn/notice(msg); sleep(s)
session.models() -> models, failures  -- models: {provider,model,context_window}[] (the model menu's list); failures: {provider: error}; listed once per session, failures cached with it; listed again after a provider change
## global (~/.zel/init.lua)
web.search(q: string, n?: integer=5) -> {title,url,body}[]  -- DuckDuckGo search via ddgs
--   e.g. for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end
web.fetch(url: string, max?: integer=20000) -> string  -- page as markdown (trafilatura, fallback crwl)
--   max: cut to N bytes
```
The whole API takes about 550 tokens; each plugin function is 1–3 lines.

### 6.3 Project `repos/zel/.zel/init.lua` — instructions, a project function, hooks, state
```lua
-- Copy to ./.zel/init.lua of a Go project: instructions, a project function, hooks, state.
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

-- A Lua call lives 30 s, so the slow checks run as one background task.
zel.fn{
  name = "zel.check", summary = "start gofmt + vet + race tests in the background, returns the task id",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "string (task id: read it with tasks.wait(id, 20) or wait for its notification)",
  examples = {'local id = zel.check("./internal/agent/...")'},
  run = function(pkg)
    pkg = pkg or "./..."
    local id = tasks.spawn(function()
      local r = sh('test -z "$(gofmt -l .)" && go vet ' .. pkg .. ' && go test -race -count=1 ' .. pkg)
      print(r.out)
      return r.ok
    end, {name = "check", timeout = 600})
    state.set("last_check", {id = id, at = os.time()}, "project")
    return id
  end,
}

zel.on("turn_end", function()
  if sh("git status --porcelain").out ~= "" then
    return "uncommitted changes: run zel.check() and commit per AGENTS.md"
  end
end)
```
It adds to the prompt:
```text
## project (repos/zel/.zel/init.lua)
zel.check(pkg?: string="./...") -> string (task id: read it with tasks.wait(id, 20) or wait for its notification)  -- start gofmt + vet + race tests in the background, returns the task id
--   pkg: go package pattern
--   e.g. local id = zel.check("./internal/agent/...")
```

## 7. Implementation notes (deviations and details of 0.5.0)
- Cancelling: the TUI has no Esc-cancel for a running turn; **Ctrl+D** (stop the active turn) cancels the running
  call (`ECANCELED`). Background tasks and agents keep running, as designed.
- `turn_end` may continue a turn at most 3 times (returned text → notification), so a hook that always answers cannot loop forever.
- A queued user message is also passed through `message` hooks when it is accepted into the history.
- `agents.spawn` runs a child agent inside the application (its own session; no `zel -p` process, no report file, no
  socket). Subagents cannot start subagents (0.5.2; `agents.*` raises `EDENIED` there, `tasks.*` works). The child is visible in the task panel like any task, and closing the parent
  cancels it. `sh()` still gets `ZEL_SESSION_ID` and `ZEL_DEPTH`.
- Compatibility (0.5.1): handles and the global `wait_all` are gone (use `tasks.wait(id)` / `agents.wait_all(ids)`);
  `--! timeout=N`, the timeout setting and `zel.defaults{timeout=}` are removed; `zel view` and `zel tasks run|stop`
  are removed (use `view()` and `tasks.*`).
- Compatibility (0.5.2): `tasks.spawn(cmd)`, `tasks.run` and the `zel tasks` CLI are removed (wrap shell work in
  `tasks.spawn(function() return sh(cmd) end)`); `fs.read` lost `{from, lines}` (use `slice`); the output-limit
  setting and `zel.defaults{output_limit=}` are removed; hooks are `session_start`, `session_end`, `message`, `turn_end`.
- 0.5.3: `session.models()` is new. A turn that fails or is stopped (Ctrl+D) closes in the chat with its wall-clock time, steps and
  the error text or `stopped by user`; that closing message is kept in the saved history but never sent to the model.
- Reload with a broken layer keeps the previous snapshot completely (not layer by layer) and raises a notice;
  the first load at session start skips only the broken layer.
- Tracing kind of a call is `lua` (search `:lua text`). The lua output is not truncated (1 MiB safety cap) and the 30 s call timeout is fixed (0.5.2).
- `ZEL.md` is neither read nor migrated; the repository itself uses `.zel/init.lua` + `AGENTS.md`.
- Ready-made examples (global config, web plugin, project config) are in `docs/examples/` and covered by tests.
