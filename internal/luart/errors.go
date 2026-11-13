package luart

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// Error codes of "fn: CODE: message" errors raised to Lua.
const (
	EINVAL    = "EINVAL"
	ENOENT    = "ENOENT"
	ENOMATCH  = "ENOMATCH"
	EAMBIG    = "EAMBIG"
	ELIMIT    = "ELIMIT"
	EFAIL     = "EFAIL"
	EDENIED   = "EDENIED"
	ETIMEOUT  = "ETIMEOUT"
	ECANCELED = "ECANCELED"
)

// raise throws "fn: CODE: msg" as a plain string, without a position prefix, so
// scripts can match it with pcall and string.find.
func raise(L *lua.LState, fn, code, format string, args ...any) {
	L.Error(lua.LString(fn+": "+code+": "+fmt.Sprintf(format, args...)), 0)
}
