# settings.toml example

- `settings.toml`: providers, settings and colors.
- `instructions/*.toml`: instructions, including one only for `--mode plan`.
- `hooks/*.toml`: notifications, terminal progress and a guard for force pushes.
- `prompts/*.toml`: a prompt completion.

Use this layout in `~/.config/zel/`; the reference lists `[keys]` bindings too.
- `web-search` - a standalone script: web search via the Tavily API. Invoke it by its path or install it on your normal `PATH`.

The tests in `internal/config` load the toml file, so it stays valid.
