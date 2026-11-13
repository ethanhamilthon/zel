# Architecture

`main` is the composition root. It wires the runtime constructor and interactive
frontend into CLI dispatch, installs signal handling, and reports the exit status.
No provider or database is opened for standalone helpers such as `zel init`.

## Packages

| Package | Responsibility | Internal dependencies |
| --- | --- | --- |
| `entities` | Shared input, attachments, tool calls, records, usage, model choices and domain events | None |
| `config` | Defaults, validation, preference decoding, instruction layering, paths and project initialization | None |
| `provider` | Typed requests, Responses/Chat Completions adapters, model catalog, normalized streaming, structured errors and retry | `entities` |
| `store` | SQLite opening, migrations, preferences, session/item transactions and task records | `entities` |
| `shell` | Command execution, process groups, cancellation, bounded output and image capture | `entities` |
| `agent` | One model/tool loop, steering checkpoints and cancellation boundaries | `entities` |
| `core` | Runtime assembly, sessions, controller, queues, task lifecycle, persistence and application events | Backend packages |
| `cli` | Argument parsing, helper commands, output and headless execution | `core`, `entities` |
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

`config.Load` receives persisted preference bytes from `core`, then merges the
existing instruction layers. `config` never opens the database or imports runtime
types. Session model overrides and mutable execution state remain in `core`.

All SQL lives in `store`. Existing database paths, migrations, saved history,
configuration formats and helper protocols are retained.

## Verification

Run `make test` for formatting, vet and race-enabled tests. Package-boundary tests
also enforce frontend isolation, independent configuration/domain packages and the
production-code budget of fewer than 10,000 Go lines (including blank lines and
comments, excluding tests).
