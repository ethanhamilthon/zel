# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider is opened for `zel version`, `zel help`, `zel update`, `zel docs`
or `zel config`.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, images, tool calls, records, usage, model choices and domain events | None |
| `config` | Config data: settings (parsed from the Lua `settings` table), `connect.json` with `$VAR` expansion and the write of `provider`/`model`/`effort`, paths, the default home files (`EnsureHome`), the config snapshot types, `zel config` text | `defaults` |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | Session folders: append-only `session.jsonl`, `meta.json` for the list and `choices.jsonl` (answered questions) | None |
| `shell` | `bash -c` in a process group, cancellation, bounded output and image capture | `entities` |
| `ipc` | The session socket of `zel -p` children: one JSON request and response per connection, token check, cancellation when the client hangs up | None |
| `defaults` | The embedded Lua defaults (`defaults/*.lua`: `sh`, `read`, `write`, `edit`, `grep`, ...) and the default `init.lua` | None |
| `luart` | The Lua runtime (gopher-lua): the config VM that runs the config files and their loads, the session VM (Lua standard library plus the config `functions`, copied in one pass so shared upvalues stay shared), hook and command calls, the result rule of a lua call | `config`, `shell` |
| `core` | Runtime assembly, sessions, controller, queues, the model/tool loop, the lua tool host, hooks, commands, the prompt, the session socket, indicators, print lines, choice queue, zel -p children, reload, persistence and application events | Backend packages |
| `cli` | Argument parsing, headless runs, `update`, `docs`, `models`, `config` | `core`, `config`, `defaults` |
| `tui` | Display state, composer, menus, input events and rendering | `core`, `config` |

The frontend imports `core` and the data types of `config`: it uses the controller for runtime operations. Backend packages never point back to `core`. A production
import-boundary test (`docs/docs_test.go`) protects this dependency graph.

## Ownership and events

`core.Runtime` owns live application state and backend lifetimes. Its controller
serializes interactive operations, runs one turn at a time, and emits ordered
`core.Event` lifecycle notifications. A `TurnEvent` is the shared `entities.Event`
produced during a turn. UI-only state stays in `tui`. Slow config reloads, model selection and session changes
reserve an operation and release the controller mutex during external work.
Results publish before queued turns proceed. Conflicting session changes are
rejected; input queued during a config reload is delivered afterward. Closing
waits for reserved operations. Status and snapshot reads remain available.

The model/tool loop is `turnHost.run` in `core`: accept steering, save, request a
response, commit its items, execute the tools, compact, accept steering again.
Response items are committed before tools execute; steering is accepted only at
safe boundaries. Cancellation is checked before every request and tool execution.

`provider.Request` is API-independent. Both adapters emit one canonical event/item
format (Responses-style items, also used for persisted history). Wire translation
stays inside `provider`; the runtime adds metadata and maps events for consumers.
Errors preserve their structured causes; the provider client redacts its API key
from every message it returns. A request is tried up to six times with exponential
backoff (1 s to 30 s) and jitter, honoring `Retry-After`; permanent errors are not
retried. Once a response event has been delivered, a failed stream is not
transparently replayed. A response has no total time limit: a stream that
stays silent for five minutes ends as a retryable failure.

## Config

A session reads the config files in order: `~/.config/zel/init.lua` (or
`$ZEL_HOME/init.lua`), then `zel.lua` of the folder it started in, then every
`--config FILE` (`--no-default-configs` drops the first two; a `zel -p` child
gets the explicit files in `ZEL_CONFIGS` and the flag as
`ZEL_NO_DEFAULT_CONFIGS`), and `connect.json` next to `init.lua`.
`config.LoadConnect` gives the providers and the selected model;
`luart.LoadConfig` runs the Lua files and their `loads` in a new config VM,
merges what they return (settings deep, later names replace earlier ones with a
warning, hooks concatenated in file order) and keeps the VM for the functions,
hooks and commands. The snapshot is a `config.Config`: settings, systems,
prompts, hooks and commands by reference, functions, the files read, and the
source file of each entry. The only write is the model selection,
`config.SaveSelection`, which rewrites `provider`, `model` and `effort` of
connect.json and keeps the rest.

Nothing watches the files. The config loads at start, on a new session, on
resume and on Ctrl+R in the Config screen; the controller applies a reload
between turns: a new config VM, a fresh session VM and a rebuilt system prompt.
A config with errors keeps the old snapshot and the error shows in Config.
`config.EnsureHome` creates `~/.config/zel` with `init.lua`, `defaults/` (from
`internal/defaults`), a `connect.json` skeleton and a `.gitignore`; existing
files are never touched, `zel defaults --reset` rewrites `defaults/`. Sessions
and runtime sockets live in `~/.zel/sessions/`.

## The lua tool and the socket

The model sees one tool, `lua`, described by `internal/core/prompt/lua_tool.md`
with the timeout. The system prompt is `internal/core/prompt/zel.md`, then one
line per config function, then the config `systems`; it is stored in the history
and rebuilt on a new session, on resume and on reload. `provider` normalizes the
tool transport (free-form custom tool on Responses, JSON `{code}` elsewhere) so
history always holds a function call with `{"code": ...}`. `core.runLua` runs it
in the session VM with `lua_before`/`lua_after` hooks around it and the
`settings.timeout` and `settings.output` limits. The VM has no host API: files,
`sh` and the rest are Lua in `defaults/`, reaching Zel through the `zel` CLI.
It keeps its globals between calls and is replaced after a timeout or a reload.

Every session (main or headless) listens on `~/.zel/sessions/run/<hash>.sock`
(the hash is of the session ID; a very long home falls back to the temp folder)
with a random token, also written to a 0600 file in the session folder. The
`zel` subcommands `steer`, `status`, `choice`, `print`, `session` and `rename`
use it (`zel exec` does not); a command run inside a call finds the socket in its
environment, `-s ID` picks another live session. A `zel -p` child becomes a
child of that session (headless, depth + 1, at most 3) and reports
`child.start`/`child.end` for the parent's lifecycle/idle tracking, and
`child.usage` (model, context, spend) after each model response for the parent's
Info page. `zel exec` supervises the foreground command in a separate process
group. If the Lua call kills the outer `zel exec`, a lifeline pipe closes and
the supervisor kills the foreground command's group. Once the foreground
command exits normally, intentional background jobs can outlive the call.

`zel status` sets the working line text of the running call; `zel status new`
creates an indicator line with its own timer (`Controller.Indicators`).
`zel print` and the output of a config command are `print` records: kept in the
session history, shown in the chat, never sent to the model.

Hooks are Lua functions of the config: sync ones run in the config VM, async
ones in a fresh VM with copies of their upvalues. A returned string is the
event's text, `false, reason` blocks the blockable events. Commands are config
functions run asynchronously from the menu or their key.

## History, images, and rendering

History items are immutable after append. The session caches their display
records, type/role and token estimates; compaction caches transformed zones.
Saves prepare only dirty suffixes and leave unchanged metadata files alone.

Images exist only in memory: `read()` of an image runs `zel steer --file`, the session sends it
with the next request, and `withoutImages` turns it into a text placeholder in
the saved history. Nothing is written to an attachments folder.

Only complete commands are published before tool execution; incomplete argument
fragments do not become UI records. The tcell frontend has one event loop owning
all UI state. Controller events use a bounded, lossless spool; background work
returns results with session identities so stale results cannot replace a draft.
The shared editor is used for chat, custom answers, search and forms;
the shared searchable list is used by menus and completions. There is no widget
framework or second frontend. tcell owns terminal modes, input, cursor and cell
updates. Glamour formats Markdown; its output is decoded into styled cells,
never sent directly to the terminal. Formatted records and stable Markdown
prefixes are cached; oversized text uses a bounded plain-text fallback.
The mouse is not captured: the terminal selects text and opens links, and the
arrows or PgUp/PgDn scroll the chat. Lists share one layout: a label column, a
second column for hotkeys or dates, and the selection arrow `‹` on the right.

Asynchronous hooks belong to the session lifecycle. Shutdown cancels and joins
them before releasing resources or removing an unsaved session directory.

## Verification

Run `make test` for formatting, vet and race-enabled tests. Package-boundary tests
also enforce frontend isolation and independent configuration/domain packages.
