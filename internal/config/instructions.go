package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
func ConfigPaths(home, dir string) []string {
	return configPaths(home, dir, FlagConfig(dir))
}

func configPaths(home, dir, flagged string) []string {
	var local []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		local = append(local, filepath.Join(d, "ZEL.md"))
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(local)
	paths := append([]string{filepath.Join(home, "ZEL.md")}, local...)
	if flagged != "" {
		paths = append(paths, flagged)
	}
	seen := map[string]bool{}
	var unique []string
	for i := len(paths) - 1; i >= 0; i-- {
		path := filepath.Clean(paths[i])
		if !seen[path] {
			unique = append(unique, path)
			seen[path] = true
		}
	}
	slices.Reverse(unique)
	return unique
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
	for _, path := range configPaths(home, dir, flagged) {
		err := mergeConfig(&s, path)
		if errors.Is(err, os.ErrNotExist) && path != flagged {
			continue
		}
		if err != nil {
			return s, err
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

func mergeConfig(s *Settings, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if body := strings.ReplaceAll(string(data), "\r\n", "\n"); strings.TrimSpace(body) != "" {
		s.System = append(s.System, SystemBlock{Name: path, Value: body})
	}
	return nil
}
