# zel 0.12 reference

Zel gives the model one tool, `lua`: its input is Lua 5.1 code. The session VM
has the Lua 5.1 standard library plus the `functions` from your config — no
built-in host functions. Default functions live as plain Lua in
`~/.config/zel/defaults/` (`require("defaults")`) and talk to Zel through the
`zel` CLI (`exec`, `steer`, `status`, `choice`, `print`, `session`, `rename`).

## The lua tool

- One Lua VM per session: globals survive between calls. The VM is reset
  (fresh state) when a call times out, when the config reloads, and when the
  session changes. The next output then starts with `[lua: fresh VM]`.
- There is no host API in the VM (`no print`, no `require("zel")`, no
  `task.*`): only Lua 5.1 stdlib (`io`, `os`, `string`, `table`, `math`, ...)
  and whatever your config puts in `functions`.
- Output of a call is the values of `return ...`, or the values returned by the
  last statement when it is a function call, or the value of an expression.
  Strings come back as they are, tables as JSON, several values one per line,
  `nil` results give no line. A raised error appends `error: <msg>`.
- Terminal isolation: `io.write`, `io.stdout` and `io.stderr` go to a
  discarded sink; children of `io.popen` and `os.execute` get no TTY (stdin
  `/dev/null` for `popen "r"`, stderr `/dev/null`, `os.execute` stdout
  `/dev/null`), start in their own process group in the session start folder
  with the session environment, and are killed when the call times out, is
  cancelled, or the VM resets.
- A call has `settings.timeout` seconds (default 30). At the timeout the output
  ends with `[timeout after 30s; VM reset]`.
- Background work: start a shell command with `&` and redirect its output, track
  it with `zel status new` / `zel status -i ID --done`, and report back with
  `zel steer`:

  ```sh
  id=$(zel status new "make test")
  (make test > /tmp/test.log 2>&1; zel status -i "$id" --done; zel steer "tests done: $(tail -3 /tmp/test.log)") &
  ```
- The text the agent gets is cut to `settings.output` bytes (default 8000;
  `0` turns the cut off). A longer text becomes its first and last half of
  the limit, with a marker between; the full text is written to
  `outputs/b<N>.txt` in the session folder.
- Steering: when you type while a call runs, the call runs to its end. Calls
  of the same model response that have not started are returned as
  `[skipped: superseded by steering]`. Your text follows the tool results.
- Working line: shows elapsed time, the `zel status` text of the running call
  (or the code's first line when `settings.show_code` is `true`), calls, and
  queued input. `zel status new` indicators appear above the working line as
  `◐ 6s · text`. `zel print` lines appear in `dark_gray` with no header.
- `settings.trace_hooks` (default `true`) shows the hook runs in Tracing.
  `Ctrl+H` in Tracing toggles them until zel quits.
- Tracing (`Ctrl+T`) lists the session by turn. `Tab` (`Shift+Tab` back) cycles
  All, Lua, Agent and Errors; typing searches; `Enter` opens an entry;
  `Left`/`Right` step within the view; `Esc` goes back.
- Environment in the VM and subprocesses: `ZEL_SESSION_ID`, `ZEL_SOCKET`,
  `ZEL_TOKEN`, `ZEL_MODE` (`main` or `headless`), `ZEL_DEPTH`, `ZEL_CONFIGS`,
  `ZEL_NO_DEFAULT_CONFIGS`, plus `settings.env`. API keys are never exported.

## API

### The `defaults` module

`~/.config/zel/defaults/` holds the default Lua module (embedded in `zel`; reset
anytime with `zel defaults --reset`). `require("defaults")` returns a table of
`{ description, fn }` entries with a `__call` metatable, so `d.sh("git status")`
works in config/hooks/commands and `functions = { sh = d.sh }` installs `fn`
as a global in the session VM.

Errors are raised as `"fn: CODE: msg"` (catchable with `pcall`). Codes:
`EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`.
Paths are relative to the defaults' `cwd` (initially the session start folder);
`~` is expanded. File/shell calls report their progress via `zel status`.

```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}
  -- runs `zel exec --timeout N --cwd D [--env K=V]... -- bash -c CMD`.
  -- o: timeout (s), cwd, env {K=V}, stdin, check (raise EFAIL on code ~= 0).
read(path) -> string          -- > 16 MiB: ELIMIT
read("img.png") -> nil        -- png, jpg, jpeg, gif, webp: `zel steer --file PATH`
slice(s, from, count?) -> string   -- lines from..from+count-1; from < 0 counts from the end
write(path, s, o?)            -- o.append; creates parent folders
edit(path, old, new, o?) -> n -- exact text; ENOMATCH; EAMBIG unless o.all
edit(path, {{old, new, all?}, ...}) -> n   -- several edits, all or nothing
list(dir?) -> {name, dir, size}[]
glob(pattern) -> string[]     -- `**` supported; uses `rg` when on PATH, else `find`
grep(re, o?) -> {file, line, text}[]
  -- uses `rg` when on PATH (respects .gitignore), else `find`/`grep -rnE`.
  -- o: path, glob, fixed (literal), max (default 200)
exists(path) -> bool, "file"|"dir"|nil
cd(dir?)                      -- folder of the next calls; no argument = start folder
choice(question, {options...}) -> id   -- `zel choice` (non-blocking; main mode only)
docs(topic?) -> string        -- `zel docs`
models() -> list, failures    -- `zel models --json`
session() -> table            -- `zel session --json`
rename(name)                  -- `zel rename`
```

Custom functions in `functions` are installed directly as globals in the session
VM. When the session VM starts, all `fn` values from `functions` are copied from
the config VM in one pass, so shared upvalues (such as `defaults`' `cwd`) stay
shared.

### The `zel` CLI

Subcommands act on `$ZEL_SESSION_ID` (`-s ID` selects another live session):

- `zel exec [--timeout S] [--cwd DIR] [--env K=V]... -- CMD ARGS...`: runs CMD
  in a new process group (no socket), stdin passed through, stdout+stderr
  merged, prints `{"ok","code","out","timed_out","ms"}`. On timeout kills the
  group; waits at most 100 ms after exit for pipes so `&` jobs do not block.
- `zel steer [-s ID] [--file PATH]... [TEXT|-]`: sends steering text/images.
  During a turn it follows the tool results; when idle it starts a new turn.
- `zel status [-s ID] TEXT`: sets the working line status for the running call.
- `zel status new TEXT`: creates an indicator line above the working line and
  prints its ID; `zel status -i ID TEXT` updates it; `zel status -i ID --done`
  removes it.
- `zel choice [-s ID] QUESTION OPTION...`: shows the picker and prints its ID;
  the answer arrives as a `<notification>`. Headless: exit 6 (`EDENIED`).
- `zel print [-s ID] TEXT|-`: shows a `dark_gray` line in the chat (saved in
  history, never sent to the model, shown in Tracing).
- `zel session [-s ID] --json`: `{id, name, cwd, root, mode, modes, depth, model, effort, provider}`.
- `zel rename [-s ID] NAME`.
- `zel defaults --reset`: restores `~/.config/zel/defaults/` from the binary.

## Config

Config files are loaded in order:
1. `~/.config/zel/init.lua` (or `$ZEL_HOME/init.lua`), skipped if missing.
2. `./zel.lua` of the start folder, skipped if missing.
3. Each `--config FILE` (repeatable), error if missing.

`--no-default-configs` drops the two defaults. A `zel -p` child inherits
`ZEL_CONFIGS` (explicit `--config` paths) and `ZEL_NO_DEFAULT_CONFIGS=1`, and
resolves defaults itself (using its own `--cwd` for `./zel.lua`).

First run creates `init.lua` (wiring `require("defaults")`), `defaults/`,
`connect.json`, and `.gitignore`, without overwriting existing files.

Each file returns a table with any of: `settings`, `systems`, `prompts`,
`functions`, `hooks`, `commands`, `loads` (unknown key = error).

```lua
local d = require("defaults")
return {
  settings = {
    compaction = 400000,   -- tokens, 0 = off
    timeout = 30,          -- seconds per lua call
    output = 8000,         -- bytes of one call the agent gets, 0 = no cut
    show_code = true,      -- working line falls back to code when no zel status
    show_prompt = false,   -- chat shows #name instead of expanded prompt text
    trace_hooks = true,    -- Tracing opens with hook runs shown
    editor = "nvim",       -- Ctrl+E opens composer text in it
    colors = { accent = "#c3e88d", trace = { lua = "#a0a0a0" } },
    keys = { menu = "ctrl+k" },
    env = { EDITOR = "true" },
  },
  systems = { style = "Reply short." },
  prompts = { review = "Review the diff of the current branch." },
  functions = {
    sh = d.sh,
    read = d.read,
    server_sh = {
      description = "`server_sh(cmd) -> {ok, code, out}` - bash on mainserver",
      fn = function(cmd) return d.sh("ssh mainserver 'bash -s'", {stdin = cmd}) end,
    },
  },
  hooks = {
    { events = {"idle"}, async = true, fn = function() end },
  },
  commands = {
    { name = "Commit", description = "commit the diff", key = "alt+g", fn = function() ... end },
  },
  loads = { "lua/web.lua" },   -- relative to this file; not allowed inside loaded files
}
```

- **Merge**: later files win for `systems`, `prompts`, `functions`, `commands`
  (by name) and `settings` keys (with a warning when replacing). `hooks` append
  in file order. `commands` keep menu order; a key clash with `settings.keys` or
  another command is a config error.
- **Function entries**: `{description = string, fn = function}`. `description`
  is the prompt line (if it does not start with a backtick, Zel formats
  `` `name` - description ``). Mode checks use `os.getenv("ZEL_MODE")`.
- **Commands**: `{name, description?, key?, fn}` appear in the main menu and
  trigger via `key` wherever global keys work. A command runs async with no
  timeout; a returned nonempty string shows like `zel print`, an error shows in
  red.
- **package.path**: starts with `<home>/defaults/?.lua;<home>/defaults/?/init.lua`,
  then each config file's folder.
- **Reload**: no file watching. Config loads at start, new session, resume, and
  on `Ctrl+R` in the `Config` menu screen (deferred after an active turn; a
  failed reload keeps the previous config and shows the error in `Config`).
  `zel config` prints every file read and the merged result.
- `keys` binds actions (`menu`, `models`, `sessions`, `new_session`, `info`,
  `trace`, `editor`, `stop`, `quit`, `redraw`) to `ctrl+LETTER`, `alt+LETTER`,
  or `""`. Russian letters on the same keys work too.

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
its upvalues. A hook has 60 s. What it returns:

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
| `turn_end` | `{answer}` | a notification; the turn goes on (at most 3 times in a row) | |
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
local d = require("defaults")

hooks = {
  { events = {"lua_before"}, fn = function(event, data)
      if data.code:find("git push %-%-force") then return false, "no force push" end
    end },
  { events = {"turn_end"}, fn = function(event, data)
      local st = d.sh("git status --short").out
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
    notice = "#808080",     -- notices, compaction
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
- `dark_gray`: the "context compacted" divider, `zel print` lines, the status/code
  in the working line, the text in an indicator line, hotkeys in menus, Tracing
  row numbers, Lua comments.
- `error`: failed turns, prompt errors, form errors.
- `trace.*`: the kind labels and bodies in Tracing, one per kind; `trace.lua`
  is also the plain text of highlighted Lua.

Strings and numbers in highlighted Lua keep fixed tones.

## Examples

See the examples topic: `init.lua`, `zel.lua`, `connect.json` and a module
loaded with `loads`.
