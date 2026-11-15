# Zel — Minimalist AI Coding Agent

A focused terminal coding agent with one tool: Lua. The model writes Lua; shell
commands, files, background tasks and subagents are plain functions in it.
You extend the agent in Lua too, with `~/.zel/init.lua` and `./.zel/init.lua`;
providers and preferences live in the UI.

> **0.6.0 is a breaking release.** The Lua API is one flat set of functions
> (`read`, `write`, `edit`, ... instead of `fs.*`; `new_agent{}` instead of
> `agents.*`; `tasks.spawn(name, fn)`), and `web`, `json`, `sleep` and `state`
> are gone: use `sh` and files. init.lua returns a table instead of calling
> `require("zel")`, `defaults` and the subagent hooks are gone, and the CLI keeps
> only `zel`, `zel -p`, `update`, `version` and `help`. See
> [docs/lua-api.md](docs/lua-api.md). There is no migration.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

Installs the latest release for macOS or Linux (arm64/amd64) into
`~/.local/bin` (override with `ZEL_BINDIR`; pin with `ZEL_VERSION=v0.6.3`).
`zel update` later replaces the binary with the latest GitHub release.

## Build from source

```sh
make install     # installs ~/.local/bin/zel
zel              # opens first-run onboarding if no provider exists
make dev         # isolated ~/.zeldev data
make test        # formatting, vet, race tests
```

Add `~/.local/bin` to `PATH`. Go 1.27.3+ and Make are required.

Zel keeps its data in `~/.zel` (override with `ZEL_HOME`).

## Onboarding and settings

The first launch opens a four-step provider form: Name, Base URL, API key, API
type (`responses` or `chat`). Tab / Shift+Tab switch steps and Enter saves the
last step; an empty API key is allowed for local servers. Then choose a model
and reasoning effort (default `medium`).

Ctrl+K opens the menu (typing `/` into an empty composer does too; Settings →
Slash opens menu turns that off). Providers adds, edits or deletes providers;
Settings controls the Lua output budget, auto compaction, reasoning summaries
and the accent color. Providers, keys, preferences and sessions live in the
private SQLite database `~/.zel/zel.db`. API keys are stored locally, not
encrypted; a key may reference an environment variable with `{{$VARIABLE}}`.

## The lua tool

The model has exactly one tool, `lua`; its input is Lua code. Each call has a
fixed 30-second timeout. The session has one VM, so globals stay between calls
until the session changes, init.lua reloads or a call times out. Output is
whatever the code prints plus the returned value, never truncated: the model
filters in Lua.

```lua
sh(cmd, o?)                       -- shell, also for curl, jq, sleep
read write edit patch list glob grep exists tmp slice
tasks.spawn(name, fn, o?)         -- background work; tasks.list/get/wait/tail/stop
new_agent{prompt, model?, ...}    -- a subagent; send_agent(id, text) writes to it again
choice(name, options)             -- you pick an option in the UI instead of typing
view log models session
docs(topic?)                      -- the agent reads how zel works, by topic
```

Everything blocks except `tasks.spawn`. The agent wakes up only for your
messages and short notifications such as `<notification>t1 finished</notification>`.
Messages you type during a call wait until the call ends. Full reference:
[docs/lua-api.md](docs/lua-api.md).

There is no sandbox, permission prompt or trust dialog: Lua runs with your user
permissions and network access, so only use `init.lua` files you trust.

## Configuration: init.lua

Zel reads `~/.zel/init.lua`, then the nearest `.zel/init.lua` from the working
directory upward. A file returns a table, or a list of them:

```lua
return {
  require("web"),   -- ~/.zel/lua/web.lua returns a table too
  {
    instructions = {"Read code first.", function() return read("AGENTS.md") end},
    tools = {
      check = {
        desc = "gofmt + vet + tests. args: pkg? (default ./...). returns failures",
        fn = function(a) return sh("go vet " .. (a.pkg or "./...")).out end,
      },
    },
    hooks = {
      turn_end = function(answer, running)
        if #running == 0 and sh("git status --porcelain").out ~= "" then
          return "uncommitted changes, commit them"
        end
      end,
    },
    completions = {plan = function() return read("PLAN.md") end},
  },
}
```

- **instructions** go into the system prompt: text, functions that return
  text, or a list of them; `{..., only = "main"}` or `{..., only = "sub"}`
  limits entries to the main agent or to subagents.
- **tools** are functions for the agent. The agent calls `check{pkg = "./x"}`;
  `desc` is shown in the system prompt as is.
- **hooks**: `session_created()` (once per new session), `message(text, notice)`,
  `turn_end(answer, running)` and `session_closed(reason)`. Returned text from
  `turn_end` reaches the agent as a notification; `message` can rewrite what you
  typed.
- **completions** register `#plan`: type `#` to list them, and the returned
  text replaces the token when you send the message.

A session reads the files once, at start or resume. Menu → Reload reads them
again; a broken file keeps the previous snapshot. Examples are in
[docs/examples](docs/examples/).

### Output limits

Lua call output is not truncated; only a 1 MiB safety cap (head and tail kept,
with a marker) protects memory and the context. `sh().out` keeps up to 8 MiB,
`read` refuses files over 16 MiB.

### Auto compaction

When the context reaches the compaction threshold (default 400000 tokens; 0
turns it off), Zel compacts it between model steps. Counting back from the
latest message, the last quarter of the threshold is sent unchanged; the next
quarter keeps its commands but cuts their outputs to the first line; anything
older keeps only user and assistant messages, with a marker telling the model
where the compaction happened. Saved history is never rewritten, so Tracing
still shows everything, and the chat shows a "context compacted" divider.

## Chat and Tracing

Chat shows user messages, agent messages and the working indicator. The
indicator shows `thinking...` until the agent does something, then its last
action (for example `reading ~/.zel/init.lua`) until the next one; labels never
show a command's arguments or a secret. Each running background task has a row:
a blinking dot, the task's name and how long it runs (a background subagent is a
task too). A row goes away when its task ends.

Tools, thoughts, errors and notices belong in Tracing. Tracing search accepts a
leading type, optionally followed by text: `:user`, `:assistant fix tests`,
`:lua go test`, `:error`. A turn that fails or is stopped with Ctrl+D ends in
the chat with a red line of time and steps, then the reason.

Typing `@` in the composer completes file paths; `#` lists prompt completions.

### Shortcuts

- Ctrl+K: Menu
- Ctrl+M: Models
- Ctrl+R: Resume
- Ctrl+N: New session
- Ctrl+I: Info
- Ctrl+T: Tracing (Enter opens an entry; Left/Right switch entries)
- Ctrl+Y: Copy the last answer as plain text
- Ctrl+D: Stop the active turn or a prompt completion still expanding
- Ctrl+Q: Save and quit
- Esc: back
- Up / Down move through the composer's rows, then scroll the chat; PgUp / PgDn
  scroll the timeline

Ctrl+M and Ctrl+I need the Kitty keyboard protocol; otherwise use the menu.
Mouse reporting is off, so selection and copying are your terminal's. A queued
message stays editable in the composer; editing withdraws it.

## Headless

```sh
zel -p "Review this project" --provider NAME --model MODEL --effort high
```

The first stdout line is the session ID, written before any model request; the
final answer follows when the task finishes. Errors go to stderr. Success exits
0, errors nonzero, cancellation 130. The flags apply to this run only. Resume
the session later from the TUI.

## Contributing

See [Architecture](docs/ARCHITECTURE.md) for package boundaries and ownership.

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.

## License

[MIT](LICENSE)
