# Zel — An AI Coding Agent for Peace of Mind

Zel is built around one idea: making it simple and efficient to guide your
agent's work. No complicated architecture, overengineered plugins, or entire
system to learn before you can get started.

Zel is an agent that simply writes Lua. Through that one tool, it can run
shell commands, background tasks and subagents, view images, work with files, and more.

Your config is Lua: `~/.config/zel/init.lua`, an optional `zel.lua` in a project, and `connect.json` for providers. They set systems, prompts (`#name`), functions for the agent, hooks and the look of the terminal interface. A project's own rules come in through your systems, for example `systems = { agents = zel.read("AGENTS.md") }`. The TUI is as minimal as it
gets: just you and your agent.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/install.sh | sh
```

macOS and Linux, arm64 and amd64. Add `~/.local/bin` to `PATH`.
Run `zel update` to replace the binary with the latest release; it touches
nothing else.

## Get started

Run `zel` once: it creates `~/.config/zel` with a commented `init.lua` and a `connect.json`. Add your
provider to `connect.json` (`zel docs connect`), run `zel` in your project folder, and choose
a model in the picker that opens automatically. Without a provider Zel exits with an error.

Ctrl+K opens the menu for models, sessions and Info. Type `@` for files
or `#` for your prompts. Ctrl+E opens what you typed in your editor
(`nvim` by default, `settings.editor`). Ctrl+D stops the current turn; Ctrl+Q saves
and quits. Lua calls and other details live in Tracing (Ctrl+T), not the chat:
grouped by turn, with Tab to show only Lua calls, answers or errors, and `Ctrl+H`
to hide the hooks.

The composer follows the conversation and working indicator, rather than being
pinned to the bottom. Model and context details are in Info, not in the chat;
Info also lists the session's subagents (running and finished) with their model, effort, context and spend.

The interface uses tcell and leaves the mouse to the terminal: select and copy
text, and open links, the way your terminal does. PgUp/PgDn scroll the chat.
Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Some shortcuts need an
enhanced keyboard protocol; the Ctrl+K menu works in legacy terminals too.

**No sandbox:** commands run with your user permissions and network access.
Only use configuration files and tools you trust.

## Make it yours

Zel reads `~/.config/zel/init.lua` (global) and `./zel.lua` (the project where
you start it), plus `connect.json` with your providers. `--config FILE` uses
that file instead of `init.lua` and skips `zel.lua`. Sessions live in `~/.zel/sessions/`.

Both Lua files return a table: `settings`, `systems`, `prompts`, `functions`
(your own tools for the agent), `hooks` and `loads` (more files of the same
shape). Use `settings.keys` to rebind shortcuts, for example
`keys = { menu = "alt+k" }`; Ctrl/Alt shortcuts also recognize the Russian
keyboard layout. Config code can run commands and read files with `zel.sh`,
`zel.read` and the rest of the same API the agent has. No plugin framework needed.

The config reloads automatically; Zel only reads it, except that choosing a
model writes `provider`, `model` and `effort` to `connect.json`. `zel config`
shows the result. See [configuration examples](docs/examples/) for a starting point.

## Documentation

- [Reference](docs/lua-api.md): the Lua API, configuration, hooks and colors.
  Also available in the terminal with `zel docs`.
- [Examples](docs/examples/): `init.lua`, `zel.lua`, `connect.json` and a loaded module.
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
python3 scripts/verify-terminal.py BINARY OUTDIR   # real terminal check with a fake provider (needs pyte, Pillow)
```

Keep code, UI and documentation in English. Run `make test` before committing.

## License

[MIT](LICENSE)
