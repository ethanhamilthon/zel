-- ./zel.lua: the local config, read only in the folder where the session started.
-- It is merged over init.lua: same-named settings, systems, prompts, functions and commands replace.
return {
  settings = { env = { GOFLAGS = "-count=1" } },
  systems = {
    project = "This is a Go project. Run `make test` before you say a task is done.",
  },
  prompts = {
    release = "Prepare the release notes from the commits since the last tag.",
  },
  hooks = {
    { events = { "lua_after" }, fn = function(event, data)
        if data.code:find("make test", 1, true) and not data.ok then
          return "tests failed, fix them before going on"
        end
      end },
  },
}
