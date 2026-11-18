-- ~/.config/zel/init.lua: the global config. Copy it and cut what you do not need.
local zel = require("zel")

return {
  settings = {
    compaction = 400000,
    timeout = 30,
    show_code = true,
    show_prompt = false,
    trace_hooks = true,
    editor = "nvim",
    colors = { accent = "#c3e88d" },
    keys = { menu = "alt+k" },
  },

  systems = {
    style = "Reply short. Prefer plain words.",
    git = "git branch: " .. zel.sh("git branch --show-current").out,
    -- config code checks the mode instead of the old `modes` key
    plan = zel.session.mode == "main" and "Plan first: list the steps, then do them." or nil,
  },

  prompts = {
    review = "Review the diff of the current branch. List bugs first, style last.",
    plan = "Write a short plan. Do not change any file until I agree.",
  },

  functions = {
    server_sh = {
      description = "run bash on mainserver. args: cmd. returns like sh()",
      fn = function(a)
        return zel.sh("ssh mainserver 'bash -s'", { stdin = a.cmd })
      end,
    },
  },

  hooks = {
    -- guard: no force push
    { events = { "lua_before" }, fn = function(event, data)
        if data.code:find("git push %-%-force") or data.code:find("git push %-f") then
          return false, "no force push"
        end
      end },

    -- ask the agent to commit before it stops
    { events = { "turn_end" }, fn = function(event, data)
        if zel.sh("git status --short").out ~= "" then
          return "uncommitted changes, commit them"
        end
      end },

    -- terminal progress bar (OSC 9;4) on /dev/tty
    { events = { "turn_start", "turn_end", "error", "session_closed" }, async = true,
      fn = function(event)
        local code = ({ turn_start = "3", error = "2;100" })[event] or "0"
        local tty = io.open("/dev/tty", "w")
        if tty then tty:write("\27]9;4;" .. code .. "\7"); tty:close() end
      end },

    -- notification when the agent is idle
    { events = { "idle" }, async = true, fn = function()
        zel.sh([[osascript -e 'display notification "done" with title "zel"']])
      end },
  },

  loads = { "lua/web.lua" },
}
