-- Copy to ~/.zel/init.lua: shared instructions and defaults.
local zel = require("zel")
zel.defaults{model = "sonnet-5.5", effort = "medium"}
zel.instructions [[
Read code first. Work read-only unless the request needs edits. Verify before the final answer.
Reply like an SMS: short, lowercase, user's language, no trailing dot.
Subagents: cheap work → haiku-5.5, real code → sonnet-5.5, review/hard → opus-5.5.
]]
zel.prompt(function() return "env: " .. sh("uname -srm").out .. "today: " .. os.date("%Y-%m-%d") end)
zel.on("session_start", function() sh("mkdir -p ~/.zel/cache") end)
