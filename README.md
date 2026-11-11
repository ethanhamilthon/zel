# Dashy - Minimalist AI Coding Agent

A minimalistic AI coding agent where you find peace and focus.

### Minimalist
- **Single Tool:** Just Bash. One tool to inspect, build, edit, and test.
- **No Hidden System Prompt:** You control exactly how the agent thinks and acts.
- **Configured via `dashy.toml`:** Everything is configured in simple TOML layers—no complex frameworks to maintain.

### Simple
- **Under 10,000 LOC:** Clean Go codebase that any developer or agent can read and understand end-to-end.
- **`dashy setup`:** One interactive command to set up providers, models, and API keys.
- **Distraction-Free Interface:** A clean TUI with an input bar, a Ctrl+K menu, and a clear timeline.

### Powerful
- **Headless JSON Mode:** Run `dashy json` via stdin/stdout to automate workflows, build sub-agents, or connect alternative frontends (see [docs/JSON.md](docs/JSON.md)).
- **Lifecycle Hooks:** Execute shell scripts on agent events (`turn_start`, `turn_end`, `code_run`, etc.).
- **Project-Level Configuration:** Drop a `dashy.toml` in your project directory to override settings, themes, and system prompt blocks per repository.

---

The problem isn't the lack of good AI agents. The problem is that most agents either strip away your control or force you to adopt and maintain a bloated codebase.

Dashy was created to give you 100% control over the agent without drowning in configuration. Built in Go, developed with Dashy itself.

## Installation & Setup

### Quick Install (Linux & macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/dashy/main/scripts/install.sh | sh
```

Make sure `~/.local/bin` is in your `PATH`.

### Initial Setup

Run the interactive setup wizard to configure your preferred provider, API key, model, and reasoning effort:

```sh
dashy setup
```

Configuration is stored in `~/.dashy/config.toml` (or `$DASHY_HOME/config.toml`).

## Building from Source

Prerequisites: Go 1.24+ and Make.

```sh
git clone https://github.com/ethanhamilthon/dashy.git
cd dashy
make install
```

This compiles `dashy` and places the binary into `~/.local/bin/dashy`.

To run Dashy locally in development mode with an isolated dev environment:

```sh
make dev
```

Run tests and verification:

```sh
make test
```

## Keyboard and queued messages

Ctrl+K opens the menu. Ctrl+M opens Models, Ctrl+R opens Resume, and Ctrl+I
opens Info. Ctrl+T opens Tracing, and Ctrl+E edits the current input in an
external editor. Ctrl+M and Ctrl+I require a terminal with Kitty keyboard protocol
support so they can be distinguished from Enter and Tab; otherwise use Ctrl+K.
The main menu, Models, Resume, Tracing, and Efforts pages have searchable lists.
Esc returns directly to chat when a page was opened by a shortcut.
New session and Resume are unavailable while an agent turn is active.
The chat contains only user messages, agent messages, and the working indicator.
Errors and other events appear only in Tracing.
Info, Tracing, Tasks, and task output refresh while open; the current session
model is always first in Models.

The Editor menu action (Ctrl+E) opens the draft in `[ui].editor`, then `$VISUAL`,
then `$EDITOR`, falling back to `nvim`. The editor opens in the same terminal,
temporarily taking over the display and keyboard. The agent and background tasks keep running independently; UI events
are processed when the editor exits. Saving and closing restores the draft
without sending it. For example:

```toml
[ui]
editor = "nvim"
```

The input grows to the available terminal height instead of stopping halfway
down the screen; longer drafts scroll within the input.

Messages sent during a turn stay in the composer with a pulsing left rail until
the agent accepts them. Editing a queued message withdraws it into a draft;
press Enter to queue or send the edited draft again.

## Configuration

Dashy merges configuration layers in order:
1. Global configuration: `$DASHY_HOME/config.toml` (default `~/.dashy/config.toml`)
2. Hierarchical project configs: `dashy.toml` from the filesystem root down to the current project directory
3. Command-line flag: `--config <file>`

Reasoning effort defaults to `medium` when omitted or empty, including in older
saved sessions. The effort picker has no separate `default` option.

Example `dashy.toml`:

```toml
[agent]
provider = "anthropic"
model = "claude-3-7-sonnet"
effort = "high"

[ui.theme]
accent = "#d97757"
heading = "#d97757"
assistant = "#d97757"

[system.instructions]
type = "text"
value = "Keep responses concise and direct."

[hooks.format]
events = ["turn_end"]
run = "go fmt ./..."
```

See `dashy --help` for full configuration options and environment variables.

## Security

Dashy runs commands directly on your system using your user account permissions and network access. It is not an isolated sandbox or container.

API keys and secrets can be specified directly or interpolated securely from environment variables using `{{$VAR}}` syntax.

## Contributing

Contributions are welcome! Please follow these guidelines:
- **Code size:** Keep total non-test Go source code strictly under 10,000 lines (`find . -name '*.go' ! -name '*_test.go' | xargs cat | wc -l`).
- **Language:** English only for code, comments, UI text, and documentation.
- **Tests & Quality:** Ensure `make test` (gofmt, `go vet`, race tests) passes cleanly before submitting.
- **Commits:** Make concise, focused commits with clear messages.

## License

Dashy is licensed under the [MIT License](LICENSE).
