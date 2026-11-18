package config

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
)

const (
	// CompactionThreshold is the default context size, in tokens, that triggers
	// automatic compaction; zero disables it.
	CompactionThreshold = 400000
	// Timeout is the default limit of one lua call of the agent, in seconds.
	Timeout = 30
	// Output is the default byte limit of one lua call's output; 0 = no cut.
	Output = 8000
	// Editor is the default editor of Ctrl+E.
	Editor = "nvim"
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

// Settings holds the configuration settings from init.lua/zel.lua and connect.json.
type Settings struct {
	Provider, Model, Effort string
	CompactionThreshold     int
	Timeout, Output         int // s per lua call; bytes, 0 = no cut
	ShowCode, ShowPrompt    bool
	TraceHooks              bool
	Editor                  string
	Colors                  Colors
	Keys                    map[string]string
	Env                     map[string]string
	Providers               map[string]Provider // from connect.json
}

func DefaultSettings() Settings {
	return Settings{
		Effort:              "medium",
		CompactionThreshold: CompactionThreshold,
		Timeout:             Timeout,
		Output:              Output,
		ShowCode:            true,
		ShowPrompt:          false,
		TraceHooks:          true,
		Editor:              Editor,
		Colors:              DefaultColors(),
		Keys:                maps.Clone(DefaultKeys),
		Providers:           map[string]Provider{},
		Env:                 map[string]string{},
	}
}

// ParseSettings parses a Lua `settings` table (as map[string]any) on top of base.
// Unknown keys and invalid types or values return an error.
func ParseSettings(raw map[string]any, base Settings) (Settings, error) {
	st := base
	if st.Keys != nil {
		st.Keys = maps.Clone(st.Keys)
	} else {
		st.Keys = maps.Clone(DefaultKeys)
	}
	if st.Env != nil {
		st.Env = maps.Clone(st.Env)
	} else {
		st.Env = map[string]string{}
	}
	if st.Providers != nil {
		st.Providers = maps.Clone(st.Providers)
	} else {
		st.Providers = map[string]Provider{}
	}

	for _, k := range slices.Sorted(maps.Keys(raw)) {
		v, where := raw[k], "settings."+k
		switch k {
		case "compaction":
			n, err := asInt(v, where)
			if err != nil || n < 0 {
				return base, fmt.Errorf("%s: must be an integer >= 0", where)
			}
			st.CompactionThreshold = n
		case "timeout":
			n, err := asInt(v, where)
			if err != nil || n <= 0 {
				return base, fmt.Errorf("%s: must be an integer > 0 (seconds)", where)
			}
			st.Timeout = n
		case "output":
			n, err := asInt(v, where)
			if err != nil || n < 0 || (n > 0 && n < 1000) {
				return base, fmt.Errorf("%s: must be 0 (no cut) or an integer >= 1000 (bytes)", where)
			}
			st.Output = n
		case "show_code":
			b, ok := v.(bool)
			if !ok {
				return base, fmt.Errorf("%s: must be true or false", where)
			}
			st.ShowCode = b
		case "show_prompt":
			b, ok := v.(bool)
			if !ok {
				return base, fmt.Errorf("%s: must be true or false", where)
			}
			st.ShowPrompt = b
		case "trace_hooks":
			b, ok := v.(bool)
			if !ok {
				return base, fmt.Errorf("%s: must be true or false", where)
			}
			st.TraceHooks = b
		case "editor":
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return base, fmt.Errorf("%s: must be a command", where)
			}
			st.Editor = s
		case "colors":
			c, err := parseColors(v, st.Colors)
			if err != nil {
				return base, err
			}
			st.Colors = c
		case "keys":
			keys, err := parseKeys(v, st.Keys)
			if err != nil {
				return base, err
			}
			st.Keys = keys
		case "env":
			env, err := parseEnv(v, st.Env)
			if err != nil {
				return base, err
			}
			st.Env = env
		default:
			return base, fmt.Errorf("unknown key %q", where)
		}
	}
	return st, nil
}

func asInt(v any, where string) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || float64(int(n)) != n {
			return 0, fmt.Errorf("%s: must be an integer", where)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s: must be an integer", where)
	}
}

func parseEnv(v any, base map[string]string) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("env: must be a table")
	}
	env := maps.Clone(base)
	if env == nil {
		env = map[string]string{}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("env: invalid variable name %q", k)
		}
		s, ok := m[k].(string)
		if !ok {
			return nil, fmt.Errorf("env.%s: must be a string", k)
		}
		env[k] = s
	}
	return env, nil
}

// Redact hides the API keys of the providers in text.
func Redact(s Settings, text string) string {
	var secrets []string
	for _, p := range s.Providers {
		secrets = append(secrets, p.APIKey)
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
		return "No provider configured: add one to connect.json (zel docs connect)"
	}
	return "No model selected: choose one with Ctrl+M or set model in connect.json"
}

// Validate checks effort, providers, and that the selected model has a provider.
func Validate(s Settings) error {
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
