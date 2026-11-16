package core

// Changes reserve the controller without holding its mutex across subprocesses
// or persistence. The reservation prevents session/config replacement and new
// turns; UI status reads and cancellation remain available.
func (c *Controller) change(kind string, work func() error, publish func(error)) error {
	c.mu.Lock()
	if err := c.requireIdle("Wait for the active turn or session change to finish."); err != nil {
		c.mu.Unlock()
		return err
	}
	c.beginChangeLocked(kind == "session")
	c.mu.Unlock()
	err := work()
	c.completeChange(err, publish)
	return err
}

func (c *Controller) beginChangeLocked(switching bool) {
	c.changing, c.switching = true, switching
	c.idle = make(chan struct{})
}

func (c *Controller) completeChange(err error, publish func(error)) {
	c.view(func() {
		c.changing, c.switching = false, false
		close(c.idle)
		c.idle = nil
		if publish != nil {
			publish(err)
		}
		c.settleLocked()
	})
}

// startReloadLocked is used at turn boundaries; it never runs config scripts
// under the controller lock or overlaps another reserved operation.
func (c *Controller) startReloadLocked() {
	c.reload = false
	c.beginChangeLocked(false)
	go func() {
		err := c.a.Reload()
		c.completeChange(err, c.publishReload)
	}()
}

func (c *Controller) publishReload(err error) {
	if err != nil {
		c.emit(Event{Kind: "error", Text: err.Error()})
	} else {
		c.emit(Event{Kind: "notice", Text: "zel.toml reloaded"})
	}
}
