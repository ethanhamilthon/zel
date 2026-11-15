-- Copy to ~/.zel/init.lua. A file returns a table, or a list of them; modules in
-- ~/.zel/lua are loaded with require and merged the same way.
return {
  instructions = {
    [==[
Read code first. Work read-only unless the request needs edits. Verify before the final answer.
Reply like an SMS: short, lowercase, user's language, no trailing dot.
]==],
    function() return "env: " .. sh("uname -srm").out .. "today: " .. os.date("%Y-%m-%d") end,
  },
  hooks = {
    session_created = function() sh("mkdir -p ~/.zel/cache") end,
  },
}
