// Package defaults embeds the default Lua functions module and init.lua.
package defaults

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed lua/*.lua
var luaFS embed.FS

//go:embed init.lua
var initLua string

// Files returns the embedded defaults files keyed by path relative to the
// defaults directory.
func Files() map[string]string {
	out := map[string]string{}
	_ = fs.WalkDir(luaFS, "lua", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := luaFS.ReadFile(path)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(path, "lua/")] = string(data)
		return nil
	})
	return out
}

// InitLua returns the default init.lua contents.
func InitLua() string {
	return initLua
}
