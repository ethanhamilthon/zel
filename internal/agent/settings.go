package agent

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
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
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	API     string `toml:"api"`
}

func (p Provider) key() string { return p.APIKey }

// UI holds the [ui] display options: event toggles and theme colors.
type UI struct {
	Editor string
	Events map[string]bool
	Theme  map[string]string
}

// Show reports whether an [ui.events] toggle is on; missing toggles are on.
func (u UI) Show(event string) bool {
	on, ok := u.Events[event]
	return on || !ok
}

// SystemBlock is one [system.NAME] prompt block: type "text" or "bash".
type SystemBlock struct{ Name, Type, Value string }

// Hook is one [hooks.NAME] command run on agent events.
type Hook struct {
	Name        string
	Events      []string
	Run         string
	Timeout     int
	Enabled     bool
	PasteOutput bool
}

// Settings is the effective configuration: the layers of ConfigPaths merged in
// order, plus the session's model selection.
type Settings struct {
	DBPath, Provider, Model, Effort string
	ReasoningSummary                bool
	ShellOutput, ShellTimeout       int
	UI                              UI
	Providers                       map[string]Provider
	System                          []SystemBlock
	Hooks                           []Hook
	Sources                         map[string]string // config file that last set provider, model or effort
}

func DefaultSettings() Settings {
	return Settings{Effort: "medium", ReasoningSummary: true, ShellOutput: ShellOutput, ShellTimeout: ShellTimeout,
		UI: UI{Events: map[string]bool{}, Theme: map[string]string{}}, Providers: map[string]Provider{}, Sources: map[string]string{}}
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
		return "No provider configured · run dashy setup or edit config.toml"
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

// fileConfig is the config.toml schema; pointers tell set values from missing ones.
type fileConfig struct {
	Agent struct {
		DBPath           *string `toml:"db_path"`
		Provider         *string `toml:"provider"`
		Model            *string `toml:"model"`
		Effort           *string `toml:"effort"`
		ReasoningSummary *bool   `toml:"reasoning_summary"`
		ShellOutput      *int    `toml:"shell_output"`
		ShellTimeout     *int    `toml:"shell_timeout"`
	} `toml:"agent"`
	UI struct {
		Editor *string `toml:"editor"`
		// The line caps are gone: the chat shows user messages and answers only, and the
		// trace of a turn lives in the Tracing menu. They stay parsed so that an
		// older config.toml still loads.
		BashLines     *int              `toml:"bash_lines"`
		OutputLines   *int              `toml:"output_lines"`
		ThinkingLines *int              `toml:"thinking_lines"`
		Events        map[string]bool   `toml:"events"`
		Theme         map[string]string `toml:"theme"`
	} `toml:"ui"`
	Provider map[string]struct {
		BaseURL *string `toml:"base_url"`
		APIKey  *string `toml:"api_key"`
		API     *string `toml:"api"`
	} `toml:"provider"`
	System map[string]struct {
		Type  string `toml:"type"`
		Value string `toml:"value"`
	} `toml:"system"`
	Hooks map[string]struct {
		Events      []string `toml:"events"`
		Run         string   `toml:"run"`
		Timeout     int      `toml:"timeout"`
		Enabled     *bool    `toml:"enabled"`
		PasteOutput bool     `toml:"paste_output"`
	} `toml:"hooks"`
}

var envReference = regexp.MustCompile(`\{\{\$([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// Expand replaces {{$VAR}} references with environment values, as config.toml does.
func Expand(s string) string { return expand(s) }

// expand replaces {{$VAR}} with the environment value.
func expand(s string) string {
	return envReference.ReplaceAllStringFunc(s, func(m string) string { return os.Getenv(m[3 : len(m)-2]) })
}

func expandPath(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// FlagConfig is the file named by --config; relative paths resolve against the project directory.
func FlagConfig(dir string) string {
	path := os.Getenv("DASHY_CONFIG")
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path
}

// ConfigPaths are the config files in merge order: $DASHY_HOME/config.toml, every
// dashy.toml from the filesystem root down to dir, then the --config file.
func ConfigPaths(home, dir string) []string {
	var local []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		local = append(local, filepath.Join(d, "dashy.toml"))
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(local)
	paths := append([]string{filepath.Join(home, "config.toml")}, local...)
	if flagged := FlagConfig(dir); flagged != "" {
		paths = append(paths, flagged)
	}
	return paths
}

// LoadConfig merges the ConfigPaths layers over the defaults. Missing files are
// skipped, except the --config one; malformed files are errors.
func LoadConfig(home, dir string) (Settings, error) {
	s := DefaultSettings()
	s.DBPath = filepath.Join(home, "dashy.db")
	flagged := FlagConfig(dir)
	for _, path := range ConfigPaths(home, dir) {
		err := mergeConfig(&s, path)
		if errors.Is(err, os.ErrNotExist) && path != flagged {
			continue
		}
		if err != nil {
			return Settings{}, err
		}
	}
	if s.Effort == "" {
		s.Effort = "medium"
	}
	return s, validateSettings(s)
}

func mergeConfig(s *Settings, path string) error {
	var f fileConfig
	md, err := toml.DecodeFile(path, &f)
	if err == nil {
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			err = fmt.Errorf("unknown key %s", undecoded[0])
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for target, value := range map[*string]*string{&s.DBPath: f.Agent.DBPath, &s.Provider: f.Agent.Provider, &s.Model: f.Agent.Model, &s.Effort: f.Agent.Effort} {
		if value != nil {
			*target = expand(*value)
		}
	}
	s.DBPath = expandPath(s.DBPath)
	s.Sources = maps.Clone(s.Sources)
	for key, value := range map[string]*string{"provider": f.Agent.Provider, "model": f.Agent.Model, "effort": f.Agent.Effort} {
		if value != nil {
			s.Sources[key] = path
		}
	}
	for target, value := range map[*int]*int{&s.ShellOutput: f.Agent.ShellOutput, &s.ShellTimeout: f.Agent.ShellTimeout} {
		if value != nil {
			*target = *value
		}
	}
	if f.Agent.ReasoningSummary != nil {
		s.ReasoningSummary = *f.Agent.ReasoningSummary
	}
	if f.UI.Editor != nil {
		s.UI.Editor = *f.UI.Editor
	}
	s.UI.Events = merged(s.UI.Events, f.UI.Events)
	s.UI.Theme = merged(s.UI.Theme, f.UI.Theme)
	s.Providers = maps.Clone(s.Providers)
	// A provider merges by field, so a later layer can set only its api_key.
	for name, p := range f.Provider {
		merged := s.Providers[name]
		for target, value := range map[*string]*string{&merged.BaseURL: p.BaseURL, &merged.APIKey: p.APIKey, &merged.API: p.API} {
			if value != nil {
				*target = expand(*value)
			}
		}
		s.Providers[name] = merged
	}
	// Blocks and hooks keep file order; a later entry with the same name replaces the earlier one.
	for _, key := range md.Keys() {
		if len(key) != 2 {
			continue
		}
		name := key[1]
		switch key[0] {
		case "system":
			b := f.System[name]
			if b.Type != "text" && b.Type != "bash" {
				return fmt.Errorf("%s: system.%s: type must be text or bash", path, name)
			}
			s.System = upsert(s.System, SystemBlock{name, b.Type, expand(b.Value)}, func(x SystemBlock) string { return x.Name })
		case "hooks":
			h := f.Hooks[name]
			hook := Hook{Name: name, Events: h.Events, Run: expand(h.Run), Timeout: h.Timeout, Enabled: h.Enabled == nil || *h.Enabled, PasteOutput: h.PasteOutput}
			if hook.Timeout == 0 {
				hook.Timeout = HookTimeout
			}
			if hook.Timeout < 0 || hook.Timeout > ShellMaxTimeout {
				return fmt.Errorf("%s: hooks.%s: timeout must be 1..%d seconds", path, name, ShellMaxTimeout)
			}
			s.Hooks = upsert(s.Hooks, hook, func(x Hook) string { return x.Name })
		}
	}
	return nil
}

func merged[V any](base, over map[string]V) map[string]V {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]V{}
	}
	maps.Copy(out, over)
	return out
}

func upsert[T any](list []T, item T, name func(T) string) []T {
	list = slices.Clone(list)
	if i := slices.IndexFunc(list, func(x T) bool { return name(x) == name(item) }); i >= 0 {
		list[i] = item
		return list
	}
	return append(list, item)
}

func DashyHome(cwd string) (string, error) {
	dir := os.Getenv("DASHY_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".dashy")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

// DBPath is the configured SQLite path for helper commands that run without an agent.
func DBPath(home string) string {
	cwd, _ := os.Getwd()
	if s, err := LoadConfig(home, cwd); err == nil {
		return s.DBPath
	}
	return filepath.Join(home, "dashy.db")
}

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

// WriteConfig writes s as $home/config.toml, private to the user.
func WriteConfig(home string, s Settings) error {
	var agentTable struct {
		DBPath           string `toml:"db_path,omitempty"`
		Provider         string `toml:"provider,omitempty"`
		Model            string `toml:"model,omitempty"`
		Effort           string `toml:"effort,omitempty"`
		ReasoningSummary bool   `toml:"reasoning_summary"`
		ShellOutput      int    `toml:"shell_output"`
		ShellTimeout     int    `toml:"shell_timeout"`
	}
	agentTable.DBPath, agentTable.Provider, agentTable.Model, agentTable.Effort = s.DBPath, s.Provider, s.Model, s.Effort
	agentTable.ReasoningSummary, agentTable.ShellOutput, agentTable.ShellTimeout = s.ReasoningSummary, s.ShellOutput, s.ShellTimeout
	var ui struct {
		Editor string            `toml:"editor,omitempty"`
		Events map[string]bool   `toml:"events"`
		Theme  map[string]string `toml:"theme"`
	}
	ui.Editor = s.UI.Editor
	ui.Events, ui.Theme = s.UI.Events, s.UI.Theme
	var out strings.Builder
	section := func(header string, table any) error {
		if header != "" {
			out.WriteString(header + "\n")
		}
		err := toml.NewEncoder(&out).Encode(table)
		out.WriteString("\n")
		return err
	}
	tables := []any{map[string]any{"agent": agentTable}, map[string]any{"ui": ui}}
	if len(s.Providers) > 0 {
		tables = append(tables, map[string]any{"provider": s.Providers})
	}
	for _, table := range tables {
		if err := section("", table); err != nil {
			return err
		}
	}
	// Explicit headers keep system blocks and hooks in order.
	for _, b := range s.System {
		if err := section("[system."+tomlKey(b.Name)+"]", map[string]string{"type": b.Type, "value": b.Value}); err != nil {
			return err
		}
	}
	for _, h := range s.Hooks {
		hook := map[string]any{"events": h.Events, "run": h.Run, "timeout": h.Timeout, "enabled": h.Enabled, "paste_output": h.PasteOutput}
		if err := section("[hooks."+tomlKey(h.Name)+"]", hook); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "config.toml"), []byte(out.String()), 0600)
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(name string) string {
	if bareKey.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}
