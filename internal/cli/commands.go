package cli

import (
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

// configPath resolves --config relative to the process working directory.
func configPath(_, flag string) (string, error) {
	p, err := config.ResolvePath(flag)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func loadConfig(flag string) (*config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, err := core.ZelHome(cwd)
	if err != nil {
		return nil, err
	}
	if flag == "" {
		flag = os.Getenv("ZEL_CONFIG")
	}
	paths := config.DefaultPaths(home, cwd, flag)
	if flag == "" && os.Getenv("ZEL_NO_LOCAL") == "1" {
		paths.Local = ""
	}
	cfg := &config.Config{
		Paths:    paths,
		Settings: config.DefaultSettings(),
	}
	conn, err := config.LoadConnect(paths.Connect)
	if err == nil {
		cfg.Settings.Providers = conn.Providers
		if conn.Provider != "" {
			cfg.Settings.Provider = conn.Provider
		}
		if conn.Model != "" {
			cfg.Settings.Model = conn.Model
		}
		if conn.Effort != "" {
			cfg.Settings.Effort = conn.Effort
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cfg.Errors = append(cfg.Errors, err.Error())
	}
	return cfg, nil
}

func modelsCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	file := fs.String("config", "", "")
	if rest, err := parseFlags(fs, args); err != nil || len(rest) > 0 {
		return usageError("models", "models [--json] [--config FILE]")
	}
	cfg, err := loadConfig(*file)
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

func configCommand(_ context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	file := fs.String("config", "", "")
	if rest, err := parseFlags(fs, args); err != nil || len(rest) > 0 {
		return usageError("config", "config [--config FILE]")
	}
	cfg, err := loadConfig(*file)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(out, config.Describe(cfg))
	return err
}
