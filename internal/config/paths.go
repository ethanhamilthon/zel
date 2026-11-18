package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"zel/internal/defaults"
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

// DefaultPaths returns the config files of a session: the default init.lua
// and ./zel.lua (unless noDefaults), then the configs of --config, made
// absolute against cwd.
func DefaultPaths(home, cwd string, configs []string, noDefaults bool) Paths {
	p := Paths{Connect: filepath.Join(home, "connect.json")}
	if !noDefaults {
		p.Files = append(p.Files, filepath.Join(home, "init.lua"))
		if cwd != "" {
			p.Files = append(p.Files, filepath.Join(cwd, "zel.lua"))
		}
	}
	for _, c := range configs {
		if c == "" {
			continue
		}
		path, err := ResolvePath(c)
		if err != nil {
			path = c
		}
		if !filepath.IsAbs(path) && cwd != "" {
			path = filepath.Join(cwd, path)
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		p.Files = append(p.Files, path)
		p.Required = append(p.Required, path)
	}
	return p
}

const defaultConnectJSON = `{
  "provider": "",
  "model": "",
  "effort": "medium",
  "providers": {}
}
`

// DefaultsDir is the folder of the Lua defaults module in the Zel home.
func DefaultsDir(home string) string { return filepath.Join(home, "defaults") }

// WriteDefaults writes the files of internal/defaults into <home>/defaults.
func WriteDefaults(home string) error {
	for rel, text := range defaults.Files() {
		path := filepath.Join(DefaultsDir(home), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			return err
		}
	}
	return nil
}

// EnsureHome creates the Zel home with the default init.lua, defaults/,
// connect.json and .gitignore. Existing files and folders stay untouched.
func EnsureHome(home string) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	if _, err := os.Stat(DefaultsDir(home)); errors.Is(err, os.ErrNotExist) {
		if err := WriteDefaults(home); err != nil {
			return err
		}
	}
	files := map[string]string{
		"init.lua":     defaults.InitLua(),
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
