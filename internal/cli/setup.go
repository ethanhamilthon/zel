package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"dashy/internal/agent"
)

// setupCommand asks for one provider and a default model, then writes a clean config.toml.
func setupCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) > 0 {
		return errors.New("usage: dashy setup")
	}
	cwd, _ := os.Getwd()
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "config.toml")
	ui := setupStyle{color: isTerminal(out) && os.Getenv("NO_COLOR") == ""}
	fmt.Fprintf(out, "\n  %s\n  %s\n\n", ui.accent("dashy setup"), ui.dim("Writes "+path+"; Enter keeps the [default]."))
	lines := bufio.NewScanner(in)
	ask := func(question, fallback string) (string, error) {
		prompt := "  " + ui.accent("›") + " " + question
		if fallback != "" {
			prompt += " " + ui.dim("["+fallback+"]")
		}
		fmt.Fprint(out, prompt+" ")
		if !lines.Scan() {
			return "", cmpErr(lines.Err(), errors.New("setup cancelled"))
		}
		if answer := strings.TrimSpace(lines.Text()); answer != "" {
			return answer, nil
		}
		return fallback, nil
	}
	if _, err := os.Stat(path); err == nil {
		answer, err := ask("Overwrite the existing config?", "y/N")
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintf(out, "  %s\n", ui.dim("Kept the existing config."))
			return nil
		}
	}
	name, err := ask("Provider name", "openai")
	if err != nil {
		return err
	}
	if strings.ContainsAny(name, " .\"[]") {
		return fmt.Errorf("provider name %q must not contain spaces, dots, quotes or brackets", name)
	}
	baseURL, err := ask("Base URL", "https://api.openai.com/v1")
	if err != nil {
		return err
	}
	if u, err := url.Parse(baseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("base URL must be http(s): %s", baseURL)
	}
	apiTypes := []string{agent.APIResponses, agent.APIChat}
	api := agent.APIResponses
	if i, ok, err := pick(in, out, ui, "API", []string{"responses  OpenAI Responses API", "chat       Chat Completions API"}, 0); err != nil {
		return err
	} else if ok {
		api = apiTypes[i]
	} else if api, err = ask("API type "+ui.dim("(responses or chat)"), api); err != nil {
		return err
	} else if !slices.Contains(apiTypes, api) {
		return fmt.Errorf("api must be responses or chat, got %q", api)
	}
	keyRef := "{{$" + strings.ToUpper(strings.NewReplacer("-", "_").Replace(name)) + "_API_KEY}}"
	apiKey, err := ask("API key "+ui.dim("(env reference, literal key, - for none)"), keyRef)
	if err != nil {
		return err
	}
	if apiKey == "-" {
		apiKey = ""
	}
	var models []string
	for {
		listing, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err = agent.ProviderModels(listing, agent.Provider{BaseURL: baseURL, APIKey: agent.Expand(apiKey)})
		cancel()
		if err == nil {
			break
		}
		fmt.Fprintf(out, "\n  %s %v\n", ui.warn("Could not list models:"), err)
		again, askErr := ask("Try again? "+ui.dim("(Y retries, anything else types the model by hand)"), "Y")
		if askErr != nil {
			return askErr
		}
		if !strings.EqualFold(again, "y") && !strings.EqualFold(again, "yes") {
			break
		}
	}
	model, fallback := "", ""
	if len(models) > 0 {
		const other = "other (type the name)"
		if i, ok, err := pick(in, out, ui, "Model", append(slices.Clone(models), other), 0); err != nil {
			return err
		} else if ok {
			if i < len(models) {
				model = models[i]
			}
		} else {
			fmt.Fprintln(out)
			for i, m := range models[:min(len(models), 40)] {
				fmt.Fprintf(out, "  %s  %s\n", ui.accent(fmt.Sprintf("%3d", i+1)), m)
			}
			if len(models) > 40 {
				fmt.Fprintf(out, "       %s\n", ui.dim(fmt.Sprintf("… %d more, type the name", len(models)-40)))
			}
			fmt.Fprintln(out)
			fallback = models[0]
		}
	}
	if model == "" {
		if model, err = ask("Model (name or number)", fallback); err != nil {
			return err
		}
		if n, err := strconv.Atoi(model); err == nil && n >= 1 && n <= len(models) {
			model = models[n-1]
		}
	}
	efforts := slices.Clone(agent.Efforts)
	effort := "medium"
	if i, ok, err := pick(in, out, ui, "Reasoning effort", efforts, slices.Index(efforts, effort)); err != nil {
		return err
	} else if ok {
		effort = efforts[i]
	} else if effort, err = ask("Reasoning effort "+ui.dim("(none minimal low medium high xhigh max)"), effort); err != nil {
		return err
	}
	if !slices.Contains(agent.Efforts, effort) {
		return fmt.Errorf("unsupported effort %q", effort)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(starterConfig(name, baseURL, api, apiKey, model, effort)), 0600); err != nil {
		return err
	}
	if _, err := agent.LoadConfig(home, cwd); err != nil {
		return fmt.Errorf("wrote %s, but it does not load: %w", path, err)
	}
	fmt.Fprintf(out, "\n  %s %s\n  %s\n\n", ui.accent("✓"), "Wrote "+path, ui.dim("Run dashy to start."))
	return nil
}

// pick shows items as a list moved with the arrow keys and chosen with Enter. It
// reports false when stdin or stdout is not a terminal, so the caller asks in plain text.
func pick(in io.Reader, out io.Writer, ui setupStyle, label string, items []string, sel int) (int, bool, error) {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) || !isTerminal(out) {
		return 0, false, nil
	}
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return 0, false, nil
	}
	defer term.Restore(int(f.Fd()), state)
	visible, drawn := min(len(items), 10), 0
	fmt.Fprint(out, "\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?25h")
	buf := make([]byte, 16)
	for {
		if drawn > 0 {
			fmt.Fprintf(out, "\x1b[%dA", drawn)
		}
		fmt.Fprintf(out, "\r\x1b[J  %s %s\r\n", ui.accent("›"), label)
		start := min(max(sel-visible/2, 0), len(items)-visible)
		for i := start; i < start+visible; i++ {
			if i == sel {
				fmt.Fprintf(out, "    %s\r\n", ui.accent("❯ "+items[i]))
			} else {
				fmt.Fprintf(out, "      %s\r\n", items[i])
			}
		}
		fmt.Fprintf(out, "    %s\r\n", ui.dim("↑/↓ move · Enter select"))
		drawn = visible + 2
		n, err := f.Read(buf)
		if err != nil {
			return 0, true, err
		}
		for keys := string(buf[:n]); keys != ""; {
			size := 1
			if strings.HasPrefix(keys, "\x1b[") || strings.HasPrefix(keys, "\x1bO") {
				size = min(3, len(keys))
			}
			key := keys[:size]
			keys = keys[size:]
			switch key {
			case "\x1b[A", "\x1bOA", "k":
				sel = (sel + len(items) - 1) % len(items)
			case "\x1b[B", "\x1bOB", "j":
				sel = (sel + 1) % len(items)
			case "\r", "\n", "\x03":
				fmt.Fprintf(out, "\x1b[%dA\r\x1b[J", drawn)
				if key == "\x03" {
					return 0, true, errors.New("setup cancelled")
				}
				fmt.Fprintf(out, "  %s %s  %s\r\n", ui.accent("›"), label, strings.Fields(items[sel])[0])
				return sel, true, nil
			}
		}
	}
}

// setupStyle colors setup output with the TUI's accent; plain when not a terminal or NO_COLOR is set.
type setupStyle struct{ color bool }

func (s setupStyle) paint(rgb, text string) string {
	if !s.color {
		return text
	}
	return "\x1b[38;2;" + rgb + "m" + text + "\x1b[0m"
}

func (s setupStyle) accent(text string) string { return s.paint("138;173;255", text) }
func (s setupStyle) dim(text string) string    { return s.paint("135;148;168", text) }
func (s setupStyle) warn(text string) string   { return s.paint("229;192;123", text) }

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func starterConfig(name, baseURL, api, apiKey, model, effort string) string {
	key := ""
	if apiKey != "" {
		key = "api_key = " + strconv.Quote(apiKey) + "\n"
	}
	return `# dashy configuration; every dashy.toml from / down to the project, then --config, is merged on top.
# "{{$VAR}}" in string values reads $VAR from the environment.

[agent]
# db_path = "~/.dashy/dashy.db" # SQLite database for sessions and tasks; default $DASHY_HOME/dashy.db
provider = ` + strconv.Quote(name) + `
model = ` + strconv.Quote(model) + `
effort = ` + strconv.Quote(effort) + `               # none | minimal | low | medium | high | xhigh | max
reasoning_summary = true
shell_output = 8192             # max bytes per bash call returned to the model
shell_timeout = 120             # default bash timeout in seconds (max 600)

[ui.events]
turn_stats = true

[ui.theme]
input_bg = "#202020"
select_bg = "#303030"
foreground = "#d5ddeb"
muted = "#8794a8"
dim = "#606060"
code = "#a0a0a0"
accent = "#8aadff"
heading = "#79d9ff"
assistant = "#9bdef5"
info = "#d7c38b"
warn = "#e5c07b"
error = "#f09eac"

[provider.` + name + `]
base_url = ` + strconv.Quote(baseURL) + `
api = ` + strconv.Quote(api) + `                  # responses | chat (Chat Completions)
` + key + `
# System prompt blocks, in this order: "text" appends value, "bash" appends stdout.
[system.main]
type = "text"
value = "You are Dashy, a coding agent. Keep answers short and precise."

[system.agents]
type = "bash"
value = "cat AGENTS.md 2>/dev/null || true"

# Hooks: [hooks.NAME] with events = ["turn_end"], run = "...", paste_output = true.
`
}
