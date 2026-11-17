# Zel — An AI Coding Agent for Peace of Mind

Zel is built around one idea: making it simple and efficient to guide your
agent's work. No complicated architecture, overengineered plugins, or entire
system to learn before you can get started.

Zel is an agent that simply writes bash. Through that one tool, it can run
background tasks and subagents, view images, work with files, and more.

One file, `zel.toml`, lets you tailor Zel to yourself and your project,
including the look of the terminal interface. The TUI is as minimal as it
gets: just you and your agent.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

macOS and Linux, arm64 and amd64. Add `~/.local/bin` to `PATH`.
Run `zel update` to get the latest release.

## Get started

Run `zel` in your project folder. On the first launch, add your provider's
API URL and key, then choose a model.

Ctrl+K opens the menu for models, sessions and settings. Type `@` for files
or `#` for prompt completions. Ctrl+D stops the current turn; Ctrl+Q saves
and quits. Bash calls and other details live in Tracing, not the chat.

The composer follows the conversation and working indicator, rather than being
pinned to the bottom. Model and context details are in Info, not in the chat.

The interface uses tcell. Drag text to select and copy it, or use Shift+drag
for native terminal selection. Click in the input to place the cursor;
Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Some shortcuts need an
enhanced keyboard protocol; the Ctrl+K menu works in legacy terminals too.

**No sandbox:** commands run with your user permissions and network access.
Only use configuration files and tools you trust.

## Make it yours

Zel reads `~/.zel/zel.toml`, then your project's `./zel.toml`, then any
`--config FILE` layers. Later layers override earlier ones.

Use these files to set instructions, providers, colors, hooks and prompt
completions. Describe your own tools in your instructions and invoke scripts
by their paths, or install them on your normal `PATH`. No plugin framework needed.

Configuration reloads automatically. `zel config` shows the merged result.
See [configuration examples](docs/examples/) for a starting point.

## Documentation

- [Reference](docs/bash-api.md): commands, configuration, hooks and colors.
  Also available in the terminal with `zel docs`.
- [Examples](docs/examples/): complete configurations and a script tool.
- [Architecture](docs/ARCHITECTURE.md): project structure and ownership.
- [0.8.0 release](docs/RELEASE-0.8.0.md): a smaller tcell frontend with a shared editor and direct terminal rendering.
- [0.7.6 release](docs/RELEASE-0.7.6.md): choice answers in the chat, composer-grade custom answers, steadier updates, completed tokens.
- [0.7.5 release](docs/RELEASE-0.7.5.md): improvements, compatibility and verification evidence.

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
