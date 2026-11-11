package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	ShellTimeout    = 120
	ShellMaxTimeout = 600
	ShellOutput     = 8192
	HookTimeout     = 10
)

var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Provider API types: the OpenAI Responses API (default) or Chat Completions.
const (
	APIResponses = "responses"
	APIChat      = "chat"
)

type Provider struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	API     string `json:"api"`
}

func (p Provider) key() string { return p.APIKey }

// UI holds application display preferences.
type UI struct {
	Editor string
	Theme  map[string]string `json:"-"`
}

// SystemBlock is a Markdown body from one project instruction layer.
type SystemBlock struct{ Name, Value string }

// Hook is a command subscribed to a project lifecycle event.
type Hook struct {
	Name   string
	Events []string
	Run    string
	Dir    string
}

// Settings is the effective configuration: the layers of ConfigPaths merged in
// order, plus the session's model selection.
type Settings struct {
	DBPath                    string `json:"-"`
	Provider, Model, Effort   string
	ReasoningSummary          bool
	ShellOutput, ShellTimeout int
	UI                        UI
	Providers                 map[string]Provider
	System                    []SystemBlock `json:"-"`
	Hooks                     []Hook        `json:"-"`
	Name, Description, Color  string        `json:"-"`
}

func DefaultSettings() Settings {
	return Settings{Effort: "medium", ReasoningSummary: true, ShellOutput: ShellOutput, ShellTimeout: ShellTimeout,
		UI: UI{Theme: map[string]string{}}, Providers: map[string]Provider{}}
}

func Redact(s Settings, text string, extra ...string) string {
	secrets := extra
	for _, p := range s.Providers {
		secrets = append(secrets, p.key())
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func (s Settings) SetupHint() string {
	switch {
	case s.Provider != "" && s.Model != "":
		return ""
	case len(s.Providers) == 0:
		return "No provider configured · open Providers in the menu"
	}
	return "No model selected · choose one in the Ctrl+K menu"
}

func validateSettings(s Settings) error {
	if s.ShellOutput < 0 {
		return fmt.Errorf("shell_output must be >= 0 bytes (0 = no truncation)")
	}
	if s.ShellTimeout < 1 || s.ShellTimeout > ShellMaxTimeout {
		return fmt.Errorf("shell_timeout must be 1..%d seconds", ShellMaxTimeout)
	}
	if !slices.Contains(Efforts, s.Effort) {
		return fmt.Errorf("unsupported effort %q", s.Effort)
	}
	for name, p := range s.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("provider %q needs an HTTP(S) base_url", name)
		}
		if p.API != "" && p.API != APIResponses && p.API != APIChat {
			return fmt.Errorf("provider %q: api must be %q or %q", name, APIResponses, APIChat)
		}
	}
	if s.Provider == "" {
		if s.Model != "" {
			return fmt.Errorf("model %q selected but no provider configured", s.Model)
		}
		return nil
	}
	if _, ok := s.Providers[s.Provider]; !ok {
		return fmt.Errorf("selected provider %q is not configured", s.Provider)
	}
	return nil
}

var AccentColors = map[string]string{
	"terracotta": "#d97757", "rose": "#c98798", "dusty-pink": "#c59baa", "mauve": "#b69ab8",
	"lavender": "#a79bc8", "violet": "#a18bc0", "slate": "#8e9db5", "blue": "#82a5c6",
	"sky": "#8ab6c9", "teal": "#80b3ad", "sage": "#9bb49a", "olive": "#acb18b",
	"moss": "#93a382", "sand": "#c6b38c", "ochre": "#c4a46d", "amber": "#c9a179",
	"peach": "#d3a28e", "clay": "#b88e7c", "copper": "#bd937c", "plum": "#ab8fa6",
}

var envReference = regexp.MustCompile(`\{\{\$([A-Za-z_][A-Za-z0-9_]*)\}\}`)

func Expand(s string) string { return expand(s) }
func expand(s string) string {
	return envReference.ReplaceAllStringFunc(s, func(m string) string { return os.Getenv(m[3 : len(m)-2]) })
}
func FlagConfig(dir string) string {
	path := os.Getenv("ZEL_CONFIG")
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path
}
func ConfigPaths(home, dir string) []string {
	var local []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		local = append(local, filepath.Join(d, "ZEL.md"))
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(local)
	paths := append([]string{filepath.Join(home, "ZEL.md")}, local...)
	if flagged := FlagConfig(dir); flagged != "" {
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

// LoadConfig reads database preferences and layers project instructions over them.
func LoadConfig(home, dir string) (Settings, error) {
	s := DefaultSettings()
	s.DBPath = filepath.Join(home, "zel.db")
	db, err := openDB(s.DBPath)
	if err != nil {
		return s, err
	}
	var data string
	err = db.QueryRow(`SELECT data FROM preferences WHERE id = 1`).Scan(&data)
	db.Close()
	if err == nil {
		err = json.Unmarshal([]byte(data), &s)
	} else if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return s, fmt.Errorf("preferences: %w", err)
	}
	// Stored preferences must never supply project instructions or identity.
	s.System, s.Hooks = nil, nil
	for _, path := range ConfigPaths(home, dir) {
		err := mergeConfig(&s, path)
		if errors.Is(err, os.ErrNotExist) && path != FlagConfig(dir) {
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
	return s, validateSettings(s)
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

// WriteConfig persists only application preferences, never project instructions.
func WriteConfig(home string, s Settings) error {
	if err := validateSettings(s); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	db, err := openDB(filepath.Join(home, "zel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO preferences(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(data))
	return err
}

// UpdateSettings is called between turns by the settings/provider UI.
func (a *Agent) UpdateSettings(s Settings) error {
	if err := WriteConfig(a.home, s); err != nil {
		return err
	}
	return a.ReloadConfig()
}
func ZelHome(cwd string) (string, error) {
	dir := os.Getenv("ZEL_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".zel")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

// DBPath is the SQLite state path for helper commands that run without an agent.
func DBPath(home string) string { return filepath.Join(home, "zel.db") }

func (a *Agent) DBPath() string { return a.Settings().DBPath }

// Project is the project directory of the session.
func (a *Agent) Project() string { return a.project }

// ConfigFiles are the config layers that exist, in merge order.
func (a *Agent) ConfigFiles() []string {
	var files []string
	for _, path := range ConfigPaths(a.home, a.Shell.Dir) {
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	return files
}

// settingsState is the live configuration of an agent. The turn goroutine and the
// clients share it, so every access goes through mu.
type settingsState struct {
	mu     sync.Mutex
	base   Settings
	sel    selection
	s      Settings
	client *client
}

type selection struct{ Provider, Model, Effort string }

func (a *Agent) applyLocked(base Settings) error {
	s := base
	s.Providers = maps.Clone(base.Providers)
	if sel := a.cfg.sel; sel.Model != "" {
		s.Provider, s.Model, s.Effort = sel.Provider, sel.Model, sel.Effort
	}
	if s.Effort == "" {
		s.Effort = "medium"
	}
	if err := validateSettings(s); err != nil {
		return err
	}
	a.cfg.base, a.cfg.s = base, s
	a.cfg.client = newClient(s.Providers[s.Provider])
	return nil
}

// fixSelection applies a session's stored model; one that no longer validates is dropped.
func (a *Agent) fixSelection(sel selection) {
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sel = sel
	if sel.Model == "" || a.applyLocked(c.base) != nil {
		c.sel = selection{}
		_ = a.applyLocked(c.base)
	}
}

func (a *Agent) snapshot() (Settings, *client) {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked(), a.cfg.client
}

func (a *Agent) settingsLocked() Settings {
	s := a.cfg.s
	s.Providers = maps.Clone(s.Providers)
	return s
}

func (a *Agent) Settings() Settings {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked()
}
