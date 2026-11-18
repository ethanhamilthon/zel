# zel 0.11 reference

Zel gives the model one tool, `lua`: its input is Lua code (Lua 5.1). Files,
shell commands, background tasks, questions to the user and images are
functions the code calls. You configure Zel in Lua too: `init.lua`, `zel.lua`
and `connect.json`.

## The lua tool

- One Lua VM per session: globals survive between calls. The VM is reset
  (fresh state) when a call times out, when the config reloads and when the
  session changes. The next output then starts with `[lua: fresh VM]`.
- A call has `settings.timeout` seconds (default 30), blocking calls such as
  `sh` or `task.wait` included. At the timeout the output ends with
  `[timeout after 30s; VM reset]`. Longer work goes to `task.spawn`.
- The output is what the code `print()`s, then the returned values (strings
  as they are, tables as JSON, several values one per line). A raised error
  appends `error: <msg>`.
- The standard library is fully open (`io`, `os`, ...). There is no sandbox:
  code runs with your user's permissions and network access.
- The text the agent gets is cut to `settings.output` bytes (default 8000;
  `0` turns the cut off). A longer text becomes its first and last half of
  the limit, with a marker between; the full text is written to
  `outputs/b<N>.txt` in the session folder:

  ```
  …first 4000 bytes…

  ======== [output cut: 5000 of 13000 bytes omitted]
  ======== full output: /Users/…/.zel/sessions/…/outputs/b12.txt
  ======== read it with slice(), grep() or sh("tail ...")

  …last 4000 bytes…
  ```

  Behind it, the runtime keeps at most 1 MiB of raw output (head and tail).
- Steering: when you type while a call runs, the call runs to its end. Calls
  of the same model response that have not started are returned as
  `[skipped: superseded by steering]`. Your text follows the tool results.
- `settings.show_code` (default `true`) shows the running code (its first
  line) in the working line. With `false` the line keeps only time, calls and
  queued input (`12s · 3 calls`).
- `settings.trace_hooks` (default `true`) shows the hook runs in Tracing.
  `Ctrl+H` in Tracing toggles them until zel quits.
- Tracing (Ctrl+T) lists the session by turn: a row per turn with its first
  message and duration, then its entries with a failure, duration and time.
  Tab cycles the view: All, Lua, Agent and Errors; Shift+Tab goes back.
  Typing searches. Enter opens an entry: a Lua call shows its highlighted code
  and its output, every entry its time and the model request it came from.
  Left and Right go to the previous and next entry; Esc goes back.
- A running background task has a line above the working line:
  `◐ 6s · test background`.
- The agent wakes up only for user messages and short notifications such as
  `<notification>t1 finished</notification>`.
- `sh` and tasks run with the session environment: `ZEL_SESSION_ID`,
  `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG`, ... and `settings.env`. API keys are
  never exported.

## API

These are globals in the agent VM and in task VMs. Config code gets the same
functions from `local zel = require("zel")` (`zel.sh`, `zel.read`,
`zel.task.spawn`, `zel.session.mode`, ...); there are no API globals in the
config VM.

Errors are raised as `"fn: CODE: msg"` and can be caught with `pcall`. Codes:
`EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`.

```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}
  -- bash -c in its own process group, in the session folder.
  -- out = stdout+stderr. o: timeout (s, capped by the time the call has left),
  -- cwd, env {K=V}, stdin, check (raise EFAIL when code ~= 0).
  -- Never raises otherwise. Subagents: sh("zel -p 'prompt'").
read(path) -> string          -- relative to the session folder; > 16 MiB: ELIMIT
read("img.png") -> nil        -- png, jpg, jpeg, gif, webp: attached to the next request
slice(s, from, count?) -> string   -- lines from..from+count-1; from < 0 counts from the end
write(path, s, o?)            -- o.append; parent folders are created
edit(path, old, new, o?) -> n -- exact text; ENOMATCH; EAMBIG unless o.all
edit(path, {{old, new, all?}, ...}) -> n   -- several edits, all or nothing
list(dir?) -> {name, dir, size}[]
glob(pattern) -> string[]     -- "internal/**/*.go"
grep(re, o?) -> {file, line, text}[]
  -- Go RE2; walks o.path (default: the folder), respects .gitignore, skips
  -- .git and binary files. o: path, glob, fixed (literal), max (default 200)
exists(path) -> bool, "file"|"dir"|nil

task.spawn(name, fn, o?) -> id   -- fn runs in its own fresh VM, no call timeout;
  -- o.timeout (s). fn gets COPIES of its upvalues; globals of the caller are
  -- not there. print() goes to the task log; the return value is the result.
  -- When nobody waits, a notification tells that it finished.
task.list() -> {id, name, state, ms}[]
task.get(id) -> {id, name, state, ms, result?, error?} | nil
task.wait(id | {ids}, timeout?) -> result | results[]   -- nil where not finished
task.tail(id, n?) -> string      -- the last lines it printed (default 20)
task.stop(id)

choice(question, {options...}) -> id   -- a picker in the UI, does not block; the
  -- answer arrives as a notification. Main session only (headless: EDENIED).
cd(dir?)            -- folder of the next calls; no argument = the start folder
docs(topic?) -> string
models() -> {provider, model, context_window}[], failures
session.id, session.name, session.cwd, session.root, session.mode ("main" | "headless"),
session.modes (with the --mode one), session.depth, session.model,
session.effort, session.provider
session.rename(name)
```

Functions from the config (`functions` in `init.lua`) are globals too, called
with one table: `server_sh{cmd = "docker ps"}`. Their arguments and results
are copied (data only).

## Config

Three files, all in `~/.config/zel/` or `$ZEL_HOME`, except `zel.lua`:

- `init.lua`: global. `--config FILE` replaces it; then `zel.lua` is not read.
- `zel.lua`: local, read only in the folder where the session started (no
  search upwards), without a trust prompt.
- `connect.json`: providers and the selected model, see `zel docs connect`.

`init.lua` and `zel.lua` return a table; an unknown key is an error:

```lua
local zel = require("zel")
return {
  settings = {
    compaction = 400000,   -- tokens, 0 = off
    timeout = 30,          -- seconds per lua call
    output = 8000,         -- bytes of one call the agent gets, 0 = no cut
    show_code = true,      -- the working line shows the running code
    show_prompt = false,   -- the chat shows the full #prompt text instead of #name
    trace_hooks = true,    -- Tracing opens with the hook runs shown
    editor = "nvim",       -- Ctrl+E opens the composer text in it
    colors = { accent = "#c3e88d", trace = { lua = "#a0a0a0" } },
    keys = { menu = "ctrl+k" },
    env = { EDITOR = "true" },
  },
  systems = { style = "Reply short." },
  prompts = { review = "Review the diff of the current branch." },
  functions = {
    server_sh = {
      description = "run bash on mainserver. args: cmd. returns like sh()",
      fn = function(a) return zel.sh("ssh mainserver 'bash -s'", {stdin = a.cmd}) end,
    },
  },
  hooks = {
    { events = {"idle"}, async = true, fn = function() end },
  },
  loads = { "lua/web.lua" },   -- relative to this file
}
```

- `systems` and `prompts`: name -> string. A name has 1 to 64 letters, digits,
  `-` or `_`. Dynamic text is computed when the file loads, with `zel.*`.
- `functions`: name -> `{description, fn}`. The description goes into the
  tool text. A name that collides with a built-in is an error.
- `hooks`: a list of `{events, fn, async?}`, see `zel docs hooks`.
- `loads`: files with the same shape (no `loads` inside), relative to the file
  that lists them. `package.path` gets the folder of each config file.
- There are no modes: check `zel.session.mode` or `zel.session.modes`.
- Merge order: `init.lua`, its loads, `zel.lua`, its loads. `settings` are
  deep-merged, later wins. The same name in `systems`, `prompts` or `functions`:
  the later one replaces the earlier with a warning. Systems are ordered by file,
  inside a file by name. Hooks are concatenated.
- `keys` binds actions to `ctrl+LETTER` or `alt+LETTER`; an empty string
  disables an action. Actions and defaults: `menu` ctrl+k, `models` ctrl+m,
  `sessions` ctrl+r, `new_session` ctrl+n, `info` ctrl+i, `trace` ctrl+t,
  `editor` ctrl+e, `stop` ctrl+d, `quit` ctrl+q, `redraw` ctrl+l. Russian
  letters on the same keys work too. Ctrl+M and Ctrl+I need an enhanced
  keyboard protocol, or bind those actions elsewhere.
- Loading has a 30 s budget. An error in any file makes the config invalid: at
  start Zel runs with defaults and shows the error; on a hot reload the
  previous config stays. Zel watches `init.lua`, `zel.lua`, every loaded file
  and `connect.json`. `zel config` shows the result.

## connect.json

Providers and the active model, in the config folder:

```json
{
  "provider": "openai",
  "model": "gpt-5",
  "effort": "medium",
  "providers": {
    "openai": {"base_url": "https://api.openai.com/v1", "api": "responses", "api_key": "$OPENAI_API_KEY"},
    "local": {"base_url": "http://localhost:11434/v1", "api": "chat", "api_key": ""}
  }
}
```

- `api` is `responses` (default) or `chat`. `effort` is one of `none minimal
  low medium high xhigh max`.
- A string with `$NAME` or `${NAME}` is expanded from the environment; `$$` is
  a literal `$`.
- The model menu writes `provider`, `model` and `effort` back; the rest of the
  file stays.

## Hooks

A hook is `{events = {...}, fn = function(event, data) ... end, async = false}`.
`fn` runs in the config VM, or for `async = true` in a fresh VM with copies of
its upvalues (like a task). A hook has 60 s. What it returns:

- nothing or `nil`: no effect.
- a string: the text for the event, see the table.
- `false, "reason"`: blocks an event that can be blocked.
- a raised error: recorded in Tracing and the diagnostics; the hook is skipped.

Async hooks start and are not waited for; their return values are ignored.
Hooks run one by one in config order; rewriting hooks chain. Every run is a
`hook` entry in Tracing.

| event | data | a returned string | `false, reason` |
|---|---|---|---|
| `session_created`, `session_resumed` | `{}` | | |
| `session_closed` | `{reason}` | | |
| `config_changed` | `{ok, error, file}` | | |
| `message` | `{text}` | the new message text | the message is dropped |
| `notification` | `{text}` (plain) | the new text; zel wraps it in `<notification>` | dropped |
| `turn_start` | `{text}` | | |
| `turn_end` | `{answer, running = {{id, name, ms}}}` | a notification; the turn goes on (at most 3 times in a row) | |
| `idle` | `{}` | | |
| `lua_before` | `{code}` | replaces the code | the call does not run; the agent sees `[blocked by a hook: reason]` |
| `lua_after` | `{code, out, ok, ms, timed_out}` | added on its own line to the output | |
| `compaction` | `{before, after}` | | |
| `model_changed` | `{provider, model, effort}` | | |
| `error` | `{kind, message}` | | |

Only `message`, `notification`, `turn_end`, `lua_before` and `lua_after` send
text to the agent; the strings of the other events are only kept in the
session diagnostics. `lua_after` hooks get the full output, before the cut.

```lua
hooks = {
  { events = {"lua_before"}, fn = function(event, data)
      if data.code:find("git push %-%-force") then return false, "no force push" end
    end },
  { events = {"turn_end"}, fn = function(event, data)
      local st = zel.sh("git status --short").out
      if st ~= "" then return "uncommitted changes, commit them" end
    end },
}
```

Zel writes no window title or progress to the terminal. A hook can, by writing
to `/dev/tty` (see the example `init.lua`).

## Prompts

`prompts = { name = "text" }` registers `#name`. In the composer, `#name`
completes like `@path`; when the message is sent, the token is replaced by

```
<prompt name="name">
text
</prompt>
```

Unknown names stay as typed. Prompts work only in the main session.

`settings.show_prompt` (default `false`): the chat shows each prompt block of
your messages as `#name`; the history keeps the full text, also in resumed
sessions. `true` shows the full text. Tracing always shows the full text.
A change applies at once.

## Colors

`settings.colors` sets every color of the TUI. Values are `#rrggbb`; a missing
key keeps the default. A bad value is a config error.

```lua
colors = {
  bg = "",                  -- empty: the terminal's own background
  accent = "#d97757",
  agent = "#d6d6d6",
  user = "#606060",
  user_highlight = "#8c8c8c",
  gray = "#929292",
  dark_gray = "#606060",
  error = "#e27878",
  trace = {
    system = "#606060",
    user = "#d97757",
    agent = "#ffffff",
    lua = "#a0a0a0",
    notice = "#808080",     -- notices, compaction, tasks
    output = "#404040",
    error = "#e27878",
  },
}
```

- `bg`: the background of the whole screen.
- `accent`: the composer rail, the `*` header over answers, completed `#name`
  and `@path` tokens, answered choices, menu markers, status icons, headings,
  inline code and links in answers, keywords in highlighted Lua.
- `agent`: the text of answers, menus, Info values, the choice title.
- `user`: your messages and choice answers, the composer text;
  `user_highlight`: the composer line under the cursor.
- `gray`: time and calls, the working line, inactive menu rows, Info keys, the
  composer placeholder, punctuation in Lua.
- `dark_gray`: the "context compacted" divider, the code in the working line,
  the name in a task line, hotkeys in menus, Tracing row numbers, Lua comments.
- `error`: failed turns, prompt errors, form errors.
- `trace.*`: the kind labels and bodies in Tracing, one per kind; `trace.lua`
  is also the plain text of highlighted Lua.

Strings and numbers in highlighted Lua keep fixed tones.

## Examples

See the examples topic: `init.lua`, `zel.lua`, `connect.json` and a module
loaded with `loads`.
