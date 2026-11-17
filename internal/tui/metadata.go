package tui

import (
	"cmp"
	"fmt"
	"strings"
	"time"

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
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
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
			b.WriteString("\n" + firstLine(cmp.Or(c.Command, c.ID)) + "\n")
			fmt.Fprintf(&b, "%-8s%s\n", "ID", c.ID)
			fmt.Fprintf(&b, "%-8s%s\n", "Model", strings.TrimPrefix(c.Provider+" · ", " · ")+c.Model+" · "+c.Effort)
			fmt.Fprintf(&b, "%-8s%s · running %s\n", "Context", formatContext(c.Context), formatDuration(time.Since(c.Started)))
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
