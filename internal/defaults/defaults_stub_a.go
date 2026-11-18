package defaults

// Stub of workstream A until workstream C's internal/defaults is merged: the
// real package has the same two functions over an embed.FS. Delete this file
// when it arrives.

// Files maps a path relative to <home>/defaults to its text.
func Files() map[string]string { return map[string]string{"defaults.lua": stubModule} }

// InitLua is the default init.lua.
func InitLua() string { return stubInit }

const stubModule = `local M = {}

local function entry(description, fn)
	return setmetatable({description = description, fn = fn}, {__call = function(self, ...) return self.fn(...) end})
end

M.sh = entry("` + "`sh(cmd) -> {ok, code, out}`" + ` - bash -c (stub of the defaults)", function(cmd)
	local p = io.popen("bash -c '" .. cmd:gsub("'", "'\\''") .. "' 2>&1")
	local out = p:read("*a")
	local ok, _, code = p:close()
	return {ok = ok and true or false, code = code, out = out}
end)

return M
`

const stubInit = `-- Zel configuration. Reference: zel docs config
local d = require("defaults")

return {
  functions = {
    sh = d.sh,
  },
}
`
