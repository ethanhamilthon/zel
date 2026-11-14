package cli

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"zel/internal/core"
)

// eventsCommand prints the notices and errors a session showed in its UI.
func eventsCommand(args []string, out io.Writer) error {
	var session string
	fs := flagSet("events", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: zel events [-s ID]")
	}
	session = cmp.Or(session, os.Getenv("ZEL_SESSION_ID"))
	if session == "" {
		return errors.New("outside a zel shell, pass -s SESSION_ID")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := core.ZelHome(cwd)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(core.EventsLog(home, session))
	if errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintln(out, "no events")
		return err
	}
	if err == nil {
		_, err = out.Write(data)
	}
	return err
}
