package tui

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"zel/internal/config"
	"zel/internal/core"
)

const markdownTemplate = `{"document":{"margin":0},"heading":{"bold":true,"block_suffix":"\n"},"h1":{"prefix":"# ","color":"%s"},"h2":{"prefix":"## ","color":"%s"},"h3":{"prefix":"### ","color":"%s"},"h4":{"color":"%s"},"h5":{"color":"%s"},"h6":{"color":"%s"},"strong":{"bold":true},"emph":{"italic":true},"code":{"color":"%s"},"code_block":{"margin":0},"list":{"level_indent":2},"item":{"block_prefix":"- "},"enumeration":{"block_prefix":". "},"block_quote":{"indent":1,"indent_token":"│ "},"link":{"underline":true,"color":"%s","format":"\u2063{{.text}}\u2064"},"link_text":{"bold":true},"table":{"center_separator":"┼","column_separator":"│","row_separator":"─"}}`

func formatDuration(d time.Duration) string { return d.Round(time.Second).String() }
func formatTokens(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fm", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
func formatSteps(n int) string {
	if n == 1 {
		return "1 call"
	}
	return fmt.Sprintf("%d calls", n)
}
func formatContext(n int64) string {
	return fmt.Sprintf("%dK", (n+500)/1000)
}

// infoText is the Info page: the session name, then aligned key/value rows (id,
// dir, config, model, context and spend).
func infoText(info core.Info) string {
	title := info.Title
	if title == "" {
		title = "Session"
	}
	var b strings.Builder
	b.WriteString(title + "\n\n")
	for _, r := range [][2]string{
		{"ID", info.Session},
		{"Dir", core.DisplayPath(info.Project)},
		{"Config", core.DisplayPath(info.Config)},
		{"Model", info.Provider + " · " + info.Model + " · " + info.Effort},
		{"Context", formatContext(info.Context)},
		{"Spend", spend(info.Usage)},
	} {
		fmt.Fprintf(&b, "%-8s%s\n", r[0], r[1])
	}
	if len(info.Subagents) > 0 {
		fmt.Fprintf(&b, "\nSubagents (%d)\n", len(info.Subagents))
		for _, c := range info.Subagents {
			state := "running " + formatDuration(time.Since(c.Started))
			if !c.Ended.IsZero() {
				state = "done " + formatDuration(max(0, c.Ended.Sub(c.Started)))
			}
			b.WriteString("\n" + firstLine(cmp.Or(c.Command, c.ID)) + "\n")
			fmt.Fprintf(&b, "%-8s%s\n", "ID", c.ID)
			fmt.Fprintf(&b, "%-8s%s\n", "Model", strings.TrimPrefix(c.Provider+" · ", " · ")+c.Model+" · "+c.Effort)
			fmt.Fprintf(&b, "%-8s%s · %s\n", "Context", formatContext(c.Context), state)
			fmt.Fprintf(&b, "%-8s%s\n", "Spend", spend(c.Usage))
		}
	}
	return b.String()
}

// spend is the token use: input, output and the share of the input that hit the cache.
func spend(u core.Usage) string {
	cache := "cache hit unavailable"
	if u.Cached >= 0 {
		percent := float64(0)
		if u.Input > 0 {
			percent = float64(u.Cached) * 100 / float64(u.Input)
		}
		cache = fmt.Sprintf("%s cached, %.0f%% hit", formatTokens(u.Cached), percent)
	}
	return fmt.Sprintf("%s in · %s out · %s", formatTokens(u.Input), formatTokens(u.Output), cache)
}

func configSource(s string) string {
	if s == "" {
		return ""
	}
	return core.DisplayPath(s)
}

// configText formats the Config menu page: reload status, warnings/errors, and
// every loaded section with its source file.
func configText(cfg *config.Config, reloadStatus string) string {
	var b strings.Builder
	if reloadStatus != "" {
		b.WriteString(reloadStatus + "\n\n")
	} else {
		b.WriteString("Ctrl+R to reload\n\n")
	}
	if cfg == nil {
		b.WriteString("No configuration loaded.\n")
		return b.String()
	}
	if len(cfg.Errors) > 0 {
		fmt.Fprintf(&b, "Errors (%d)\n", len(cfg.Errors))
		for _, e := range cfg.Errors {
			fmt.Fprintf(&b, "  %s\n", e)
		}
		b.WriteByte('\n')
	}
	if len(cfg.Warnings) > 0 {
		fmt.Fprintf(&b, "Warnings (%d)\n", len(cfg.Warnings))
		for _, w := range cfg.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Files (%d)\n", len(cfg.Files))
	if len(cfg.Files) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, f := range cfg.Files {
		fmt.Fprintf(&b, "  %s\n", configSource(f))
	}
	fmt.Fprintf(&b, "\nSystems (%d)\n", len(cfg.Systems))
	if len(cfg.Systems) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, s := range cfg.Systems {
		if src := configSource(s.Source); src != "" {
			fmt.Fprintf(&b, "  %s · %s\n", s.Name, src)
		} else {
			fmt.Fprintf(&b, "  %s\n", s.Name)
		}
	}
	fmt.Fprintf(&b, "\nFunctions (%d)\n", len(cfg.Functions))
	if len(cfg.Functions) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, f := range cfg.Functions {
		line := firstLine(f.Description)
		if src := configSource(f.Source); src != "" {
			fmt.Fprintf(&b, "  %s · %s\n    %s\n", f.Name, src, line)
		} else {
			fmt.Fprintf(&b, "  %s\n    %s\n", f.Name, line)
		}
	}
	fmt.Fprintf(&b, "\nPrompts (%d)\n", len(cfg.Prompts))
	if len(cfg.Prompts) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Prompts)) {
		if src := configSource(cfg.PromptSources[name]); src != "" {
			fmt.Fprintf(&b, "  %s · %s\n", name, src)
		} else {
			fmt.Fprintf(&b, "  %s\n", name)
		}
	}
	fmt.Fprintf(&b, "\nHooks (%d)\n", len(cfg.Hooks))
	if len(cfg.Hooks) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, h := range cfg.Hooks {
		label := strings.Join(h.Events, ", ")
		if h.Async {
			label += " (async)"
		}
		if src := configSource(h.Source); src != "" {
			fmt.Fprintf(&b, "  %s · %s\n", label, src)
		} else {
			fmt.Fprintf(&b, "  %s\n", label)
		}
	}
	fmt.Fprintf(&b, "\nCommands (%d)\n", len(cfg.Commands))
	if len(cfg.Commands) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, cmd := range cfg.Commands {
		var parts []string
		parts = append(parts, cmd.Name)
		if cmd.Key != "" {
			parts = append(parts, cmd.Key)
		}
		if src := configSource(cmd.Source); src != "" {
			parts = append(parts, src)
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(parts, " · "))
	}
	return b.String()
}
