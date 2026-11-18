# Config examples

- `init.lua`: the global config: settings, systems, prompts, a function and hooks (a force-push guard, a commit reminder, terminal progress, a notification).
- `zel.lua`: the local config of a project, merged over `init.lua`.
- `connect.json`: providers and the selected model.
- `lua/web.lua`: a module loaded with `loads`: a `web_search` function (Tavily).

Use them in `~/.config/zel/`; `zel.lua` goes into the project folder. The reference lists `keys` bindings too.
