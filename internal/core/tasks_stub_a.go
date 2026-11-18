package core

import "time"

// Stub of workstream A: background tasks are gone. The TUI still reads the
// task rows; workstream B/C replace them with the indicators of `zel status
// new`. Delete this file when the TUI no longer calls ActiveTasks.

// ActiveTask is a row above the working line.
type ActiveTask struct {
	ID, Name string
	Started  time.Time
}

// ActiveTasks is always empty.
func (a *Runtime) ActiveTasks() []ActiveTask { return nil }

// ActiveTasks is always empty.
func (c *Controller) ActiveTasks() []ActiveTask { return nil }
