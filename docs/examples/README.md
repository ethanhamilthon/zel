# settings.yaml example

- `settings.yaml`: providers, settings and colors.
- `systems/*.md`: system prompt snippets, including one only for `--mode plan`.
- `hooks/*.yaml`: notifications, terminal progress and a guard for force pushes.
- `prompts/*.md`: prompt completions.

Use this layout in `~/.config/zel/`; the reference lists `keys` bindings too.
- `web-search` - a standalone script: web search via the Tavily API. Invoke it by its path or install it on your normal `PATH`.

The tests in `internal/config` load the yaml file, so it stays valid.
