package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath expands a leading home marker using the current user's actual home.
func ResolvePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// DisplayPath shortens absolute paths under the current user's home to ~ paths.
func DisplayPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "~"
	}
	return "~/" + filepath.ToSlash(rel)
}

func ZelHome(cwd string) (string, error) {
	dir := os.Getenv("ZEL_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "zel")
	} else {
		var err error
		dir, err = ResolvePath(dir)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

// StateHome is the directory where sessions and runtime sockets live (~/.zel).
func StateHome(cwd string) (string, error) {
	if os.Getenv("ZEL_HOME") != "" {
		dir, err := ZelHome(cwd)
		if err != nil {
			return "", err
		}
		return filepath.Join(filepath.Dir(dir), ".zel-"+filepath.Base(dir)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".zel"), nil
}

// DefaultPaths returns the config file paths for a session. When configFlag is
// set, it replaces init.lua and local zel.lua is disabled.
func DefaultPaths(home, cwd string, configFlag string) Paths {
	connect := filepath.Join(home, "connect.json")
	if configFlag != "" {
		p, err := ResolvePath(configFlag)
		if err != nil {
			p = configFlag
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return Paths{Init: p, Local: "", Connect: connect}
	}
	local := ""
	if cwd != "" {
		local = filepath.Join(cwd, "zel.lua")
	}
	return Paths{
		Init:    filepath.Join(home, "init.lua"),
		Local:   local,
		Connect: connect,
	}
}

const defaultInitLua = `-- Zel configuration. Edits apply by themselves. Reference: zel docs config
-- Project-local overrides live in ./zel.lua.
-- Providers and active model selection live in connect.json.
--
-- local zel = require("zel")
--
-- return {
--   settings = {
--     compaction = 400000,
--     timeout = 30,
--     output = 8000,
--     show_code = true,
--     show_prompt = false,
--     trace_hooks = true,
--     editor = "nvim",
--     -- colors = { accent = "#c3e88d", trace = { lua = "#a0a0a0" } },
--     -- keys = { menu = "ctrl+k" },
--     -- env = { EDITOR = "true" },
--   },
--   systems = {
--     -- style = "Reply short.",
--   },
--   prompts = {
--     -- review = "Review the diff of the current branch.",
--   },
--   hooks = {
--     -- { events = {"idle"}, async = true, fn = function() end },
--   },
-- }
return {}
`

const defaultConnectJSON = `{
  "provider": "",
  "model": "",
  "effort": "medium",
  "providers": {}
}
`

// EnsureHome creates the Zel home with default init.lua, connect.json and
// .gitignore. Existing files stay untouched.
func EnsureHome(home string) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	files := map[string]string{
		"init.lua":     defaultInitLua,
		"connect.json": defaultConnectJSON,
		".gitignore":   ".env\n",
	}
	for name, text := range files {
		f, err := os.OpenFile(filepath.Join(home, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
