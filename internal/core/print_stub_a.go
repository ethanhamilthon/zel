package core

// Stub of workstream A until the `print` records of workstream B are merged:
// the output of a command is shown as a notice. Replace the body with the
// print record when it arrives (and delete this file).
func (c *Controller) printLine(text string) { c.emit(Event{Kind: "notice", Text: text}) }
