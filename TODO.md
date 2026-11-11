# TODO

## Direction

- Keep TUI as the only interactive application interface; remove JSON mode completely
- Preserve CLI helpers needed by the agent and users (`tasks`, `view`, `events`, `models`, version and help)
- Keep production code below 10,000 lines, excluding tests

## Priority: cancellation and correctness

- [x] Fix Esc cancelling active work: Esc must only dismiss menus, completion and other UI overlays, never stop a turn or command; keep explicit cancellation on Ctrl+C
- [x] Add a regression test proving Esc does not cancel active work, including when closing an overlay
- [x] Fix Chat Completions token accounting: add `prompt_tokens` and `completion_tokens` JSON tags in `internal/agent/chat.go` and test nonzero usage

## Remove JSON mode

- [x] Remove the `zel json` command, request/result protocol, outbox and JSON-only adapters from `internal/cli/json.go`
- [x] Remove JSON-mode dispatch, flags, startup paths and mode-specific environment handling where no longer needed
- [x] Remove JSON-mode tests and fixtures; retain coverage for shared controller behavior
- [x] Remove `docs/JSON.md` and all JSON-mode references from help, README and other documentation
- [x] Review `internal/core` after removal: keep the TUI controller, remove abstractions and public surface that existed only for JSON mode
- [x] Verify the remaining CLI helpers still work; do not remove helper output formats merely because they use JSON
- [x] JSON outbox backpressure, blocked stdout shutdown and text-only recall/unsent protocol issues are superseded by removal, not separate fixes

## Headless mode (`zel -p`)

- [x] Add a non-interactive `zel -p "prompt"` mode that runs one task without starting the TUI; this is separate from the removed JSON request/result protocol.
- [x] Create and persist the session before starting work, immediately write its session ID as the first line of stdout, and make it available for reading by that ID while running and after exit.
- [x] Write the final answer to stdout when the task finishes, then exit. Keep progress, diagnostics and errors off stdout; use stderr for diagnostics and a nonzero exit code on failure or cancellation.
- [x] Support CLI overrides for model, effort, provider, config and other applicable run settings; document supported flags and precedence over saved/default settings.
- [x] Reuse the existing agent and session persistence rather than introducing another interactive protocol or a duplicate execution engine.
- [x] Add focused regression coverage for immediate session ID output, session readability, final-answer output and exit, flag overrides, failures and cancellation; document headless usage in help and README.

## Dead code and simplification

- [x] Remove unused `ProviderModels` and its private `client.models` wrapper
- [x] Remove unused `NoticeSummary` and its dedicated regular expressions
- [x] Remove the unused exported `Expand` wrapper; retain the internal expansion implementation
- [x] Remove unused `formatMetadata`
- [x] Remove the empty `untitled` branch and its unnecessary calculation in `agent.go`
- [x] Remove empty cancelled/error branches in `tui/ui_events.go`
- [x] Replace `deferredAction` with the minimal state needed for deferred quit; remove unreachable deferred new/resume paths
- [x] Remove the full session-list lookup before resume; use the existing targeted session/project lookup and preserve useful errors
- [x] Share external-editor command construction between `tui/editor.go` and `tui/browser.go`
- [x] Recount production lines after cleanup; do not merge files merely to reduce file count

## Resource limits and persistence

- [x] Bound background-task log growth with a clear truncation or rotation policy; tail limits alone do not limit disk usage
- [x] Surface failures to persist task completion instead of silently leaving stale task status in SQLite
- [x] Make attachment cleanup reference-aware, or explicitly define retention and show when an original image is unavailable; do not silently break saved-session paths
- [x] Define bounded buffering for TUI events and task notices; coalesce streaming updates where safe without dropping final results
- [x] Review unlimited `PushNotice` versus the 64-item normal input limit and define deliberate behavior under load

## Consistency

- [x] Make `Controller.Input` validation agree with its text-or-images contract; normalize image-only input in the shared layer if supported
- [x] Define model changes during an active turn: either reject them until idle or explicitly support and document next-request behavior
- [x] Keep controller contracts and comments aligned with the TUI-only lifecycle

## UX

- [x] Keep a persistent blank row above the input after the first message is sent, matching a new session. Review `timelineRows` in `internal/tui/render.go`: the initial spacer is conditional on `len(screen) < 2`, so it is not a persistent composer margin once timeline/status rows exist. Reserve the spacing independently of timeline content and working status; add regression coverage for empty, busy and completed sessions.
- [x] Ask for confirmation before deleting a provider and its saved API key
- [x] Move directory reads and symlink metadata lookups off the TUI event loop; ignore stale asynchronous results
- [x] Clarify keyboard help: Esc dismisses UI, Ctrl+C cancels active work, Ctrl+Q exits
- [x] Warn before new/resume stops running background tasks, and let the user abort the switch

## Verification

- [x] Add focused regression coverage for changes that can break behavior, keeping at most 1–2 test files per package
- [x] Run `make test` (gofmt, vet and tests with `-race`)
- [x] Smoke-test TUI startup, Esc, cancellation, new/resume, provider deletion, image input and CLI helpers
- [x] Verify no obsolete JSON-mode commands or documentation references remain
- [x] Commit implementation changes without including unrelated user edits

## Implementation notes

- Headless mode prints the persisted session ID before startup hooks or model requests; `zel session -s ID` reads committed history during or after execution. CLI model/provider/effort overrides do not change saved defaults.
- Task logs retain their first 8 MiB plus a truncation marker. Task admission bounds running tasks plus pending completion notices to 64; composer inputs have a separate 64-item limit. UI events coalesce adjacent deltas and spill ordered overflow from a 256-event memory queue to a private temporary file.
- Image originals are retained indefinitely rather than age-deleted. Reading/resuming a saved session reports missing originals while preserving embedded previews.
- Verification: `make test` (gofmt, vet, race tests), PTY startup/Esc/Ctrl+Q smoke, subprocess headless/session-reader smoke with a local fake provider, and focused new/resume, deletion, image and helper regressions. No external-provider credentials were needed.

## Tracing chronology

- [x] Merge history, task starts and logged notices/errors by timestamp instead of appending task/event blocks; keep equal timestamps stable and undated legacy entries first.
- [x] Save event timestamps with nanosecond precision and timezone; retain support for legacy local-time logs and multiline messages.
- [x] Merge live preview records into the same chronological order, with focused regression tests.

## Persistent chat screen margin

- [x] Reserve one blank screen row above the chat viewport, independent of timeline contents and scroll position; avoid doubling it in an empty session. The earlier composer-gap fix did not preserve the top-of-screen margin.
- [x] Account for the margin in scrolling, native cursor and composer mouse coordinates. Verify rendered frames before/after sending, completion, new/resume, scrolling and resize; retain usable content in a one-row terminal.
