-- Copy to ~/.zel/lua/web.lua and add require("web") to the list your init.lua
-- returns: web.search (ddgs) and web.fetch (trafilatura, fallback crwl) as tools.
-- The core has no HTTP functions; sh("curl ...") covers raw requests.
return {
  tools = {
    ["web.search"] = {
      desc = "DuckDuckGo search via ddgs. args: q, n? (default 5). returns the hits as text",
      fn = function(a)
        local r = sh(("ddgs text -q %q -m %d --no-color"):format(a.q, a.n or 5), {check = true, timeout = 20})
        return r.out
      end,
    },
    ["web.fetch"] = {
      desc = "page as markdown (trafilatura, fallback crwl). args: url, max? (cut to N bytes, default 20000)",
      fn = function(a)
        local r = sh(("trafilatura -u %q --markdown --no-comments --links"):format(a.url), {timeout = 20})
        if not r.ok or r.out == "" then r = sh(("crwl crawl %q -o markdown"):format(a.url), {check = true}) end
        return r.out:sub(1, a.max or 20000)
      end,
    },
  },
}
