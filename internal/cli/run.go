package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"dashy/internal/agent"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

//go:embed help.txt
var helpText string

type ExitError struct {
	Code     int
	Err      error
	Reported bool
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

func ExitCode(err error) int {
	var exit *ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	}
	return 1
}

func IsReported(err error) bool {
	var exit *ExitError
	return errors.As(err, &exit) && exit.Reported
}

var cancelled = &ExitError{Code: 130, Reported: true}

func isTerminal(v any) bool {
	if w, ok := v.(*failedWriter); ok {
		v = w.w
	}
	f, ok := v.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

func IsHelper(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "help", "--help", "-h", "tasks", "view", "events":
		return true
	}
	return false
}

type failedWriter struct {
	w   io.Writer
	err error
}

func (f *failedWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

var Version = "dev"

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	w := &failedWriter{w: out}
	err := run(ctx, args, in, w, diagnostics)
	if err == nil && w.err != nil {
		err = fmt.Errorf("write output: %w", w.err)
	}
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "help", "--help", "-h":
			if len(args) > 1 {
				return fmt.Errorf("%s takes no arguments", args[0])
			}
			if args[0] == "version" {
				fmt.Fprintln(out, strings.TrimPrefix(Version, "v"))
			} else {
				fmt.Fprint(out, helpText)
			}
			return nil
		case "tasks":
			return tasksCommand(args[1:], out)
		case "view":
			return viewCommand(args[1:])
		case "events":
			return eventsCommand(args[1:], out)
		case "models":
			return modelsCommand(ctx, args[1:], out, diagnostics)
		case "json":
			return jsonCommand(ctx, args[1:], in, out, diagnostics)
		case "setup":
			return setupCommand(ctx, args[1:], in, out)
		}
	}
	return sessionCommand(ctx, args, in, out)
}

func flagSet(name string, define func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	return fs
}

// parseWithConfig parses fs with a --config flag and exports the file as DASHY_CONFIG,
// so the agent, its bash tool and helper commands all read the same layers.
func parseWithConfig(fs *flag.FlagSet, args []string) error {
	config := fs.String("config", "", "config file merged on top of the others")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *config == "" {
		return nil
	}
	path, err := filepath.Abs(*config)
	if err != nil {
		return err
	}
	return os.Setenv("DASHY_CONFIG", path)
}

func viewCommand(args []string) error {
	var session string
	fs := flagSet("view", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("view: %w", err)
	}
	if fs.NArg() == 0 {
		return errors.New("usage: dashy view [-s ID] PATH [PATH ...]")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	endpoint, err := agent.Endpoint(cwd, session)
	if err != nil {
		return err
	}
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	for _, path := range fs.Args() {
		image, err := shell.CaptureImageCopy(path, filepath.Join(home, "attachments"))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err = agent.Send(endpoint, agent.Message{Image: &image}); err != nil {
			_ = os.Remove(image.Path)
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func modelsCommand(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	var asJSON bool
	fs := flagSet("models", func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "print JSON") })
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: dashy models [--json]")
	}
	cwd, _ := os.Getwd() // only resolves a relative DASHY_HOME, which then reports its own error
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	settings, err := agent.LoadConfig(home, cwd)
	if err != nil {
		return err
	}
	providers := settings.Providers
	if len(providers) == 0 {
		return errors.New("no providers configured: run dashy setup or edit config.toml")
	}
	choices, errs := agent.AllModels(ctx, providers)
	if ctx.Err() != nil {
		return cancelled
	}
	if asJSON {
		failures := map[string]string{}
		for name, err := range errs {
			failures[name] = err.Error()
		}
		data, _ := json.Marshal(map[string]any{"models": modelList(choices), "failures": failures})
		fmt.Fprintf(out, "%s\n", data)
	} else {
		for _, c := range choices {
			fmt.Fprintf(out, "%s\t%s\n", c.Provider, c.Model)
		}
		for _, name := range slices.Sorted(maps.Keys(errs)) {
			fmt.Fprintf(diagnostics, "%s: %v\n", name, errs[name])
		}
	}
	if len(errs) > 0 {
		return &ExitError{Code: 1, Reported: true}
	}
	return nil
}

func sessionCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flagSet("dashy", func(*flag.FlagSet) {})
	if err := parseWithConfig(fs, args); err != nil {
		return fmt.Errorf("%w (see dashy --help)", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !isTerminal(in) || !isTerminal(out) {
		return errors.New("the interactive TUI needs a terminal")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	a, err := agent.New(cwd, agent.Options{Mode: "tui"})
	if err != nil {
		return err
	}
	defer a.Close()
	running, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if err := watchProviders(running, a.Settings(), stop); err != nil {
		return err
	}
	tui.Version = Version
	err = tui.Run(running, a, cwd)
	if cause := context.Cause(running); ctx.Err() == nil && cause != nil {
		return cause
	}
	return cancelledOr(ctx, err)
}

// watchProviders fails at once when no provider is configured. Otherwise it lists
// the models of every provider in the background and calls fail with a guide when
// none of them answers, so a broken setup ends the session instead of every turn failing.
func watchProviders(ctx context.Context, s agent.Settings, fail func(error)) error {
	home, err := agent.DashyHome(".")
	if err != nil {
		return err
	}
	if len(s.Providers) == 0 {
		return providerGuide(home, nil)
	}
	go func() {
		listing, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, errs := agent.AllModels(listing, s.Providers)
		if ctx.Err() == nil && len(errs) == len(s.Providers) {
			fail(providerGuide(home, errs))
		}
	}()
	return nil
}

func providerGuide(home string, failures map[string]error) error {
	var b strings.Builder
	if len(failures) == 0 {
		b.WriteString("no provider is configured\n")
	} else {
		b.WriteString("no provider answers; listing models failed:\n")
	}
	for _, name := range slices.Sorted(maps.Keys(failures)) {
		fmt.Fprintf(&b, "  %s: %v\n", name, failures[name])
	}
	fmt.Fprintf(&b, "\nWhat next:\n  - run dashy setup, or edit %s\n", filepath.Join(home, "config.toml"))
	b.WriteString("  - check base_url, the API key (or the {{$VAR}} it references) and that the server is running\n")
	b.WriteString("  - run dashy models to test the connection")
	return errors.New(b.String())
}

func cancelledOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return cancelled
	}
	return err
}
