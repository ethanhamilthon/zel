package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"zel/internal/core"
)

var socketUsage = map[string]string{
	"steer":   "steer [-s ID] [--file PATH]... [TEXT|-]",
	"status":  "status [-s ID] TEXT | status [-s ID] new TEXT | status [-s ID] -i ID (TEXT|--done)",
	"choice":  "choice [-s ID] QUESTION OPTION...",
	"print":   "print [-s ID] TEXT|-",
	"session": "session [-s ID] --json",
	"rename":  "rename [-s ID] NAME",
}

func socketCommand(ctx context.Context, command string, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	id := fs.String("s", "", "")
	var a core.SessionArgs
	var asJSON bool
	switch command {
	case "steer":
		fs.Func("file", "", func(path string) error {
			if path == "" {
				return errors.New("empty file path")
			}
			a.Files = append(a.Files, path)
			return nil
		})
	case "status":
		fs.StringVar(&a.ID, "i", "", "")
		fs.BoolVar(&a.Done, "done", false, "")
	case "session":
		fs.BoolVar(&asJSON, "json", false, "")
	}
	usage := func() error { return usageError(command, socketUsage[command]) }
	pos, err := parseFlags(fs, args)
	if err != nil {
		return usage()
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "s" {
			explicit = true
		}
		if f.Name == "i" && a.ID == "" {
			err = usage()
		}
	})
	if err != nil || (explicit && *id == "") {
		return usage()
	}
	op := command
	readStdin := false
	switch command {
	case "steer", "print":
		switch {
		case len(pos) == 1 && pos[0] == "-":
			readStdin = true
		case len(pos) == 0 && command == "steer" && len(a.Files) == 0 && !isTerminal(in):
			readStdin = true
		case len(pos) == 0 && len(a.Files) == 0:
			return usage()
		default:
			a.Text = strings.Join(pos, " ")
		}
	case "status":
		if a.ID != "" {
			op = "status.update"
			if (len(pos) == 0) != a.Done {
				return usage()
			}
		} else if a.Done || len(pos) == 0 {
			return usage()
		} else if pos[0] == "new" {
			if len(pos) == 1 {
				return usage()
			}
			op, pos = "status.new", pos[1:]
		}
		a.Text = strings.Join(pos, " ")
	case "choice":
		if len(pos) < 2 {
			return usage()
		}
		a.Text, a.Options = pos[0], pos[1:]
	case "session":
		if !asJSON || len(pos) != 0 {
			return usage()
		}
	case "rename":
		if len(pos) == 0 {
			return usage()
		}
		a.Name = strings.Join(pos, " ")
	}

	if *id == "" && os.Getenv("ZEL_SESSION_ID") == "" {
		return &ExitError{Code: 1, Err: fmt.Errorf("zel %s: EFAIL: no session (set ZEL_SESSION_ID or use -s ID)", command)}
	}
	if readStdin {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		a.Text = string(data)
	}
	text, code, err := core.CallSession(ctx, *id, op, a)
	if err != nil {
		return &ExitError{Code: code, Err: fmt.Errorf("zel %s: %w", command, err)}
	}
	if text != "" {
		_, err = fmt.Fprintln(out, text)
	}
	return err
}
