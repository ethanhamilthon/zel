-- Copy into ~/.zel/init.lua (or ~/.zel/lua/web.lua and require it): web.search and web.fetch
-- as plugin functions. The core has only raw web.request/get/post.
local zel = require("zel")
zel.fn{
  name = "web.search", summary = "DuckDuckGo search via ddgs",
  params = {{"q", "string"}, {"n", "integer?", default = 5}},
  returns = "{title,url,body}[]",
  examples = {'for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end'},
  run = function(q, n)
    local out = fs.tmp("ddgs.json")
    sh(("ddgs text -q %q -m %d -nc -o %s"):format(q, n, out), {check = true, timeout = 30})
    local hits = {}
    for i, h in ipairs(json.decode(fs.read(out))) do hits[i] = {title = h.title, url = h.href, body = h.body} end
    return hits
  end,
}
zel.fn{
  name = "web.fetch", summary = "page as markdown (trafilatura, fallback crwl)",
  params = {{"url", "string"}, {"max", "integer?", "cut to N bytes", default = 20000}},
  returns = "string",
  run = function(url, max)
    local r = sh(("trafilatura -u %q --markdown --no-comments --links"):format(url), {timeout = 40})
    if not r.ok or r.out == "" then r = sh(("crwl crawl %q -o markdown"):format(url), {check = true}) end
    return r.out:sub(1, max)
  end,
}
