# Zel — Minimalist AI Coding Agent

A focused terminal coding agent with one tool: Bash. You control the system
prompt through project Markdown, providers and preferences through the UI.

## Build and run

```sh
make install     # installs ~/.local/bin/zel
zel              # opens first-run onboarding if no provider exists
make dev         # isolated ~/.zeldev data
make test        # formatting, vet, race tests
```

Add `~/.local/bin` to `PATH`. Go 1.27.3+ and Make are required.
The repository has no remote configured yet. The optional release installer
requires `ZEL_REPO=owner/repository` pointing to a repository with Zel releases.

Zel uses fresh data in `~/.zel` (override with `ZEL_HOME`). It does not read,
modify, or migrate Dashy data. There is no `setup` command or TOML configuration.

## Onboarding and settings

The first launch opens a centered, four-step provider form: Name, Base URL,
API key, API type (`responses` or `chat`). Tab / Shift+Tab switch steps,
Left / Right move the input cursor, and Enter advances and saves the last step.
An empty API key is allowed for local servers. After saving, choose a model and
reasoning effort. Model listing failures offer retry and manual model entry.
Effort defaults to `medium`.

Ctrl+K opens the main menu. Providers lets you add, edit, or delete providers;
Settings controls the external editor, Bash/hook output budget, Bash timeout,
and reasoning summaries. Providers, keys, defaults, preferences, sessions, and
tasks live in the private SQLite database `~/.zel/zel.db`. API keys are stored
locally, not encrypted; keep this directory private. Keys may reference an
environment variable with `{{$VARIABLE}}`.

## Project instructions: ZEL.md

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

- Ctrl+K: Menu
- Ctrl+M: Models
- Ctrl+R: Resume
- Ctrl+I: Info
- Ctrl+T: Tracing
- Ctrl+E: edit the draft externally; saving restores it without sending
- Esc: back; shortcut-opened pages return directly to chat

Ctrl+M and Ctrl+I require Kitty keyboard protocol support to distinguish them
from Enter and Tab; otherwise use Menu. New session and Resume are blocked
during active turns. A queued message stays editable in the composer; editing
withdraws it, and Enter queues it again.

## Automation and security

`zel json` exposes the controller over newline-delimited JSON; see
[docs/JSON.md](docs/JSON.md). `zel tasks` manages background commands, `zel view`
queues images, and `zel models` checks providers. Run `zel --help` for details.

Zel executes commands with your user permissions and network access, not inside
a sandbox. Review project hooks before trusting an unfamiliar `ZEL.md`.

## Contributing

Keep code, UI, docs, and comments in English. Run `make test` before committing
features or fixes. Prefer focused unit tests, at most two test files per package.
There is no source line-count cap.

## License

[MIT](LICENSE)
