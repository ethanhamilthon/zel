# Zel — An AI Coding Agent for Peace of Mind

Zel is built around one idea: making it simple and efficient to guide your
agent's work. No complicated architecture, overengineered plugins, or entire
system to learn before you can get started.

Zel is an agent that simply writes bash. Through that one tool, it can run
background tasks and subagents, view images, work with files, and more.

One file, `~/.zel/config.toml`, lets you tailor Zel to yourself, including the
look of the terminal interface. A project's own rules come in through your
instructions, for example `"${{ cat AGENTS.md }}"`. The TUI is as minimal as it
gets: just you and your agent.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

macOS and Linux, arm64 and amd64. Add `~/.local/bin` to `PATH`.
Run `zel update` to replace the binary with the latest release; it touches
nothing else.

## Get started

Run `zel` once: it creates `~/.zel` with a commented `config.toml`. Add your
provider there (`zel docs config`), run `zel` in your project folder, and choose
a model with Ctrl+M. Without a provider Zel exits with an error.

Ctrl+K opens the menu for models, sessions and Info. Type `@` for files
or `#` for prompt completions. Ctrl+D stops the current turn; Ctrl+Q saves
and quits. Bash calls and other details live in Tracing, not the chat.

The composer follows the conversation and working indicator, rather than being
pinned to the bottom. Model and context details are in Info, not in the chat.

The interface uses tcell and leaves the mouse to the terminal: select and copy
text, and open links, the way your terminal does. PgUp/PgDn scroll the chat.
Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Some shortcuts need an
enhanced keyboard protocol; the Ctrl+K menu works in legacy terminals too.

**No sandbox:** commands run with your user permissions and network access.
Only use configuration files and tools you trust.

## Make it yours

Zel reads one file, `~/.zel/config.toml`. `--config FILE` uses another file
instead (not on top of it). Everything else Zel writes is in `~/.zel/.sessions/`.

Use the file to set instructions, providers, colors, hooks and prompt
completions. Describe your own tools in your instructions and invoke scripts
by their paths, or install them on your normal `PATH`. No plugin framework needed.

The config reloads automatically; Zel only reads it, except that choosing a
model writes `provider`, `model` and `effort`. `zel config` shows the result.
See [configuration examples](docs/examples/) for a starting point.

## Documentation

- [Reference](docs/bash-api.md): commands, configuration, hooks and colors.
  Also available in the terminal with `zel docs`.
- [Examples](docs/examples/): complete configurations and a script tool.
- [Architecture](docs/ARCHITECTURE.md): project structure and ownership.
- [Changelog](CHANGELOG.md): what changed in each release.

For non-interactive use: `zel -p "Review this project"`.
Add `--json` for structured output or `--resume SESSION` to continue a session.

## Build and contribute

Go 1.27.3+ and Make are required.

```sh
make install     # installs ~/.local/bin/zel
make dev         # runs with isolated ~/.zeldev data
make test        # formatting, vet and race tests
```

Keep code, UI and documentation in English. Run `make test` before committing.

## License

[MIT](LICENSE)
