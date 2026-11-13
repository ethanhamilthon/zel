# init.lua examples

- `global-init.lua` - global `~/.zel/init.lua`: instructions, defaults, a dynamic prompt piece and a session hook
- `web-plugin.lua` - `web.search` (ddgs) and `web.fetch` (trafilatura, fallback crwl) as plugin functions
- `project-init.lua` - project `.zel/init.lua`: instructions from AGENTS.md, `zel.check`, hooks and project state

They are loaded by the tests in `internal/core/lua_test.go`, so they stay valid.
