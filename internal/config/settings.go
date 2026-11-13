package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

const (
	ShellTimeout    = 120
	ShellMaxTimeout = 600
	ShellOutput     = 8192
	// CompactionThreshold is the default context size, in tokens, that triggers
	// automatic compaction; zero disables it.
	CompactionThreshold = 400000
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
	Accent    string // accent color; empty selects the default
}

// Settings is the effective configuration: stored preferences plus the session's
// model selection. Instructions and plugins live in init.lua layers (LuaLayers).
type Settings struct {
	DBPath                    string `json:"-"`
	Provider, Model, Effort   string
	ReasoningSummary          bool
	ShellOutput, ShellTimeout int
	CompactionThreshold       int
	UI                        UI
	Providers                 map[string]Provider
}

func DefaultSettings() Settings {
	return Settings{Effort: "medium", ReasoningSummary: true, ShellOutput: ShellOutput, ShellTimeout: ShellTimeout, CompactionThreshold: CompactionThreshold,
		UI: UI{SlashMenu: true}, Providers: map[string]Provider{}}
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
	if s.ShellOutput < 0 {
		return fmt.Errorf("shell_output must be >= 0 bytes (0 = no truncation)")
	}
	if s.ShellTimeout < 1 || s.ShellTimeout > ShellMaxTimeout {
		return fmt.Errorf("shell_timeout must be 1..%d seconds", ShellMaxTimeout)
	}
	if s.CompactionThreshold < 0 {
		return fmt.Errorf("compaction threshold must be >= 0 tokens (0 = off)")
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
