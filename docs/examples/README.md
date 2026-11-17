# config.toml example

- `config.toml` - `~/.zel/config.toml`: providers, settings, instructions (one only for `--mode plan`), hooks (a desktop notification, the terminal progress bar, a guard for `git push --force`) and a completion
- `web-search` - a standalone script: web search via the Tavily API. Invoke it by its path or install it on your normal `PATH`.

The tests in `internal/config` load the toml file, so it stays valid.
