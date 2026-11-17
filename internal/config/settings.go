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
	// Modes lists the session modes that may use it; empty means every mode.
	Modes []string `json:"modes,omitempty"`
}

// Settings is what the config file sets besides instructions, hooks and
// completions, which live in Config.
type Settings struct {
	Provider, Model, Effort string
	CompactionThreshold     int
	BashTimeout             int    // seconds per bash call of the agent
	BashOutput              int    // bytes of one bash call's output the agent gets, 0 = no cut
	ShowBash                bool   // show the running command in the working line
	TraceHooks              bool   // Tracing opens with the hook runs shown
	Editor                  string // the command Ctrl+E opens the composer text in
	Colors                  Colors // [colors]: the TUI theme, defaults filled in
	Providers               map[string]Provider
	Env                     map[string]string // [env]: extra variables for bash, tasks, hooks and ${{ }}
}

func DefaultSettings() Settings {
	return Settings{Effort: "medium", CompactionThreshold: CompactionThreshold, BashTimeout: BashTimeout, BashOutput: BashOutput,
		ShowBash: true, TraceHooks: true, Editor: Editor, Colors: DefaultColors(), Providers: map[string]Provider{}, Env: map[string]string{}}
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
		return "No provider configured: add one to the config file (zel docs config)"
	}
	return "No model selected: choose one with Ctrl+M or set model in the config file"
}

// Validate checks what the parser cannot: the effort, the providers after their
// ${{ }} were expanded, and that the selected model has a provider. The numbers
// and colors are checked when the file is read.
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
