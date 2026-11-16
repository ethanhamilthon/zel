package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	case "version", "help", "--help", "-h", "task", "choice", "view", "session", "cd", "edit", "docs", "models", "config":
		return true
	}
	return false
}

// removed are the commands and flags 0.7 dropped, with what to do instead.
var removed = map[string]string{
	"init":   "put your settings in ~/.zel/zel.toml or ./zel.toml (see zel help)",
	"events": "notices and errors are in Tracing (Ctrl+T)",
	"agent":  "a subagent is zel -p \"prompt\" run from the agent's bash",
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
	Stderr      io.Writer // default os.Stderr
}

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	w := &failedWriter{w: out}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
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
			return fmt.Errorf("%s was removed in zel 0.7: %s", name, hint)
		}
		rest := args[1:]
		switch args[0] {
		case "version", "help", "--help", "-h":
			if len(rest) > 0 {
				return fmt.Errorf("%s takes no arguments", args[0])
			}
			if args[0] == "version" {
				fmt.Fprintln(out, strings.TrimPrefix(Version, "v"))
			} else {
				fmt.Fprint(out, helpText)
			}
			return nil
		case "update":
			return updateCommand(ctx, rest, out)
		case "task":
			return taskCommand(ctx, rest, in, out)
		case "choice":
			return choiceCommand(ctx, rest, out)
		case "view":
			return viewCommand(ctx, rest, out)
		case "session":
			return sessionInfoCommand(ctx, rest, out)
		case "cd":
			return cdCommand(ctx, rest, out)
		case "edit":
			return editCommand(rest, in, out)
		case "docs":
			return docsCommand(rest, out)
		case "models":
			return modelsCommand(ctx, rest, out)
		case "config":
			return configCommand(ctx, rest, out)
		}
	}
	return sessionCommand(ctx, args, in, out, options)
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func sessionCommand(ctx context.Context, args []string, in io.Reader, out io.Writer, options Options) error {
	var prompt, provider, model, effort, resume, dir string
	var configs multi
	var asJSON bool
	var timeout float64
	fs := flag.NewFlagSet("zel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&prompt, "p", "", "run a prompt without the TUI")
	fs.StringVar(&provider, "provider", "", "provider for this run")
	fs.StringVar(&model, "model", "", "model for this run")
	fs.StringVar(&effort, "effort", "", "reasoning effort for this run")
	fs.StringVar(&resume, "resume", "", "continue this session")
	fs.StringVar(&dir, "cwd", "", "working directory")
	fs.Float64Var(&timeout, "timeout", 0, "seconds before the run is cancelled")
	fs.BoolVar(&asJSON, "json", false, "print {ok, answer, session, error}")
	fs.Var(&configs, "config", "extra zel.toml layer (repeatable)")
	// -p may come without its value when flags follow: zel -p --resume ID "text".
	args = slices.Clone(args)
	for i, a := range args {
		if (a == "-p" || a == "--p") && (i+1 == len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-")) {
			args[i] = "-p="
		}
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return fmt.Errorf("%w (see zel --help)", err)
	}
	headless := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "p" {
			headless = true
		}
	})
	if len(positional) > 0 {
		if !headless || prompt != "" {
			return fmt.Errorf("unknown command: %s (see zel --help)", strings.Join(positional, " "))
		}
		prompt = strings.Join(positional, " ")
	}
	if headless && (prompt == "" || prompt == "-") && !isTerminal(in) {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		prompt = string(data)
	}
	if headless && strings.TrimSpace(prompt) == "" {
		return errors.New("-p requires a nonempty prompt")
	}
	if !headless && (provider != "" || model != "" || effort != "" || resume != "" || asJSON || timeout != 0 || dir != "") {
		return errors.New("--provider, --model, --effort, --resume, --cwd, --timeout and --json require -p")
	}
	if !headless && (!isTerminal(in) || !isTerminal(out)) {
		return errors.New("the interactive TUI needs a terminal")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if dir != "" {
		if cwd, err = filepath.Abs(dir); err != nil {
			return err
		}
	}
	opts := core.Options{Mode: "tui", Provider: provider, Model: model, Effort: effort, Configs: configs, Resume: resume}
	var session string
	if headless {
		opts.Mode, opts.Prompt = "headless", prompt
		opts.SessionReady = func(a *core.Runtime) error {
			if hint := a.Settings().SetupHint(); hint != "" {
				return errors.New(hint)
			}
			if err := a.Save(); err != nil {
				return err
			}
			session = a.SessionID()
			if !asJSON {
				fmt.Fprintln(options.Stderr, "zel: session "+session)
			}
			return nil
		}
	}
	open := options.Open
	if open == nil {
		open = core.New
	}
	a, err := open(cwd, opts)
	if err != nil {
		if headless && asJSON {
			writeJSON(out, false, "", session, err.Error())
			return &ExitError{Code: exitFor(err), Err: err, Reported: true}
		}
		return &ExitError{Code: exitFor(err), Err: err}
	}
	defer a.Close()
	if headless {
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
			defer cancel()
		}
		err := a.Run(ctx, prompt, func(core.TurnEvent) {})
		if asJSON {
			if err != nil {
				msg := err.Error()
				if ctx.Err() != nil {
					msg = "cancelled"
				}
				writeJSON(out, false, a.FinalText(), session, msg)
				return &ExitError{Code: 1, Err: err, Reported: true}
			}
			writeJSON(out, true, a.FinalText(), session, "")
			return nil
		}
		if err != nil {
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

func writeJSON(out io.Writer, ok bool, answer, session, msg string) {
	data, _ := json.Marshal(map[string]any{"ok": ok, "answer": answer, "session": session, "error": msg})
	fmt.Fprintln(out, string(data))
}

// exitFor maps "CODE: msg" errors of core to the exit codes of the subcommands.
func exitFor(err error) int {
	var e *core.Error
	if errors.As(err, &e) {
		switch e.Code {
		case core.CodeDenied:
			return 6
		case core.CodeNotFound:
			return 3
		case core.CodeInvalid:
			return 2
		}
	}
	return 1
}

func cancelledOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return cancelled
	}
	return err
}
