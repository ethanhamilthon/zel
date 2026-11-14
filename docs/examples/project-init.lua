-- Copy to ./.zel/init.lua of a Go project: instructions, a project function, hooks, state.
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

-- A Lua call lives 30 s, so the slow checks run as one background task.
zel.fn{
  name = "zel.check", summary = "start gofmt + vet + race tests in the background, returns the task id",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "string (task id: read it with tasks.wait(id, 20) or wait for its notification)",
  examples = {'local id = zel.check("./internal/agent/...")'},
  run = function(pkg)
    pkg = pkg or "./..."
    local id = tasks.spawn(function()
      local r = sh('test -z "$(gofmt -l .)" && go vet ' .. pkg .. ' && go test -race -count=1 ' .. pkg)
      print(r.out)
      return r.ok
    end, {name = "check", timeout = 600})
    state.set("last_check", {id = id, at = os.time()}, "project")
    return id
  end,
}

zel.on("turn_end", function()
  if sh("git status --porcelain").out ~= "" then
    return "uncommitted changes: run zel.check() and commit per AGENTS.md"
  end
end)
