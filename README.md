# Zel — Minimalist AI Coding Agent

A focused terminal coding agent with one tool: Lua. The model writes Lua; shell
commands, files, HTTP, background tasks and subagents are plain functions in it.
You extend the agent in Lua too, with `~/.zel/init.lua` and `./.zel/init.lua`;
providers and preferences live in the UI.

> **0.5.2 changes the Lua API again.** The Lua VM now keeps globals between
> calls, the call timeout is 30 s, output is no longer truncated (`slice()` and
> `fs.patch` are new), `tasks.spawn` takes a Lua function (shell commands and
> `tasks.run` / `zel tasks` are gone), finished jobs arrive as short
> `<notification>` messages, and only four hooks remain: `session_start`,
> `session_end`, `message`, `turn_end`.
>
> **0.5.1 changes the Lua API.** The Lua call timeout is fixed at 10 s (no
> `--! timeout=N`), job handles and the global `wait_all` are replaced by
> `tasks.*` / `agents.*` functions taking a job ID, and `zel view` / `zel tasks run|stop`
> are removed.
>
> **0.5.0 is a breaking release.** The Bash tool and `ZEL.md` are gone. Move
> your instructions into `.zel/init.lua` (see below). There is no migration.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

Installs the latest release for macOS or Linux (arm64/amd64) into
`~/.local/bin` (override with `ZEL_BINDIR`; pin with `ZEL_VERSION=v0.4.0`).
`zel update` later replaces the binary with the latest GitHub release.

## Build from source

```sh
make install     # installs ~/.local/bin/zel
zel              # opens first-run onboarding if no provider exists
make dev         # isolated ~/.zeldev data
make test        # formatting, vet, race tests
```

Add `~/.local/bin` to `PATH`. Go 1.27.3+ and Make are required.

Zel uses fresh data in `~/.zel` (override with `ZEL_HOME`). It does not read,
modify, or migrate Dashy data. There is no `setup` command or TOML configuration.

## Onboarding and settings

The first launch opens a centered, four-step provider form: Name, Base URL,
API key, API type (`responses` or `chat`). Tab / Shift+Tab switch steps,
Left / Right move the input cursor, and Enter advances and saves the last step.
An empty API key is allowed for local servers. After saving, choose a model and
reasoning effort. Model listing failures offer retry and manual model entry.
Effort defaults to `medium`.

Ctrl+K opens the main menu. Typing `/` into an empty composer also opens it
without inserting the slash; disable this in Settings → Slash opens menu.
Providers lets you add, edit, or delete providers;
Settings controls the Lua output budget, auto compaction,
reasoning summaries, and the accent color (20 presets). Providers, keys, defaults, preferences, sessions, and
tasks live in the private SQLite database `~/.zel/zel.db`. API keys are stored
locally, not encrypted; keep this directory private. Keys may reference an
environment variable with `{{$VARIABLE}}`.

## The lua tool

The model has exactly one tool, `lua`. Its input is raw Lua code. Each Lua
invocation has a fixed 30-second timeout that cannot be changed; long work belongs in
`tasks.spawn` / `agents.spawn` and is checked in later calls.
Providers with free-form tools (OpenAI Responses) get the code as raw text, the
others a JSON `{"code": "..."}`; both are normalized to the same call.

The session has one Lua VM: globals and functions defined by one call are there
in the next. It is reset when the session changes, init.lua reloads or a call
times out, and the next output says so once; `state.get/set/list` outlives it
(scope `session` or `project`, stored in `zel.db`). Output is whatever the
code `print()`s plus the returned value, never truncated, so the model filters
in Lua: `local a = fs.read("bigfile.md"); print(slice(a, 100, 10))`. Everything
blocks, except `tasks.spawn(fn)` and `agents.spawn`, which return a job ID for
work that continues in the background (`tasks.wait/wait_all/result/status/tail/stop`
and the same under `agents`). A task is a Lua function that runs in its own VM
with copies of what it captured; subagents cannot start subagents. The agent
wakes up only for your messages and notifications: unclaimed results arrive as
`<notification>t1 finished</notification>` and the model fetches the details. Messages you type during a call are queued until the
call ends; Ctrl+D (stop the turn) cancels the running call; background work keeps running.

Host API: `sh`, `fs.*`, `web.request/get/post`, `json`, `tasks.*`, `agents.*`,
`state.*`, `view`, `session`, `log`, `sleep`. The model sees the
current API in the system prompt. Full reference: [docs/lua-api.md](docs/lua-api.md).

There is no sandbox, permission prompt or trust dialog: Lua runs with your user
permissions and network access, so only use `init.lua` files you trust.

## Configuration: init.lua

Run `zel init` in a project folder to create `./.zel/init.lua` (it loads a local
`AGENTS.md` as instructions when present). An existing file is never overwritten.
This command does not require a provider or create a session.

Layers load in order:

1. `$ZEL_HOME/init.lua` (default `~/.zel/init.lua`)
2. the nearest `.zel/init.lua` from the working directory upward
3. `--config FILE` (a `.lua` file), when supplied (must exist)

```lua
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}                      -- static prompt text
zel.prompt(function() return "git: " .. sh("git status --short").out end)
zel.defaults{model = "sonnet-5.5", effort = "medium"}     -- CLI flags still win
zel.fn{                                                   -- a function for the agent
  name = "zel.check", summary = "gofmt + vet + tests, returns only failures",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  run = function(pkg) return sh("go vet " .. pkg).out end,
}
zel.on("turn_end", function(ev) end)  -- also session_start, session_end, message; returned text becomes a notification
```

Registered functions are rendered into the system prompt (signature, summary,
examples), so the model finds them without extra setup. `package.path` includes
`~/.zel/lua/?.lua` and `.zel/lua/?.lua`. A layer that fails to load is skipped
with a notice. Web search and fetch are not in the core (only raw `web.request`);
a ready plugin is in [docs/examples](docs/examples/).

A session reads init.lua once, at start or resume, and keeps that snapshot (and
its saved prompt). Menu → Reload rebuilds both; while a turn is active the reload is
deferred until it ends, and a broken file keeps the previous snapshot.

### Output limits

Lua call output is not truncated; the model is told to filter and slice in
Lua. Only a 1 MiB safety cap (head and tail kept, with a marker) protects memory
and the context; `sh().out` keeps up to 8 MiB, `fs.read` refuses files over
16 MiB. The TUI bounds what it lays out on its own.

### Auto compaction

When the context reaches the compaction threshold (default 400000 tokens; 0
turns it off), Zel compacts it between model steps, after the step's commands
finish. Counting back from the latest message, the last quarter of the
threshold is sent unchanged; the next quarter keeps its commands but cuts their
outputs to the first line; anything older keeps only user and assistant
messages, at whole-turn boundaries, with a marker telling the model where the
compaction happened. Saved history is never rewritten, so Tracing still shows
everything, and the chat shows a "context compacted" divider.

## Chat and Tracing

Chat shows user messages, agent messages, and the working indicator. Tools,
thoughts, errors, and other events belong in Tracing; there are no
visibility toggles. Info, Tracing, and Tasks refresh while open.

Tracing search accepts a leading exact type, optionally followed by search text:
`:user`, `:assistant fix tests`, `:lua go test`, `:error`. Every displayed
entry type is supported; ordinary search works without a prefix.

Typing `@` in the composer completes file paths, one directory at a time.

### Shortcuts

Up moves through the input's visual rows (including wrapped lines), then scrolls
chat once the cursor reaches the top row. Down scrolls back toward the composer,
then moves through its visual rows. PgUp/PgDn scroll the timeline.
Completion lists and queued-input recall retain their existing keyboard priority.

Mouse selection and copying are handled by your terminal, with no automatic
copy on release. Use Cmd+C or Ctrl+C if configured as your terminal's copy shortcut.
Mouse reporting is disabled: use the keyboard for timeline scrolling and menus,
and your terminal's link-opening gesture for links.

- Ctrl+K: Menu
- Ctrl+M: Models
- Ctrl+R: Resume
- Ctrl+N: New session
- Ctrl+I: Info
- Ctrl+T: Tracing
- Ctrl+Q: Save and quit
- Ctrl+Y: Copy the last answer as plain text (also available in Menu → Copy answer)
- Ctrl+D: Stop the active turn (does not exit)
- Ctrl+C: reserved for terminal copying; never stops the active turn
- Esc: back; shortcut-opened pages return directly to chat
- Tracing merges history, tasks and notices/errors chronologically (tasks use their
  start time); it opens at the latest entry. Enter opens it full screen
- In a tracing entry: Left/Right switch entries within the filtered list,
  Up/Down scroll, and Esc returns to the list with its filter and selection

Ctrl+M and Ctrl+I require Kitty keyboard protocol support to distinguish them
from Enter and Tab; otherwise use Menu. New session and Resume are blocked
during active turns. A queued message stays editable in the composer and the working
indicator shows it as queued; editing withdraws it, and Enter queues it again.

## Automation and security

Run one task without the TUI:

```sh
zel -p "Review this project" --provider NAME --model MODEL --effort high --config ./extra.lua
zel session -s SESSION_ID
```

The first stdout line is the persisted session ID, written before any model
request. The final answer follows when the task finishes, then the process exits.
Progress and tool traffic do not go to stdout; errors go to stderr. Success exits
0, errors exit nonzero, and cancellation exits 130. Read a persisted snapshot with
`zel session -s ID` while it runs or after it exits; completed sessions can also be
resumed from the TUI. A snapshot includes committed items, not live stream deltas.

`--provider`, `--model` and `--effort` override saved defaults for this headless
session only; they never change defaults for later sessions. `--config` keeps its
usual highest-priority instruction layer. Headless mode reuses the normal agent,
tools and session store; it is not an interactive request protocol.

`zel events` prints the notices a session showed and `zel models`
checks providers (including `--json` output). Run `zel --help` for details.

Task logs (what a task printed) retain the first 8 MiB plus an explicit truncation marker; further
output is drained without increasing disk usage. Task admission limits running
tasks plus pending completion notices to 64, so final task results are not dropped.
Composer input is separately limited to 64 items. Slow UI consumers use a bounded
256-event memory queue with adjacent streaming deltas coalesced and ordered overflow
spooled to a private temporary file. The file is unlinked immediately and reclaimed when closed or on exit. A spool
I/O failure cancels work and exits with an explicit error; saved history remains
available through the session reader.

Captured image originals are retained indefinitely in `~/.zel/attachments` so
saved-session paths remain usable for crops. Delete originals manually only when
they are no longer needed; the embedded image preview stays in the saved history.
Settings and model changes are rejected during an active turn. Reload is deferred
until the turn ends. New/resume asks before stopping running background tasks;
provider deletion asks before deleting the provider and its saved API key.

Zel executes Lua and shell commands with your user permissions and network access, not inside
a sandbox.

## Contributing

See [Architecture](docs/ARCHITECTURE.md) for package boundaries and ownership.

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.
There is no source line-count cap.

## License

[MIT](LICENSE)
