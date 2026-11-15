package cli

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	case "version", "help", "--help", "-h":
		return true
	}
	return false
}

// removed are the commands and flags 0.6 dropped, with what to do instead.
var removed = map[string]string{
	"init":     "create .zel/init.lua yourself; it returns a table, see zel help",
	"session":  "resume sessions from the TUI (Ctrl+R) and read them in Tracing (Ctrl+T)",
	"models":   "pick models in the TUI (Ctrl+M)",
	"events":   "notices and errors are in Tracing (Ctrl+T)",
	"--config": "put the file in ~/.zel/init.lua or .zel/init.lua",
	"-config":  "put the file in ~/.zel/init.lua or .zel/init.lua",
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

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	w := &failedWriter{w: out}
	err := run(ctx, args, in, w, options)
	if err == nil && w.err != nil {
		err = fmt.Errorf("write output: %w", w.err)
	}
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	if len(args) > 0 {
		name, _, _ := strings.Cut(args[0], "=")
		if hint, ok := removed[name]; ok {
			return fmt.Errorf("%s was removed in zel 0.6: %s", name, hint)
		}
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
		case "update":
			return updateCommand(ctx, args[1:], out)
		}
	}
	return sessionCommand(ctx, args, in, out, options)
}

func sessionCommand(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	var prompt, provider, model, effort string
	fs := flag.NewFlagSet("zel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&prompt, "p", "", "run a prompt without the TUI")
	fs.StringVar(&provider, "provider", "", "provider for this run")
	fs.StringVar(&model, "model", "", "model for this run")
	fs.StringVar(&effort, "effort", "", "reasoning effort for this run")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w (see zel --help)", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown command: %s (see zel --help)", strings.Join(fs.Args(), " "))
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
		expanded, err := a.ExpandPrompt(ctx, prompt)
		if err != nil {
			return cancelledOr(ctx, err)
		}
		if err := a.Run(ctx, expanded, func(core.TurnEvent) {}); err != nil {
			return cancelledOr(ctx, err)
		}
		_, err = fmt.Fprintln(out, a.FinalText())
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
