package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Describe renders the configuration for `zel config`: layers, problems, every
// setting with the file that set it, and the names of env variables,
// instructions, hooks and completions. Secrets are never shown.
func (c *Config) Describe() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	src := func(key string) string {
		if s, ok := c.Sources[key]; ok {
			return DisplayPath(s)
		}
		return "default"
	}
	p("Layers:")
	if len(c.Layers) == 0 {
		p("  (none)")
	}
	for _, l := range c.Layers {
		p("  %-6s  %s", l.Name, DisplayPath(l.Path))
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
		{"provider", strconv.Quote(s.Provider)},
		{"model", strconv.Quote(s.Model)},
		{"effort", strconv.Quote(s.Effort)},
		{"settings.compaction", strconv.Itoa(s.CompactionThreshold)},
		{"settings.reasoning_summary", strconv.FormatBool(s.ReasoningSummary)},
		{"settings.accent", strconv.Quote(s.UI.Accent)},
		{"settings.slash_menu", strconv.FormatBool(s.UI.SlashMenu)},
		{"settings.bash_timeout", strconv.Itoa(s.BashTimeout)},
	} {
		p("  %-26s = %-14s  %s", row[0], row[1], src(row[0]))
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
			p("  %s: base_url=%s api=%s api_key=%s  %s", name, pr.BaseURL, api, key, src("providers."+name))
		}
	}
	if len(s.Env) > 0 {
		p("Env: %s", strings.Join(slices.Sorted(maps.Keys(s.Env)), ", "))
	}

	if len(c.Instructions) > 0 {
		p("Instructions:")
		for i, in := range c.Instructions {
			if i == 0 || in.Layer != c.Instructions[i-1].Layer {
				n := 0
				for _, other := range c.Instructions {
					if other.Layer == in.Layer {
						n++
					}
				}
				p("  %s (%d)", DisplayPath(in.Layer), n)
			}
			first, _, _ := strings.Cut(strings.TrimSpace(in.Text), "\n")
			p("    - %s%s", clip(first, 70), modes(in.Main, in.Headless))
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
