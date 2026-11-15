-- Copy to ./.zel/init.lua of a Go project: instructions, a tool and hooks.
return {
  instructions = {
    function() return exists("AGENTS.md") and read("AGENTS.md") or nil end,
    function() return "git status:\n" .. sh("git status --short").out end,
  },

  tools = {
    -- A Lua call lives 30 s, so the slow checks run as one background task.
    ["zel.check"] = {
      desc = [==[start gofmt + vet + race tests in the background. args: pkg? (default ./...)
returns the task id: wait for its notification or tasks.wait(id, 20)]==],
      fn = function(a)
        local pkg = a.pkg or "./..."
        return tasks.spawn("go checks", function()
          local r = sh('test -z "$(gofmt -l .)" && go vet ' .. pkg .. ' && go test -race -count=1 ' .. pkg, {timeout = 600})
          print(r.out)
          return r.ok
        end)
      end,
    },
  },

  hooks = {
    turn_end = function(answer, running)
      if #running > 0 then return end -- the agent wakes up when they finish
      if sh("git status --porcelain").out ~= "" then
        return "uncommitted changes: run zel.check{} and commit per AGENTS.md"
      end
    end,
  },
}
