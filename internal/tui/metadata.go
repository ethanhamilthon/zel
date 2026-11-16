package tui

import (
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

// infoRows are the rows of the Info page. The first row is the page title (the session
// name); the rest are key/value pairs: id, dir, model, context and spend.
func infoRows(info core.Info) []menuRow {
	cache := "cache hit unavailable"
	if info.Usage.Cached >= 0 {
		percent := float64(0)
		if info.Usage.Input > 0 {
			percent = float64(info.Usage.Cached) * 100 / float64(info.Usage.Input)
		}
		cache = fmt.Sprintf("%.0f%% cache hit", percent)
	}
	title := info.Title
	if title == "" {
		title = "Session"
	}
	return []menuRow{
		{label: title},
		{"ID:", info.Session},
		{"Dir:", core.DisplayPath(info.Project)},
		{"Config:", configLabel(info.Configs)},
		{"Model:", info.Provider + " · " + info.Model + " · " + info.Effort},
		{"Context:", formatContext(info.Context)},
		{"Spend:", fmt.Sprintf("%s in · %s out · %s", formatTokens(info.Usage.Input), formatTokens(info.Usage.Output), cache)},
	}
}

// configLabel lists the zel.toml layers of the session, in merge order.
func configLabel(files []string) string {
	if len(files) == 0 {
		return "none"
	}
	short := make([]string, len(files))
	for i, f := range files {
		short[i] = core.DisplayPath(f)
	}
	return strings.Join(short, " · ")
}
