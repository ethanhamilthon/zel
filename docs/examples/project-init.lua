-- Copy to ./.zel/init.lua of a Go project: instructions, a project function, hooks, state.
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

-- A Lua call lives 10 s, so the slow checks run as one background task.
zel.fn{
  name = "zel.check", summary = "start gofmt + vet + race tests in the background, returns the task id",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "string (task id: read it with tasks.wait(id, 8) or wait for the <task-result>)",
  examples = {'local id = zel.check("./internal/agent/...")'},
  run = function(pkg)
    pkg = pkg or "./..."
    local id = tasks.spawn('test -z "$(gofmt -l .)" && go vet ' .. pkg .. ' && go test -race -count=1 ' .. pkg,
      {name = "check", timeout = 600})
    state.set("last_check", {id = id, at = os.time()}, "project")
    return id
  end,
}

zel.on("before_call", function(ev)
  if ev.fn == "sh" and ev.args[1]:match("^go test") and not ev.args[1]:find("-race") then
    ev.args[1] = ev.args[1]:gsub("^go test", "go test -race")
    return {args = ev.args}
  end
end)
zel.on("turn_end", function()
  if sh("git status --porcelain").out ~= "" then
    return {inject = "uncommitted changes: run zel.check() and commit per AGENTS.md"}
  end
end)
