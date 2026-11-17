package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Describe renders the configuration for `zel config`: the file, problems,
// every setting, and the names of env variables, instructions, hooks and
// completions. Secrets are never shown.
func (c *Config) Describe() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("Config: %s", DisplayPath(c.Path))
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
		{"provider", strconv.Quote(s.Provider)},
		{"model", strconv.Quote(s.Model)},
		{"effort", strconv.Quote(s.Effort)},
		{"settings.compaction", strconv.Itoa(s.CompactionThreshold)},
		{"settings.reasoning_summary", strconv.FormatBool(s.ReasoningSummary)},
		{"settings.slash_menu", strconv.FormatBool(s.UI.SlashMenu)},
		{"settings.show_bash", strconv.FormatBool(s.UI.ShowBash)},
		{"settings.bash_timeout", strconv.Itoa(s.BashTimeout)},
		{"settings.bash_output", strconv.Itoa(s.BashOutput)},
	} {
		p("  %-26s = %s", row[0], row[1])
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

	if len(c.Instructions) > 0 {
		p("Instructions:")
		for _, in := range c.Instructions {
			first, _, _ := strings.Cut(strings.TrimSpace(in.Text), "\n")
			p("  - %s%s", clip(first, 70), modes(in.Main, in.Headless))
		}
	}
	if len(c.Hooks) > 0 {
		p("Hooks:")
		for _, ev := range Events {
			for _, h := range c.Hooks[ev] {
				flags := modes(h.Main, h.Headless)
				if h.Match != "" {
					flags += " match=" + strconv.Quote(h.Match)
				}
				if h.Timeout > 0 {
					flags += " timeout=" + h.Timeout.String()
				}
				if h.Async {
					flags += " async"
				}
				first, _, _ := strings.Cut(strings.TrimSpace(h.Run), "\n")
				p("  %s: %s%s", ev, clip(first, 60), flags)
			}
		}
	}
	if len(c.Completions) > 0 {
		names := slices.Sorted(maps.Keys(c.Completions))
		for i, n := range names {
			names[i] = "#" + n
		}
		p("Completions: %s", strings.Join(names, " "))
	}
	return b.String()
}

// modes marks entries that do not run in the default (main only) set of modes.
func modes(main, headless bool) string {
	switch {
	case main && !headless:
		return ""
	case main:
		return " [main, headless]"
	case headless:
		return " [headless]"
	}
	return " [off]"
}
