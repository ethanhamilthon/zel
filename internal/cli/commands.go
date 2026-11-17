package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zel/docs"
	"zel/internal/config"
	"zel/internal/core"
	"zel/internal/edit"
	"zel/internal/ipc"
	"zel/internal/shell"
)

// usageError is EINVAL with the usage of a subcommand.
func usageError(cmd, usage string) error {
	return &ExitError{Code: 2, Err: fmt.Errorf("zel %s: EINVAL: usage: zel %s", cmd, usage)}
}

// parseFlags parses flags that may come before, between or after the positional
// arguments; everything after "--" is positional.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(positional, rest...), nil
		}
		positional, args = append(positional, rest[0]), rest[1:]
	}
}

// call sends one request to the session this command runs in and prints the answer.
func call(ctx context.Context, cmd, op string, args any, out io.Writer) (ipc.Response, error) {
	socket, token := os.Getenv("ZEL_SOCKET"), os.Getenv("ZEL_TOKEN")
	if socket == "" || token == "" {
		return ipc.Response{}, &ExitError{Code: 2, Err: fmt.Errorf("zel %s: EINVAL: not inside a zel session (no ZEL_SOCKET)", cmd)}
	}
	data, _ := json.Marshal(args)
	cwd, _ := os.Getwd()
	r, err := ipc.Call(ctx, socket, ipc.Request{Token: token, Op: op, Task: os.Getenv("ZEL_TASK"), Cwd: cwd, Args: data})
	if err != nil {
		if ctx.Err() != nil {
			return r, cancelled
		}
		return r, &ExitError{Code: 1, Err: fmt.Errorf("zel %s: %v", cmd, err)}
	}
	if _, err := io.WriteString(out, r.Out); err != nil {
		return r, err
	}
	if r.Error != "" || !r.OK {
		code := r.Code
		if code == 0 {
			code = 1
		}
		return r, &ExitError{Code: code, Err: fmt.Errorf("zel %s: %s", cmd, r.Error)}
	}
	return r, nil
}

// shellJoin joins argv into one bash command line, quoting where needed. A
// single argument is taken as a script as it is.
func shellJoin(argv []string) string {
	if len(argv) == 1 {
		return argv[0]
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

const taskUsage = "task run [--name N] [--timeout S] [--cwd DIR] -- CMD... | list | get ID | tail ID [-n N] | wait ID... [--timeout S] | stop ID"

func taskCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return usageError("task", taskUsage)
	}
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	name := fs.String("name", "", "")
	timeout := fs.Float64("timeout", 0, "")
	dir := fs.String("cwd", "", "")
	n := fs.Int("n", 20, "")
	rest, err := parseFlags(fs, args[1:])
	if err != nil {
		return usageError("task", taskUsage)
	}
	op := args[0]
	switch {
	case op == "run":
		command := shellJoin(rest)
		if len(rest) == 0 {
			data, err := io.ReadAll(in)
			if err != nil {
				return err
			}
			command = string(data)
		}
		if strings.TrimSpace(command) == "" {
			return usageError("task", taskUsage)
		}
		_, err = call(ctx, "task", "task.run", map[string]any{"name": *name, "cmd": command, "timeout": *timeout, "cwd": taskDir(*dir)}, out)
	case op == "list" && len(rest) == 0:
		_, err = call(ctx, "task", "task.list", nil, out)
	case (op == "get" || op == "stop") && len(rest) == 1:
		_, err = call(ctx, "task", "task."+op, map[string]any{"id": rest[0]}, out)
	case op == "tail" && len(rest) == 1:
		_, err = call(ctx, "task", "task.tail", map[string]any{"id": rest[0], "n": *n}, out)
	case op == "wait" && len(rest) > 0:
		_, err = call(ctx, "task", "task.wait", map[string]any{"ids": rest, "timeout": *timeout}, out)
	default:
		return usageError("task", taskUsage)
	}
	return err
}

// taskDir is the folder of a new task: --cwd (relative to the caller's folder),
// else the caller's folder, which is where the agent's calls start.
func taskDir(flag string) string {
	cwd := cwdOrEmpty()
	if flag == "" || filepath.IsAbs(flag) {
		return cmp.Or(flag, cwd)
	}
	return filepath.Join(cwd, flag)
}

func cwdOrEmpty() string {
	cwd, _ := os.Getwd()
	return cwd
}

func choiceCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 {
		return usageError("choice", `choice "Question" "Option" ...`)
	}
	_, err := call(ctx, "choice", "choice.ask", map[string]any{"title": args[0], "options": args[1:]}, out)
	return err
}

func viewCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usageError("view", "view IMAGE...")
	}
	_, err := call(ctx, "view", "view.add", map[string]any{"paths": args}, out)
	return err
}

func sessionInfoCommand(ctx context.Context, args []string, out io.Writer) error {
	switch {
	case len(args) > 1 && args[0] == "rename":
		_, err := call(ctx, "session", "session.rename", map[string]any{"name": strings.Join(args[1:], " ")}, out)
		return err
	case len(args) == 0:
		_, err := call(ctx, "session", "session.get", nil, out)
		return err
	case len(args) == 1 && args[0] == "--json":
		r, err := call(ctx, "session", "session.get", nil, io.Discard)
		if err == nil {
			_, err = fmt.Fprintln(out, string(r.Data))
		}
		return err
	}
	return usageError("session", "session [--json] | session rename NAME")
}

func cdCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 1 {
		return usageError("cd", "cd [DIR]")
	}
	dir := ""
	if len(args) == 1 {
		if dir = args[0]; dir == "" {
			return usageError("cd", "cd [DIR]")
		}
	}
	_, err := call(ctx, "cd", "cd", map[string]any{"dir": dir, "cwd": cwdOrEmpty()}, out)
	return err
}

func editCommand(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	rest, err := parseFlags(fs, args)
	if err != nil || len(rest) != 1 {
		return usageError("edit", "edit FILE [--all] <<'EOF' blocks EOF")
	}
	fail := func(err error) error {
		code := 1
		var e *edit.Error
		if errors.As(err, &e) {
			code = map[string]int{"EINVAL": 2, "ENOMATCH": 4, "EAMBIG": 5}[e.Code]
		} else if errors.Is(err, os.ErrNotExist) {
			code, err = 3, fmt.Errorf("ENOENT: %v", err)
		} else {
			err = fmt.Errorf("EFAIL: %v", err)
		}
		return &ExitError{Code: code, Err: fmt.Errorf("zel edit: %v", err)}
	}
	path := rest[0]
	info, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	input, err := io.ReadAll(in)
	if err != nil {
		return fail(err)
	}
	blocks, err := edit.Parse(string(input))
	if err != nil {
		return fail(err)
	}
	text, n, err := edit.Apply(string(data), blocks, *all)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(path, []byte(text), info.Mode().Perm()); err != nil {
		return fail(err)
	}
	_, err = fmt.Fprintln(out, n)
	return err
}

func docsCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(out, docs.Index())
		return err
	}
	text, ok := docs.Get(args[0])
	if !ok || len(args) > 1 {
		return &ExitError{Code: 2, Err: fmt.Errorf("zel docs: EINVAL: unknown topic %q\n%s", strings.Join(args, " "), docs.Index())}
	}
	_, err := fmt.Fprintln(out, text)
	return err
}

// configPath is the config file a command uses: --config, else the session's
// (ZEL_CONFIG), else ~/.zel/config.toml.
func configPath(cwd, flag string) (string, error) {
	if flag == "" {
		flag = os.Getenv("ZEL_CONFIG")
	}
	if flag == "" {
		home, err := core.ZelHome(cwd)
		return config.File(home), err
	}
	flag, err := config.ResolvePath(flag)
	if err != nil {
		return "", err
	}
	return filepath.Abs(flag)
}

func loadConfig(ctx context.Context, flag string) (*config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, err := configPath(cwd, flag)
	if err != nil {
		return nil, err
	}
	run := func(env map[string]string) config.Expander {
		return func(ctx context.Context, script string) (string, error) {
			cmd := shell.Command(ctx, cwd, env, script)
			cmd.Stdin = strings.NewReader("")
			var buf strings.Builder
			cmd.Stdout = &buf
			if err := cmd.Start(); err != nil {
				return "", err
			}
			err := shell.Reap(cmd)
			return buf.String(), err
		}
	}
	return config.Load(ctx, path, run), nil
}

func modelsCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	file := fs.String("config", "", "")
	if rest, err := parseFlags(fs, args); err != nil || len(rest) > 0 {
		return usageError("models", "models [--json] [--config FILE]")
	}
	cfg, err := loadConfig(ctx, *file)
	if err != nil {
		return err
	}
	models, failures := core.AllModels(ctx, cfg.Settings.Providers)
	if *asJSON {
		data, _ := json.Marshal(map[string]any{"models": models, "failures": failures})
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	for _, m := range models {
		line := m.Provider + " " + m.Model
		if m.ContextWindow > 0 {
			line += fmt.Sprintf(" %d", m.ContextWindow)
		}
		fmt.Fprintln(out, line)
	}
	names := make([]string, 0, len(failures))
	for name := range failures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "%s: error: %s\n", name, failures[name])
	}
	return nil
}

func configCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	file := fs.String("config", "", "")
	if rest, err := parseFlags(fs, args); err != nil || len(rest) > 0 {
		return usageError("config", "config [--config FILE]")
	}
	cfg, err := loadConfig(ctx, *file)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(out, cfg.Describe())
	return err
}
