# Example configuration

- `init.lua`: global config (`~/.config/zel/init.lua`), wiring `defaults`, custom functions, hooks, and commands.
- `zel.lua`: project-local config (`./zel.lua` in the start folder).
- `connect.json`: providers and the active model (`~/.config/zel/connect.json`).
- `lua/web.lua`: a module loaded with `loads`: a `web_search` function (Tavily).

Use them in `~/.config/zel/`; `zel.lua` goes into the project folder. Run `zel docs config` for all options.
