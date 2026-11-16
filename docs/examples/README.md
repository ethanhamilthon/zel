# zel.toml examples

- `global-zel.toml` - `~/.zel/zel.toml`: providers, settings, instructions, hooks and a completion
- `project-zel.toml` - `./zel.toml` of a Go repository: AGENTS.md as instructions, a `turn_end` hook and a guard for `git push --force`
- `bin/web-search` - a script tool for `~/.zel/bin`: web search via the Tavily API

The tests in `internal/config` load the toml files, so they stay valid.