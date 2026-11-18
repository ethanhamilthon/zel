-- Zel configuration. Reference: zel docs config
-- Project-local overrides live in ./zel.lua.
-- Providers and active model selection live in connect.json.

local d = require("defaults")

return {
  functions = {
    sh = d.sh,
    read = d.read,
    write = d.write,
    edit = d.edit,
    slice = d.slice,
    list = d.list,
    glob = d.glob,
    grep = d.grep,
    exists = d.exists,
    cd = d.cd,
    docs = d.docs,
    models = d.models,
    session = d.session,
    rename = d.rename,
    choice = os.getenv("ZEL_MODE") ~= "headless" and d.choice or nil,
  },
  -- settings = {
  --   compaction = 400000,
  --   timeout = 30,
  --   output = 8000,
  --   show_prompt = false,
  --   trace_hooks = true,
  --   editor = "nvim",
  --   -- colors = { accent = "#c3e88d", trace = { lua = "#a0a0a0" } },
  --   -- keys = { menu = "ctrl+k" },
  --   -- env = { EDITOR = "true" },
  -- },
  -- systems = {
  --   -- style = "Reply short.",
  -- },
  -- prompts = {
  --   -- review = "Review the diff of the current branch.",
  -- },
  -- hooks = {
  --   -- { events = {"idle"}, async = true, fn = function() end },
  -- },
  -- commands = {
  --   -- { name = "Status", description = "git status", key = "alt+g", fn = function() return d.sh("git status -s").out end },
  -- },
}
