package core

import (
	"fmt"
	"strings"

	"zel/internal/config"
)

// RunCommand starts the command of the config called name, from the menu or by
// its key. It returns at once: the command runs in its own VM without a
// timeout, its returned text is shown like `zel print` and an error is a red
// line in the chat.
func (c *Controller) RunCommand(name string) error {
	var cmd config.Command
	var found bool
	s := c.a.session()
	lc := s.luaConfig()
	if lc != nil {
		for _, x := range lc.Data().Commands {
			if x.Name == name {
				cmd, found = x, true
			}
		}
	}
	if !found {
		return fail(CodeNotFound, "ENOENT: no command %q", name)
	}
	go func() {
		out, err := lc.RunCommand(s.hookCtx, cmd)
		c.view(func() {
			switch {
			case s.hookCtx.Err() != nil:
			case err != nil:
				c.emit(Event{Kind: "command_error", Text: fmt.Sprintf("command %s: %s", name, strings.TrimSpace(err.Error()))})
			case out != "":
				c.printLine(out)
			}
		})
	}()
	return nil
}
