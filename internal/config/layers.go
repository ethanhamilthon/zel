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

// Layer is one init.lua file: the global one or the project one.
type Layer struct{ Name, Path string }

// LuaLayers lists the init.lua files that apply to dir, in load order: the global
// ~/.zel/init.lua, then the nearest .zel/init.lua found walking up from dir.
// Files that do not exist are left out.
func LuaLayers(home, dir string) []Layer {
	var layers []Layer
	add := func(name, path string) {
		path = filepath.Clean(path)
		if _, err := os.Stat(path); err != nil {
			return
		}
		for _, l := range layers {
			if l.Path == path {
				return
			}
		}
		layers = append(layers, Layer{name, path})
	}
	add("global", filepath.Join(home, "init.lua"))
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, ".zel", "init.lua")
		if _, err := os.Stat(candidate); err == nil {
			add("project", candidate)
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return layers
}

func Load(home string, preferences []byte) (Settings, error) {
	s := DefaultSettings()
	s.DBPath = filepath.Join(home, "zel.db")
	if len(preferences) > 0 {
		if err := json.Unmarshal(preferences, &s); err != nil {
			return s, fmt.Errorf("preferences: %w", err)
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
