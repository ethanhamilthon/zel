package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var envReference = regexp.MustCompile(`\{\{\$([A-Za-z_][A-Za-z0-9_]*)\}\}`)

func expand(s string) string {
	return envReference.ReplaceAllStringFunc(s, func(m string) string { return os.Getenv(m[3 : len(m)-2]) })
}
func FlagConfig(dir string) string {
	path, _ := flagConfigPath(dir)
	return path
}

func flagConfigPath(dir string) (string, error) {
	path := os.Getenv("ZEL_CONFIG")
	if path == "" {
		return "", nil
	}
	var err error
	path, err = ResolvePath(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}

// Layer is one init.lua file: the global one, the project one or the one given with --config.
type Layer struct{ Name, Path string }

// LuaLayers lists the init.lua files that apply to dir, in load order: the global
// ~/.zel/init.lua, the nearest .zel/init.lua found walking up from dir, then the
// --config file. Files that do not exist are still listed except the first two.
func LuaLayers(home, dir string) []Layer {
	return luaLayers(home, dir, FlagConfig(dir))
}

func luaLayers(home, dir, flagged string) []Layer {
	var layers []Layer
	seen := map[string]bool{}
	add := func(name, path string, mustExist bool) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		if _, err := os.Stat(path); err != nil && !mustExist {
			return
		}
		seen[path] = true
		layers = append(layers, Layer{name, path})
	}
	add("global", filepath.Join(home, "init.lua"), false)
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, ".zel", "init.lua")
		if _, err := os.Stat(candidate); err == nil {
			add("project", candidate, false)
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if flagged != "" {
		add("config", flagged, true)
	}
	return layers
}

func Load(home, dir string, preferences []byte) (Settings, error) {
	s := DefaultSettings()
	s.DBPath = filepath.Join(home, "zel.db")
	if len(preferences) > 0 {
		if err := json.Unmarshal(preferences, &s); err != nil {
			return s, fmt.Errorf("preferences: %w", err)
		}
	}
	flagged, err := flagConfigPath(dir)
	if err != nil {
		return s, err
	}
	if flagged != "" {
		if filepath.Ext(flagged) != ".lua" {
			return s, fmt.Errorf("--config %s: only .lua files are supported", flagged)
		}
		if _, err := os.Stat(flagged); err != nil {
			return s, fmt.Errorf("--config: %w", err)
		}
	}
	if s.Effort == "" {
		s.Effort = "medium"
	}
	for name, p := range s.Providers {
		p.APIKey = expand(p.APIKey)
		s.Providers[name] = p
	}
	return s, Validate(s)
}
