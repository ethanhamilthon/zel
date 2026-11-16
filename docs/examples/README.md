# zel.toml examples

- `global-zel.toml` - `~/.zel/zel.toml`: providers, settings, instructions, hooks (a desktop notification, the terminal progress bar) and a completion
- `project-zel.toml` - `./zel.toml` of a Go repository: AGENTS.md as instructions, a `turn_end` hook and a guard for `git push --force`
- `web-search` - a standalone script: web search via the Tavily API. Invoke it by its path or install it on your normal `PATH`.

The tests in `internal/config` load the toml files, so they stay valid.