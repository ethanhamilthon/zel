package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
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
	if s.Color == "" {
		s.Color = "terracotta"
	}
	accent, ok := AccentColors[s.Color]
	if !ok {
		return s, fmt.Errorf("unknown project color %q", s.Color)
	}
	s.UI.Theme = map[string]string{"accent": accent}
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
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	body := text
	if strings.HasPrefix(text, "---\n") {
		lines := strings.Split(text[4:], "\n")
		end := slices.Index(lines, "---")
		if end < 0 {
			return fmt.Errorf("%s: unclosed YAML front matter", path)
		}
		front, rest := strings.Join(lines[:end], "\n"), strings.Join(lines[end+1:], "\n")
		var f struct {
			Name        *string   `yaml:"name"`
			Description *string   `yaml:"description"`
			Color       *string   `yaml:"color"`
			Hooks       yaml.Node `yaml:"hooks"`
		}
		dec := yaml.NewDecoder(strings.NewReader(front))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: %w", path, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return fmt.Errorf("%s: front matter must contain one YAML document", path)
		}
		for dst, v := range map[*string]*string{&s.Name: f.Name, &s.Description: f.Description, &s.Color: f.Color} {
			if v != nil {
				*dst = *v
			}
		}
		if f.Hooks.Kind != 0 && f.Hooks.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: hooks must map events to commands", path)
		}
		seenEvents := map[string]bool{}
		for i := 0; i < len(f.Hooks.Content); i += 2 {
			event, node := f.Hooks.Content[i].Value, f.Hooks.Content[i+1]
			if seenEvents[event] {
				return fmt.Errorf("%s: duplicate hook event %q; use a command list", path, event)
			}
			seenEvents[event] = true
			if !slices.Contains(HookEvents, event) {
				return fmt.Errorf("%s: unknown hook event %q", path, event)
			}
			var commands []string
			if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
				commands = []string{node.Value}
			} else if node.Kind == yaml.SequenceNode {
				if err := node.Decode(&commands); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
			} else {
				return fmt.Errorf("%s: hook %s requires a command or list of commands", path, event)
			}
			for _, command := range commands {
				if strings.TrimSpace(command) == "" {
					return fmt.Errorf("%s: empty hook command", path)
				}
				s.Hooks = append(s.Hooks, Hook{Name: path + ":" + event, Events: []string{event}, Run: command, Dir: filepath.Dir(path)})
			}
		}
		body = rest
	}
	if strings.TrimSpace(body) != "" {
		s.System = append(s.System, SystemBlock{Name: path, Value: body})
	}
	return nil
}
