package tui

import (
	"fmt"
	"strings"
	"time"

	"dashy/internal/agent"
	"dashy/internal/control"
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
func formatMetadata(meta *agent.Metadata, kind string) string {
	if kind != "assistant" && kind != "code" && kind != "output" && kind != "error" {
		return ""
	}
	duration := "?"
	if meta != nil {
		duration = formatDuration(meta.Duration)
	}
	if kind == "output" || kind == "error" {
		if meta == nil {
			return ""
		}
		return " · " + duration
	}
	input, output := "?", "?"
	if meta != nil && meta.Usage != nil {
		input, output = formatTokens(meta.Usage.Input), formatTokens(meta.Usage.Output)
	}
	if kind == "assistant" {
		return fmt.Sprintf("  ↑%s ↓%s", input, output)
	}
	return fmt.Sprintf("  ↑%s ↓%s · %s", input, output, duration)
}

func formatContext(n int64) string {
	return fmt.Sprintf("%dK", (n+500)/1000)
}

// formatBytes describes the stored output, before UI line limits and ANSI stripping.
func formatBytes(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1f MB", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1f KB", float64(n)/1000)
	}
	return fmt.Sprintf("%d B", n)
}

// infoRows are the rows of the Info page: session, model, config layers and token totals.
func infoRows(info control.Info) []menuRow {
	configs := info.Configs
	if len(configs) == 0 {
		configs = []string{"none (defaults)"}
	}
	cache := "cache hit unavailable"
	if info.Usage.Cached >= 0 {
		percent := float64(0)
		if info.Usage.Input > 0 {
			percent = float64(info.Usage.Cached) * 100 / float64(info.Usage.Input)
		}
		cache = fmt.Sprintf("%.0f%% cache hit", percent)
	}
	text := "Session:\n" + info.Session + "\n" + info.Title + "\n" + info.Project +
		"\n\nModel:\n" + info.Provider + "\n" + info.Model + " · " + info.Effort +
		"\n\nConfigs:\n" + strings.Join(configs, "\n") +
		"\n\nContext:\n" + fmt.Sprintf("%s in · %s out · %s context · %s", formatTokens(info.Usage.Input), formatTokens(info.Usage.Output), formatContext(info.Context), cache)
	var rows []menuRow
	for _, line := range strings.Split(text, "\n") {
		rows = append(rows, menuRow{label: line})
	}
	return rows
}
