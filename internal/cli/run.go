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

	"github.com/mattn/go-isatty"

	"zel/internal/core"
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
	case "version", "help", "--help", "-h", "tasks", "view", "events", "session", "init":
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
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

var Version = "dev"

type Options struct {
	Open        func(string, core.Options) (*core.Runtime, error)
	Interactive func(context.Context, *core.Runtime, string) error
}

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer, options Options) error {
	w := &failedWriter{w: out}
	err := run(ctx, args, in, w, diagnostics, options)
	if err == nil && w.err != nil {
		err = fmt.Errorf("write output: %w", w.err)
	}
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer, options Options) error {
	if len(args) > 0 {
		switch args[0] {
		case "init":
			if len(args) != 1 {
				return errors.New("usage: zel init")
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			created, err := core.InitProject(dir)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintln(out, "Created ZEL.md")
			} else {
				fmt.Fprintln(out, "ZEL.md already exists; left unchanged")
			}
			return nil
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
		case "session":
			return readSessionCommand(args[1:], out)
		case "models":
			return modelsCommand(ctx, args[1:], out, diagnostics)
		}
	}
	return sessionCommand(ctx, args, in, out, options)
}

func flagSet(name string, define func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	return fs
}

// parseWithConfig parses fs with a --config flag and exports the file as ZEL_CONFIG,
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
	return os.Setenv("ZEL_CONFIG", path)
}

func viewCommand(args []string) error {
	var session string
	fs := flagSet("view", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("view: %w", err)
	}
	if fs.NArg() == 0 {
		return errors.New("usage: zel view [-s ID] PATH [PATH ...]")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	endpoint, err := core.Endpoint(cwd, session)
	if err != nil {
		return err
	}
	home, err := core.ZelHome(cwd)
	if err != nil {
		return err
	}
	for _, path := range fs.Args() {
		image, err := core.CaptureImageCopy(path, filepath.Join(home, "attachments"))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err = core.Send(endpoint, core.Message{Image: &image}); err != nil {
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
		return errors.New("usage: zel models [--json]")
	}
	cwd, _ := os.Getwd() // only resolves a relative ZEL_HOME, which then reports its own error
	home, err := core.ZelHome(cwd)
	if err != nil {
		return err
	}
	settings, err := core.LoadConfig(home, cwd)
	if err != nil {
		return err
	}
	providers := settings.Providers
	if len(providers) == 0 {
		return errors.New("no providers configured: open zel to add a provider")
	}
	choices, errs := core.AllModels(ctx, providers)
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

func sessionCommand(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	var prompt, provider, model, effort string
	fs := flagSet("zel", func(fs *flag.FlagSet) {
		fs.StringVar(&prompt, "p", "", "run a prompt without the TUI")
		fs.StringVar(&provider, "provider", "", "provider for this run")
		fs.StringVar(&model, "model", "", "model for this run")
		fs.StringVar(&effort, "effort", "", "reasoning effort for this run")
	})
	if err := parseWithConfig(fs, args); err != nil {
		return fmt.Errorf("%w (see zel --help)", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	headless := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "p" {
			headless = true
		}
	})
	if headless && strings.TrimSpace(prompt) == "" {
		return errors.New("-p requires a nonempty prompt")
	}
	if !headless && (provider != "" || model != "" || effort != "") {
		return errors.New("--provider, --model and --effort require -p")
	}
	if !headless && (!isTerminal(in) || !isTerminal(out)) {
		return errors.New("the interactive TUI needs a terminal")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	opts := core.Options{Mode: "tui", Provider: provider, Model: model, Effort: effort}
	if headless {
		opts.Mode = "headless"
		opts.SessionReady = func(a *core.Runtime) error {
			if hint := a.Settings().SetupHint(); hint != "" {
				return errors.New(hint)
			}
			if err := a.Save(); err != nil {
				return err
			}
			_, err := fmt.Fprintln(out, a.SessionID())
			return err
		}
	}
	open := options.Open
	if open == nil {
		open = core.New
	}
	a, err := open(cwd, opts)
	if err != nil {
		return err
	}
	defer a.Close()
	if headless {
		if err := a.Run(ctx, prompt, func(core.TurnEvent) {}); err != nil {
			return cancelledOr(ctx, err)
		}
		_, err := fmt.Fprintln(out, a.FinalText())
		return cancelledOr(ctx, err)
	}
	running, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if options.Interactive == nil {
		return errors.New("interactive frontend is unavailable")
	}
	err = options.Interactive(running, a, cwd)
	if cause := context.Cause(running); ctx.Err() == nil && cause != nil {
		return cause
	}
	return cancelledOr(ctx, err)
}

func cancelledOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return cancelled
	}
	return err
}

// modelList is the JSON shape used by the models helper.
func modelList(choices []core.ModelChoice) []map[string]string {
	list := make([]map[string]string, 0, len(choices))
	for _, c := range choices {
		list = append(list, map[string]string{"provider": c.Provider, "model": c.Model})
	}
	return list
}

func readSessionCommand(args []string, out io.Writer) error {
	var id string
	fs := flagSet("session", func(fs *flag.FlagSet) { fs.StringVar(&id, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil {
		return err
	}
	if id == "" || fs.NArg() != 0 {
		return errors.New("usage: zel session -s SESSION_ID")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	records, err := core.ReadSession(cwd, id)
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(out, "[%s]\n%s\n\n", record.Kind, record.Text); err != nil {
			return err
		}
	}
	return nil
}
