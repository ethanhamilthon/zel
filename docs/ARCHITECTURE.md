# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider is opened for `zel version`, `zel help`, `zel update` or the session
subcommands (`zel task`, `zel choice`, ...), which only talk to a session socket.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, images, tool calls, records, usage, model choices and domain events | None |
| `config` | The one settings.toml: parsing and validation, `${{ }}` expansion, the default home files, the write of `provider`/`model`/`effort` that keeps comments, file watching, `zel config` text; settings and paths | None |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | Session folders: append-only `session.jsonl`, `meta.json` for the list, `tasks.jsonl`, task logs and `choices.jsonl` (answered questions) | None |
| `shell` | `bash -c` in a process group, cancellation, bounded output and image capture | `entities` |
| `ipc` | The session socket: one JSON request and response per connection, token check, cancellation when the client hangs up | None |
| `edit` | The exact-text blocks of `zel edit` | None |
| `core` | Runtime assembly, sessions, controller, queues, the model/tool loop, the bash tool, tasks, hooks, completions, choice queue, zel -p children, hot reload, persistence and application events | Backend packages |
| `cli` | Argument parsing, headless runs, `update`, and the subcommands (`task`, `choice`, `view`, `session`, `edit`, `docs`, `models`, `config`) | `core`, `config`, `ipc`, `edit`, `shell` |
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

A session has one config file: `~/.config/zel/settings.toml`, or the `--config` file in
its place (a `zel -p` child takes its parent's from `ZEL_CONFIG`). Nothing is
inherited from other homes. `config.Load` reads the selected file and sorted
`hooks/`, `prompts/`, `instructions/` snippets beside it into one snapshot: settings,
providers, instructions, hooks by event and completions. Providers,
instructions, hooks and completions are named entries (`[hook.NAME]`) with
`modes`; a session keeps those whose modes meet its own (`main` or `headless`,
plus the `--mode` one), see `config.InModes`. The only write is the model
selection, made with `config.SetKey`, which replaces or adds just the top-level
`model` (`PROVIDER/MODEL`) and `effort` lines.
In the main session `config.Watch` hashes the file and supported directory
entries, including creations and removals; a change goes through the
controller, which applies it between turns. A file with errors keeps the old
snapshot. `config.EnsureHome` creates `~/.config/zel` with a default `settings.toml`
and the three snippet directories and a `.gitignore`; runtime data is under
`~/.config/zel/.sessions/`. No migration from older layouts is attempted.

## The bash tool and the socket

The model sees one tool, `bash`, whose description is the built-in text of the
session mode (`internal/core/bash_main.md`, `bash_headless.md`) with the timeout and
output limit filled in. `provider` normalizes its transport (free-form
custom tool on Responses, JSON `{command}` elsewhere) so history always holds a
function call with `{"command": ...}`. `core.runBash` runs it with `bash -c` in
the configured bash working folder and its own process group, with the session environment
(`ZEL_SESSION_ID`, `ZEL_MODE`, `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG`, ...),
`bash_before`/`bash_after` hooks around it and the `settings.bash_timeout` and `settings.bash_output` limits.

Every session (main or headless) listens on `~/.config/zel/.sessions/run/<hash>.sock`
(the hash is of the session ID; a very long home falls back to the temp folder)
with a random token. The `zel` subcommands its bash, tasks and hooks run send one
request there: tasks (`bash -c` jobs owned by the session, logged in
`tasks/<id>.log`, a notification when nobody waits), choice (a queue the UI
shows; the answer comes back as a notification), view, session info and rename.
A `zel -p` started by that bash finds the socket in its environment, becomes a
child (headless, depth + 1, at most 3) and reports `child.start`/`child.end`
for the parent's lifecycle/idle tracking, and `child.usage` (model, context,
spend) after each model response for the parent's Info page (not separate child chat rows). Children are separate processes in the caller's
process group, so a killed call or task takes them down.

Hooks are commands from the config: the event data goes to stdin as JSON, exit
0 output is used per event, exit 2 blocks the blockable events. Instructions and
completions are strings with `${{ }}` expanded by `bash -c`. The system prompt
is stored in the history and rebuilt only when the instructions change.

## History, images, and rendering

History items are immutable after append. The session caches their display
records, type/role and token estimates; compaction caches transformed zones.
Saves prepare only dirty suffixes and leave unchanged metadata files alone.

Images exist only in memory: `zel view` makes a preview, the session sends it
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
