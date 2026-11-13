-- Copy to ./.zel/init.lua of a Go project: instructions, a project function, hooks, state.
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

zel.fn{
  name = "zel.check", summary = "gofmt + vet + race tests, returns only failures",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "{ok:boolean, failures:string[]}",
  examples = {'local r = zel.check("./internal/agent/...")'},
  run = function(pkg)
    local fails = {}
    local function add(out, pat)
      for l in out:gmatch("[^\n]+") do if not pat or l:find(pat) then fails[#fails + 1] = l end end
    end
    add(sh("gofmt -l .").out)                                   -- любой вывод = неотформатированный файл
    local v = sh("go vet " .. pkg); if not v.ok then add(v.out, "%.go:") end
    local t = sh("go test -race -count=1 " .. pkg, {timeout = 300}); if not t.ok then add(t.out, "FAIL") end
    state.set("last_check", {ok = #fails == 0, at = os.time()}, "project")
    return {ok = #fails == 0, failures = fails}
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
