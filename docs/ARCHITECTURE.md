# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider is opened for `zel version`, `zel help`, `zel update`, `zel docs`
or `zel config`.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, images, tool calls, records, usage, model choices and domain events | None |
| `config` | Config data: settings (parsed from the Lua `settings` table), `connect.json` with `$VAR` expansion and the write of `provider`/`model`/`effort`, paths, the default home files, file watching, `zel config` text | None |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | Session folders: append-only `session.jsonl`, `meta.json` for the list, `tasks.jsonl`, task logs and `choices.jsonl` (answered questions) | None |
| `shell` | `bash -c` in a process group, cancellation, bounded output and image capture | `entities` |
| `ipc` | The session socket of `zel -p` children: one JSON request and response per connection, token check, cancellation when the client hangs up | None |
| `edit` | Exact-text edits, all or none | None |
| `luart` | The Lua runtime (gopher-lua): the agent VM, task VMs, the config VM that runs `init.lua`/`zel.lua` and their loads, the API (`sh`, files, `grep`, `task.*`, `choice`, ...), config functions, hook calls and the lua tool text | `config`, `shell`, `edit`, `docs` |
| `core` | Runtime assembly, sessions, controller, queues, the model/tool loop, the lua tool host, tasks, hooks, prompts, choice queue, zel -p children, hot reload, persistence and application events | Backend packages |
| `cli` | Argument parsing, headless runs, `update`, `docs`, `models`, `config` | `core`, `config` |
| `tui` | Display state, composer, menus, input events and rendering | `core` only |

The frontend imports only `core`: it uses the controller for runtime operations,
and `core` exposes the shared domain and configuration types it needs as aliases,
not copies. Backend packages never point back to `core`. A production
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

A session reads three files: `~/.config/zel/init.lua` (or the `--config`
file; a `zel -p` child takes its parent's from `ZEL_CONFIG`), `zel.lua` of the
folder it started in (not with `--config`), and `~/.config/zel/connect.json`.
`config.LoadConnect` gives the providers and the selected model;
`luart.LoadConfig` runs the Lua files and their `loads` in a new config VM,
merges what they return (settings deep, later names replace earlier ones,
hooks concatenated) and keeps the VM for the snapshot's functions and hooks.
The snapshot is a `config.Config`: settings, systems, prompts, hooks by
reference, functions and the files read. The only write is the model
selection, `config.SaveSelection`, which rewrites `provider`, `model` and
`effort` of connect.json and keeps the rest.
In the main session `config.Watch` hashes the files of the snapshot; a change
goes through the controller, which applies it between turns: a new config VM,
and a fresh agent VM with its functions. A config with errors keeps the old
snapshot. `config.EnsureHome` creates `~/.config/zel` with a commented
`init.lua`, a `connect.json` skeleton and a `.gitignore`; sessions and runtime
sockets live in `~/.zel/sessions/`. No migration from older layouts is attempted.

## The lua tool and the socket

The model sees one tool, `lua`, whose description is built by
`luart.ToolDescription` for the session mode, with the timeout and the config
functions. `provider` normalizes its transport (free-form custom tool on
Responses, JSON `{code}` elsewhere) so history always holds a function call
with `{"code": ...}`. `core.runLua` runs it in the session's agent VM with
`lua_before`/`lua_after` hooks around it and the `settings.timeout` and
`settings.output` limits; `core.luaEnv` is the host the runtime calls for the
session cwd, environment, images, choice, rename, models and tasks. The agent
VM keeps its globals between calls and is replaced after a timeout or a config
reload. Tasks are Lua functions copied with their upvalues into fresh VMs;
`core.tasks` owns their logs, states and notifications.

Every session (main or headless) listens on `~/.zel/sessions/run/<hash>.sock`
(the hash is of the session ID; a very long home falls back to the temp folder)
with a random token. Only `zel -p` children use it: one started by `sh()` finds
the socket in its environment, becomes a child (headless, depth + 1, at most 3)
and reports `child.start`/`child.end` for the parent's lifecycle/idle
tracking, and `child.usage` (model, context, spend) after each model response
for the parent's Info page. Children are separate processes in the process
group of the `sh()` that started them, so a killed call or task takes them down.

Hooks are Lua functions of the config: sync ones run in the config VM, async
ones in a fresh VM with copies of their upvalues. A returned string is the
event's text, `false, reason` blocks the blockable events. The system prompt
is stored in the history and rebuilt only when the systems change.

## History, images, and rendering

History items are immutable after append. The session caches their display
records, type/role and token estimates; compaction caches transformed zones.
Saves prepare only dirty suffixes and leave unchanged metadata files alone.

Images exist only in memory: `read()` of an image makes a preview, the session sends it
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
