-- lua/web.lua: a module for `loads = { "lua/web.lua" }` in init.lua.
-- It has the same shape as init.lua, but no `loads` inside.
-- web_search needs TAVILY_API_KEY in the environment.
local zel = require("zel")

return {
  functions = {
    web_search = {
      description = "search the web (Tavily). args: query, max (default 5). returns a list of {title, url, content}",
      fn = function(a)
        local body = '{"query":' .. string.format("%q", a.query) .. ',"max_results":' .. (a.max or 5) .. '}'
        local r = zel.sh(
          [[curl -sS https://api.tavily.com/search -H "Authorization: Bearer $TAVILY_API_KEY" ]]
            .. [[-H 'Content-Type: application/json' -d @-]],
          { stdin = body, timeout = 20 })
        if not r.ok then error("web_search: EFAIL: " .. r.out) end
        return r.out
      end,
    },
  },
}
