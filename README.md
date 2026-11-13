# Zel — Minimalist AI Coding Agent

A focused terminal coding agent with one tool: Bash. You control the system
prompt through project Markdown, providers and preferences through the UI.

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
Settings controls the Bash output budget, Bash timeout, auto compaction,
and reasoning summaries. Providers, keys, defaults, preferences, sessions, and
tasks live in the private SQLite database `~/.zel/zel.db`. API keys are stored
locally, not encrypted; keep this directory private. Keys may reference an
environment variable with `{{$VARIABLE}}`.

## Project instructions: ZEL.md

Run `zel init` in a project folder to create `ZEL.md` from the exact contents of
a local `AGENTS.md` when present (otherwise empty). An existing `ZEL.md` is never
overwritten. This command does not require a provider or create a session.

`ZEL.md` is plain Markdown: the whole file is the system prompt. Layers load in
order and accumulate:

1. `$ZEL_HOME/ZEL.md` (default `~/.zel/ZEL.md`)
2. Every `ZEL.md` from the filesystem root down to the working directory
3. `--config FILE`, when supplied (must exist)

A session saves its prompt snapshot. Reload rebuilds it; while a turn is active,
the prompt change is deferred until that turn finishes. New sessions reread layers.

### Output limits

Bash output uses a byte budget with head/tail truncation. With
4096 bytes configured, Zel retains the first 2048 bytes, a truncation marker,
and the last 2048 bytes. The marker is additional to the retained-byte budget.
Output within the limit is unchanged. Zero means unlimited; the default is
8192 bytes.

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
`:user`, `:assistant fix tests`, `:bash go test`, `:error`. Every displayed
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
during active turns. A queued message stays editable in the composer; editing
withdraws it, and Enter queues it again.

## Automation and security

Run one task without the TUI:

```sh
zel -p "Review this project" --provider NAME --model MODEL --effort high --config ./ZEL.md
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

`zel tasks` manages background commands, `zel view` queues images, and `zel models`
checks providers (including `--json` output). Run `zel --help` for details.

Task logs retain the first 8 MiB plus an explicit truncation marker; further
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

Zel executes commands with your user permissions and network access, not inside
a sandbox.

## Contributing

See [Architecture](docs/ARCHITECTURE.md) for package boundaries and ownership.

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.
There is no source line-count cap.

## License

[MIT](LICENSE)
