# Lua API

Zel gives the model one tool, `lua`: its input is Lua code. Shell commands,
files, background tasks and subagents are plain functions in it. The same
functions are available in `init.lua`, where you add instructions, tools, hooks
and prompt completions.

## Calls

- Every call has a fixed 30 s limit. Longer work goes to `tasks.spawn` and is
  checked in later calls.
- The session has one VM: globals and functions of one call are there in the
  next. It is reset when the session changes, init.lua reloads or a call times
  out, and the next output starts with `[lua: fresh VM ...]`. Keep what must
  outlive it in files.
- Output is what the code `print()`s plus the returned value (tables as JSON),
  never truncated; only a 1 MiB safety cap protects memory. Filter in Lua:
  `print(slice(read("big.md"), 100, 10))`.
- Errors raise `"fn: CODE: msg"` and can be caught with `pcall`. Codes: `EINVAL
  ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`. `sh` and
  `agent` return `ok = false` for an ordinary failure instead.
- Everything blocks except `tasks.spawn`. The agent wakes up only for user
  messages and short notifications such as `<notification>t1 finished</notification>`.
- The whole Lua standard library is open (`io`, `os` too). `require` looks in
  `~/.zel/lua/?.lua` and `.zel/lua/?.lua`.

## Functions

```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}
  -- bash -c in its own process group; out = stdout + stderr (8 MiB kept)
  -- o: timeout (s), cwd, env {K = V}, stdin, check (raise EFAIL when code ~= 0)
  -- also for HTTP (curl), JSON (jq) and waiting (sleep)

read(path) -> string                    -- files over 16 MiB: ELIMIT
slice(s, from, count?) -> string        -- lines from..from+count-1; from < 0 counts from the end
write(path, s, o?)                      -- o: append; parent folders are created
edit(path, old, new, o?) -> n           -- exact match; ENOMATCH, EAMBIG unless o.all
patch(path, {{old, new}, ...}) -> n     -- several exact edits, all or nothing; an edit may set all = true
list(dir) -> {name, dir, size}[]
glob(pattern) -> string[]               -- "internal/**/*.go"
grep(re, o?) -> {file, line, text}[]    -- o: path, glob, fixed, max (default 200)
exists(path) -> bool, "file"|"dir"|nil
tmp(name?) -> path                      -- a unique path in the session's tmp folder

tasks.spawn(name, fn, o?) -> id         -- fn runs in its own VM in the background, no time limit
  -- o: timeout (s). name shows in the chat while the task runs
  -- fn gets copies of what it captures (locals, tables, functions); globals of this VM are not there
  -- tasks stop when the session changes or init.lua reloads
tasks.list() -> {id, name, state}[]
tasks.get(id) -> {id, name, state, result?}       -- nil for an unknown id
tasks.wait(id, timeout?) -> result | nil, "timeout"
tasks.wait({id, ...}, timeout?) -> results[]      -- nil where a task did not finish
tasks.tail(id, n?) -> string            -- last lines it printed
tasks.stop(id)
  -- result: {ok, value, error, out, ms, timed_out}; value is what fn returned

agent{prompt, model?, effort?, provider?, cwd?, timeout?} -> {ok, answer, session, error}
  -- a subagent; blocks until it answers. In a call it must answer within the 30 s,
  -- so longer work runs in a task: tasks.spawn("review", function() return agent{prompt = "..."} end)
  -- subagents cannot start subagents (EDENIED)

view(path, ...)                         -- attach images to the next model request
log(...)                                -- a line in Tracing; the model does not see it
models() -> models, failures            -- {provider, model, context_window}[], {provider = error}
docs(topic?) -> string                  -- how zel works; without a topic, the list of topics
session.{id, cwd, root, parent, depth, model, effort}
session.rename(name)
```

`sh()` gets `ZEL_SESSION_ID` and `ZEL_DEPTH` in its environment.

## init.lua

Two files are read, in this order: `~/.zel/init.lua` (or `$ZEL_HOME/init.lua`)
and the nearest `.zel/init.lua` from the working directory upward. A file
returns a table, or a list of tables nested as deep as you like:

```lua
return {
  require("instructions"),   -- ~/.zel/lua/instructions.lua returns a table too
  require("web"),
  {
    instructions = {"Read code first.", function() return "git: " .. sh("git status --short").out end},
    tools = {
      server_sh = {
        desc = "run bash on mainserver via ssh. args: cmd, timeout? (default 8). returns like sh()",
        fn = function(a)
          return sh("ssh -o BatchMode=yes mainserver 'bash -s'", {stdin = a.cmd, timeout = a.timeout or 8})
        end,
      },
    },
    hooks = {
      turn_end = function(answer, running)
        if #running == 0 and sh("git status --porcelain").out ~= "" then return "commit your changes" end
      end,
    },
    completions = {plan = function() return read("PLAN.md") end},
  },
}
```

- `instructions`: text, a function that returns text, or a list of them. They
  go before the Lua section of the system prompt; functions run when the
  session starts or reloads. Wrap entries in `{..., only = "main"}` for the
  main agent only or `{..., only = "sub"}` for subagents only:
  `{"Reply like an SMS.", only = "main"}`, `{read("REVIEW.md"), only = "sub"}`.
- `tools`: `name = {desc, fn}`. The name is `name` or `ns.name` and cannot
  replace a built-in function. The agent calls it with one table of arguments,
  `server_sh{cmd = "docker ps"}`, and `fn` gets that table. `desc` goes into the
  system prompt as is: say what the tool does, its arguments and its result.
- `hooks` and `completions`: below.

Unknown keys are an error. A file that fails to load is skipped with a notice;
the other file still loads. Instructions of both files are joined, a tool or
completion of the project file replaces the global one of the same name (with a
warning), and all hooks run, global ones first.

A session reads the files once, when it starts or resumes, and keeps that
snapshot. Menu → Reload reads them again; during a turn it waits until the turn
ends, and a broken file keeps the previous snapshot.

Tools, hooks and completions run in one config VM, one at a time, with the
same functions as the agent.

## Hooks

| hook | gets | returns |
|---|---|---|
| `session_start()` | | text: a notification for the agent |
| `message(text, notice)` | a user message, or a notification when `notice` is true | new text for a user message; ignored for a notification |
| `turn_end(answer, running)` | the final answer and the tasks still running: `{id, name, ms}[]` | text: a notification, and the turn goes on (at most 3 times in a row) |
| `session_end(reason)` | `"quit"`, `"new"` or `"resume"` | |

Several `message` hooks chain: each gets the text the previous one returned. A
hook that fails is logged in Tracing and skipped. Each hook has 60 s. When a
session is left, running tasks are stopped first and `session_end` runs with
what is left of a 5 s budget.

## Prompt completions

`completions = {plan = function() return "..." end}` registers `#plan`. Typing
`#` in the composer lists the names; when you send a message with `#plan`, the
function runs and its text replaces the token. Names are 1 to 64 letters,
digits, `-` or `_`. A completion has 10 s; one that fails, or returns nothing,
keeps your draft and shows the reason. Ctrl+D cancels an expansion that is
still running. Unknown `#names` stay as typed.

## Examples

See [examples](examples/): a global file, a web search module and a project
file for a Go repository.
