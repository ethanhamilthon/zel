# init.lua examples

- `global-init.lua` - global `~/.zel/init.lua`: instructions, a dynamic prompt piece and a session hook
- `web-plugin.lua` - a module with the `web.search` (ddgs) and `web.fetch` (trafilatura, fallback crwl) tools
- `project-init.lua` - project `.zel/init.lua`: instructions from AGENTS.md, the `zel.check` tool and a `turn_end` hook

They are loaded by the tests in `internal/core/lua_test.go`, so they stay valid.
