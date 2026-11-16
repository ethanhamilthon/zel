package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

const (
	// CompactionThreshold is the default context size, in tokens, that triggers
	// automatic compaction; zero disables it.
	CompactionThreshold = 400000
	// BashTimeout is the default limit of one bash call of the agent, in seconds.
	BashTimeout = 30
	// BashOutput is the default byte limit of one bash call's output; 0 = no cut.
	BashOutput = 8000
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

// UI holds application display preferences.
type UI struct {
	SlashMenu bool
}

// Settings is the effective configuration merged from the zel.toml layers
// (see Load); instructions, hooks and completions live in Config.
type Settings struct {
	Provider, Model, Effort string
	ReasoningSummary        bool
	CompactionThreshold     int
	BashTimeout             int // seconds per bash call of the agent
	BashOutput              int // bytes of one bash call's output the agent gets, 0 = no cut
	UI                      UI
	Colors                  Colors // [colors]: the TUI theme, defaults filled in
	Providers               map[string]Provider
	Env                     map[string]string // [env]: extra variables for bash, tasks, hooks and ${{ }}
}

func DefaultSettings() Settings {
	return Settings{Effort: "medium", ReasoningSummary: true, CompactionThreshold: CompactionThreshold, BashTimeout: BashTimeout, BashOutput: BashOutput,
		UI: UI{SlashMenu: true}, Colors: DefaultColors(), Providers: map[string]Provider{}, Env: map[string]string{}}
}

func Redact(s Settings, text string, extra ...string) string {
	secrets := extra
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
		return "No provider configured · open Providers in the menu"
	}
	return "No model selected · choose one in the Ctrl+K menu"
}

func Validate(s Settings) error {
	if s.CompactionThreshold < 0 {
		return fmt.Errorf("compaction threshold must be >= 0 tokens (0 = off)")
	}
	if s.BashTimeout <= 0 {
		return fmt.Errorf("bash_timeout must be > 0 seconds")
	}
	if s.BashOutput < 0 || (s.BashOutput > 0 && s.BashOutput < 1000) {
		return fmt.Errorf("bash_output must be 0 (no cut) or >= 1000 bytes")
	}
	for i, key := range ColorKeys {
		if err := CheckColor(key, s.Colors[i]); err != nil {
			return err
		}
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
