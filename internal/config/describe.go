package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// Describe renders the configuration for `zel config`: the files, problems,
// every setting, and the names of env variables, systems, prompts, functions
// and hooks. Secrets are never shown.
func Describe(c *Config) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("Config: %s", DisplayPath(c.Paths.Init))
	if c.Paths.Local != "" {
		p("Local:  %s", DisplayPath(c.Paths.Local))
	}
	if c.Paths.Connect != "" {
		p("Connect: %s", DisplayPath(c.Paths.Connect))
	}
	for _, list := range []struct {
		title string
		items []string
	}{{"Errors", c.Errors}, {"Warnings", c.Warnings}} {
		if len(list.items) > 0 {
			p("%s:", list.title)
			for _, e := range list.items {
				p("  %s", strings.ReplaceAll(e, "\n", "\n    "))
			}
		}
	}

	s := c.Settings
	p("Settings:")
	for _, row := range [][2]string{
		{"model", strconv.Quote(strings.TrimPrefix(s.Provider+"/"+s.Model, "/"))},
		{"effort", strconv.Quote(s.Effort)},
		{"settings.compaction", strconv.Itoa(s.CompactionThreshold)},
		{"settings.timeout", strconv.Itoa(s.Timeout)},
		{"settings.output", strconv.Itoa(s.Output)},
		{"settings.show_code", strconv.FormatBool(s.ShowCode)},
		{"settings.show_prompt", strconv.FormatBool(s.ShowPrompt)},
		{"settings.trace_hooks", strconv.FormatBool(s.TraceHooks)},
		{"settings.editor", strconv.Quote(s.Editor)},
	} {
		p("  %-26s = %s", row[0], row[1])
	}
	p("Keys:")
	keys := s.Keys
	if keys == nil {
		keys = DefaultKeys
	}
	for _, action := range slices.Sorted(maps.Keys(keys)) {
		p("  %-26s = %s", action, strconv.Quote(keys[action]))
	}
	p("Colors:")
	for i, key := range ColorKeys {
		p("  %-26s = %s", "colors."+key, strconv.Quote(s.Colors[i]))
	}
	if len(s.Providers) > 0 {
		p("Providers:")
		for _, name := range slices.Sorted(maps.Keys(s.Providers)) {
			pr, key := s.Providers[name], "empty"
			if pr.APIKey != "" {
				key = "set"
			}
			api := pr.API
			if api == "" {
				api = APIResponses
			}
			p("  %s: base_url=%s api=%s api_key=%s", name, pr.BaseURL, api, key)
		}
	}
	if len(s.Env) > 0 {
		p("Env: %s", strings.Join(slices.Sorted(maps.Keys(s.Env)), ", "))
	}

	if len(c.Systems) > 0 {
		p("Systems:")
		for _, sys := range c.Systems {
			first, _, _ := strings.Cut(strings.TrimSpace(sys.Text), "\n")
			p("  %s: %s", sys.Name, clip(first, 60))
		}
	}
	if len(c.Prompts) > 0 {
		names := slices.Sorted(maps.Keys(c.Prompts))
		for i, n := range names {
			names[i] = "#" + n
		}
		p("Prompts: %s", strings.Join(names, " "))
	}
	if len(c.Functions) > 0 {
		p("Functions:")
		for _, fn := range c.Functions {
			first, _, _ := strings.Cut(strings.TrimSpace(fn.Description), "\n")
			p("  %s: %s", fn.Name, clip(first, 60))
		}
	}
	if len(c.Hooks) > 0 {
		p("Hooks:")
		for _, h := range c.Hooks {
			flags := ""
			if h.Async {
				flags = " [async]"
			}
			p("  %s (%s)%s", h.Source, strings.Join(h.Events, ", "), flags)
		}
	}
	return b.String()
}

// Describe renders the configuration for `zel config`.
func (c *Config) Describe() string { return Describe(c) }
