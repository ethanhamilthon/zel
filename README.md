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
Settings controls the external editor, Bash/hook output budget, Bash timeout,
and reasoning summaries. Providers, keys, defaults, preferences, sessions, and
tasks live in the private SQLite database `~/.zel/zel.db`. API keys are stored
locally, not encrypted; keep this directory private. Keys may reference an
environment variable with `{{$VARIABLE}}`.

## Project instructions: ZEL.md

Run `zel init` in a project folder to create `ZEL.md`. It uses the folder name,
a random supported color, and the exact contents of a local `AGENTS.md` as the
instruction body when present (otherwise empty). An existing `ZEL.md` is never
overwritten. This command does not require a provider or create a session.

```markdown
---
name: My project
description: A short project description
color: terracotta
hooks:
  app_start: "./start.sh"
  bash_end:
    - "./check.sh"
    - "./report.sh"
---
You are a coding assistant for this project.
Read code first. Keep changes focused and verify your work.
```

The YAML front matter is optional; the Markdown body is the system prompt.
Unknown front-matter fields, invalid colors, and unknown hook events are errors.
Layers load in order:

1. `$ZEL_HOME/ZEL.md` (default `~/.zel/ZEL.md`)
2. Every `ZEL.md` from the filesystem root down to the working directory
3. `--config FILE`, when supplied (must exist)

Bodies and hooks accumulate in layer order. Later `name`, `description`, and
`color` fields override earlier values. Hook commands run relative to the file
that defines them. Metadata appears in Info; the main menu header is
`zel VERSION/NAME` (or `/Menu` without a project name).

A session saves its prompt snapshot. Reload rebuilds it; while a turn is active,
the prompt change is deferred until that turn finishes. New sessions reread layers.

### Colors

One accent is used throughout the application, alongside neutral shades and red
for errors. The default is `terracotta`. The 20 supported names are:

`terracotta`, `rose`, `dusty-pink`, `mauve`, `lavender`, `violet`, `slate`, `blue`,
`sky`, `teal`, `sage`, `olive`, `moss`, `sand`, `ochre`, `amber`, `peach`, `clay`,
`copper`, `plum`.

## Hooks

Each event accepts a shell command or a list of commands:

- `app_start`, `app_exit`
- `session_start`, `session_close`
- `first_message`, `user_message`
- `turn_start`, `turn_end`
- `bash_start`, `bash_end`
- `model_request_start`, `model_request_end`
- `error`

`session_start` also fires on resume. `session_close` fires on session switches
and app exit. `first_message` fires only once per session, not on each resume.
`user_message` includes messages accepted as steering, but not system notices.

Every hook receives `ZEL_EVENT`, `ZEL_SESSION_ID`, `ZEL_CWD`, `ZEL_HOME`,
`ZEL_MODE`, `ZEL_PROVIDER`, `ZEL_MODEL`, `ZEL_EFFORT`, and
`ZEL_CONTEXT_TOKENS`. A session's helper socket is available as `ZEL_ENDPOINT`.
Event-specific variables:

| Event | Additional variables |
|---|---|
| `first_message`, `user_message` | `ZEL_MESSAGE` |
| `turn_start` | `ZEL_MESSAGE`, `ZEL_TURN_SOURCE` (`user` or `notice`) |
| `turn_end` | `ZEL_TURN_STATUS`, `ZEL_TURN_DURATION_MS` |
| `bash_start` | `ZEL_COMMAND` |
| `bash_end` | `ZEL_COMMAND`, `ZEL_OUTPUT`, `ZEL_EXIT_CODE`, `ZEL_DURATION_MS` |
| `model_request_end` | `ZEL_DURATION_MS`, `ZEL_ERROR` on failure |
| `error` | `ZEL_ERROR` |

Only necessary event data is added, not the full history or provider secrets.
Like ordinary shell commands, hooks still inherit the user's environment.

Hooks run sequentially in layer order, with a 10-second timeout per command.
Successful, nonempty stdout is added to model context at the next safe request
boundary. Pre-request hooks finish before the request; output during a turn is
steering for a subsequent request. Output left after a turn waits for the next
user-triggered turn. **Hook output never starts a turn by itself.** Closing hook
output goes only to Tracing; shutdown waits for closing hooks within their timeouts.
Hook errors and stderr go to Tracing without failing the turn.

### Output limits

Bash and hook stdout use the same byte budget and head/tail truncation. With
4096 bytes configured, Zel retains the first 2048 bytes, a truncation marker,
and the last 2048 bytes. The marker is additional to the retained-byte budget.
Output within the limit is unchanged. Zero means unlimited; the default is
8192 bytes. Hook stderr is bounded in the same way.

## Chat, Tracing, and files

Chat shows user messages, agent messages, and the working indicator. Tools,
thoughts, hook output, errors, and other events belong in Tracing; there are no
visibility toggles. Info, Tracing, and Tasks refresh while open.

Tracing search accepts a leading exact type, optionally followed by search text:
`:user`, `:assistant fix tests`, `:bash go test`, `:hook`, `:error`. Every displayed
entry type is supported; ordinary search works without a prefix.

Files browses the working directory, including hidden files, `.` and `..`.
Directories come first. Enter descends into a directory or opens a regular file
in the external editor. Search filters the current listing. Esc returns to Menu.
The chosen editor, then `$VISUAL`, then `$EDITOR`, is used, defaulting to `nvim`.
The editor shares the terminal and returns to Files when it closes.

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
- Ctrl+F: Files
- Ctrl+Q: Save and quit
- Ctrl+Y: Copy the last answer as plain text (also available in Menu → Copy answer)
- Ctrl+D: Stop the active turn (does not exit)
- Ctrl+C: reserved for terminal copying; never stops the active turn
- Ctrl+E: edit the draft externally; saving restores it without sending
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
tools, hooks and session store; it is not an interactive request protocol.

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
a sandbox. Review project hooks before trusting an unfamiliar `ZEL.md`.

## Contributing

See [Architecture](docs/ARCHITECTURE.md) for package boundaries and ownership.

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.
There is no source line-count cap.

## License

[MIT](LICENSE)
