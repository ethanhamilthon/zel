# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider is opened for `zel version`, `zel help`, `zel update` or the session
subcommands (`zel task`, `zel choice`, ...), which only talk to a session socket.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, attachments, tool calls, records, usage, model choices and domain events | None |
| `config` | zel.toml layers: discovery, parsing and validation, merge, `${{ }}` expansion, point edits that keep comments, file watching, `zel config` text; settings and paths | None |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | Session folders: append-only `session.jsonl`, `meta.json` for the list, `tasks.jsonl` and task logs | None |
| `shell` | `bash -c` in a process group, cancellation, bounded output and image capture | `entities` |
| `ipc` | The session socket: one JSON request and response per connection, token check, cancellation when the client hangs up | None |
| `edit` | The exact-text blocks of `zel edit` | None |
| `agent` | One model/tool loop, steering checkpoints, turn-end hook and cancellation boundaries | `entities` |
| `core` | Runtime assembly, sessions, controller, queues, the bash tool, tasks, hooks, completions, choice queue, zel -p children, hot reload, persistence and application events | Backend packages |
| `cli` | Argument parsing, headless runs, `update`, and the subcommands (`task`, `choice`, `view`, `session`, `edit`, `docs`, `models`, `config`) | `core`, `config`, `ipc`, `edit`, `shell` |
| `tui` | Display state, composer, menus, input events and rendering | `core`, `entities` |

The frontend never imports backend packages directly. `core` exposes settings and
shared domain types using type aliases, not copies. Dependencies never point back
from backend packages to `core`.

## Ownership and events

`core.Runtime` owns live application state and backend lifetimes. Its controller
serializes interactive operations, runs one turn at a time, and emits ordered
`core.Event` lifecycle notifications. A `TurnEvent` is the shared `entities.Event`
produced during a turn. UI-only state stays in `tui`.

`agent.Run` depends on a small host interface rather than on `core`. The runtime
implements its operations: accepting steering, saving, requesting a response,
committing response items, and executing tools. Response items are committed before
tools execute; steering is accepted only at safe boundaries. Cancellation is checked
before every request and tool execution.

`provider.Request` is API-independent. Both adapters emit one canonical event/item
format (Responses-style items, also used for persisted history). Wire translation
stays inside `provider`; the runtime adds metadata and maps events for consumers.
Errors preserve their structured causes while user-facing messages redact API keys.
Transient requests retry at most three times with bounded delays. Once a response
event has been delivered, a failed stream is not transparently replayed.

## Config

A session has its layers: `~/.zel/zel.toml`, `./zel.toml` of its folder and the
`--config` files (a resumed session keeps the layers of its header; a `zel -p`
child takes its parent's from `ZEL_CONFIG`). `config.Load` merges them into one
snapshot: settings, instructions, hooks by event, completions, and the source
file of every settings key. The menus write a change back into that file with
`config.SetKey`/`DeleteKey`, which edit only the lines of the key. In the main
session `config.Watch` polls the files; a change goes through the controller,
which applies it between turns. A layer with errors keeps the old snapshot.

## The bash tool and the socket

The model sees one tool, `bash`. `provider` normalizes its transport (free-form
custom tool on Responses, JSON `{command}` elsewhere) so history always holds a
function call with `{"command": ...}`. `core.runBash` runs it with `bash -c` in
the session folder and its own process group, with the session environment
(`ZEL_SESSION_ID`, `ZEL_MODE`, `ZEL_SOCKET`, `ZEL_TOKEN`, `ZEL_CONFIG`, ...),
`bash_before`/`bash_after` hooks around it and the `settings.bash_timeout` and `settings.bash_output` limits.

Every session (main or headless) listens on `~/.zel/run/<hash>.sock` with a
random token. The `zel` subcommands its bash, tasks and hooks run send one
request there: tasks (`bash -c` jobs owned by the session, logged in
`tasks/<id>.log`, a notification when nobody waits), choice (a queue the UI
shows; the answer comes back as a notification), view, session info and rename.
A `zel -p` started by that bash finds the socket in its environment, becomes a
child (headless, depth + 1, at most 3) and reports `child.start`/`child.end`
for the parent's chat rows. Children are separate processes in the caller's
process group, so a killed call or task takes them down.

Hooks are commands from the config: the event data goes to stdin as JSON, exit
0 output is used per event, exit 2 blocks the blockable events. Instructions and
completions are strings with `${{ }}` expanded by `bash -c`. The system prompt
is stored in the history and rebuilt only when the instructions change.

## Verification

Run `make test` for formatting, vet and race-enabled tests. Package-boundary tests
also enforce frontend isolation and independent configuration/domain packages.
