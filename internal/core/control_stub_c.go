package core

import (
	"time"

	"zel/internal/config"
)

type Config = config.Config
type System = config.System
type Function = config.Function
type Hook = config.Hook
type Command = config.Command

// Indicator is one active `zel status new` indicator.
type Indicator struct {
	ID, Text string
	Started  time.Time
}

// Config returns the current config snapshot, or nil before load.
func (c *Controller) Config() *config.Config {
	return c.a.session().conf.get()
}

// RunCommand runs a configured command asynchronously.
// Stub until workstream A is merged.
func (c *Controller) RunCommand(name string) error {
	return fail(CodeFailed, "command %q requires workstream A", name)
}

// Indicators returns active `zel status new` indicators in creation order.
// Stub until workstream B is merged.
func (c *Controller) Indicators() []Indicator {
	return nil
}

// CallStatus returns the `zel status` text of the running call, or "".
// Stub until workstream B is merged.
func (c *Controller) CallStatus() string {
	return ""
}
