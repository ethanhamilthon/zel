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

var HookEvents = []string{"app_start", "app_exit", "session_start", "session_close", "first_message", "user_message", "turn_start", "turn_end", "bash_start", "bash_end", "model_request_start", "model_request_end", "error"}
