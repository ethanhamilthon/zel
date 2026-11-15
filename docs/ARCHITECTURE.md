# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider or database is opened for `zel version`, `zel help` or `zel update`.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, attachments, tool calls, records, usage, model choices and domain events | None |
| `config` | Defaults, validation, preference decoding, init.lua layer discovery and paths | None |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | SQLite opening, migrations, preferences, session/item transactions and task records | `entities` |
| `shell` | Command execution, process groups, cancellation, bounded output and image capture | `entities` |
| `luart` | The Lua runtime (gopher-lua): the session's main VM, task VMs, host API (`sh`, the file functions, `tasks`, `agent`, `session`, ...), the init.lua config VM with tools, hooks and completions, and system-prompt rendering. Talks to the application only through its `Env` interface | `shell` |
| `agent` | One model/tool loop, steering checkpoints, turn-end hook and cancellation boundaries | `entities` |
| `core` | Runtime assembly, sessions, controller, queues, jobs (tasks and subagents), persistence and application events; implements `luart.Env` | Backend packages, `luart` |
| `cli` | Argument parsing, `update`, output and headless execution | `core`, `entities` |
| `tui` | Display state, composer, menus, input events and rendering | `core`, `entities` |

The frontends never import backend packages directly. `core` exposes settings and
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
event has been delivered, a failed stream is not transparently replayed. Waiting for
a retry respects context cancellation.

`config.Load` receives persisted preference bytes from `core` and finds the
init.lua layers; `core` runs them in `luart`. `config` never opens the database or imports runtime
types. Session model overrides and mutable execution state remain in `core`.

All SQL lives in `store`. Existing database paths, migrations, saved history,
configuration formats and helper protocols are retained.

## Verification

Run `make test` for formatting, vet and race-enabled tests. Package-boundary tests
also enforce frontend isolation and independent configuration/domain packages.
There is no source line-count budget.

## The lua tool

The model sees one tool, `lua`. `provider` normalizes its transport (free-form
custom tool on Responses, JSON `{code}` elsewhere) so history always holds a
function call with `{"code": ...}`. `core.runTool` hands the code to the session's
main `luart` VM (`luart.Runner`), which keeps globals between calls and is
reset on new/resume/reload or after a timeout; init.lua runs in a separate
config VM, every `tasks.spawn(fn)` in a fresh task VM that gets a deep copy of
the function and its upvalues (`luart/copy.go`); the host API reaches the application through `luart.Env`
(shell environment, tasks, subagents, images, model listing, notices). Every call has a fixed 30-second deadline
(`luart.callTimeout`) that only waiting for the user in `choice()` pauses. `choice()` asks the UI through `core`'s chooser
(`Runtime.PendingChoice`/`AnswerChoice`; the UI shows the options in the composer's place), and only the main session has one. Tasks and agents share the job
bookkeeping in `core` (IDs, logs, result delivery): a task is a Lua function on
its own goroutine, and `new_agent{}` starts a full child `Runtime` with its own
session in the same process and waits for it, `send_agent` opens that stored session again in a new child
(one at a time per subagent); a background agent is a
`new_agent{}` inside a task. Running tasks are listed for the chat rows
(`ActiveTasks`); a background agent is one of them. The parent owns its context,
cancellation, progress and result; running agents live in an in-memory tree
(`agentTree`) shared by the whole agent tree (subagents cannot start agents of
their own), and closing the parent cancels
its children, waits for them, and then runs `session_closed`, all within the 5 s session budget. There is no Unix socket and no report file. init.lua layers are run once per session in
a long-lived config VM (snapshot); tools, hooks and completions live there. The agent wakes up
only for typed input or a notification: finished jobs nobody waited for, text
from turn_end hooks, and `view()` images are queued as `<notification>...</notification>`
(consecutive ones are delivered as one message). See
[lua-api.md](lua-api.md).
