package core

import (
	"context"
	"strings"
)

func (c *Controller) Input(input Input) error {
	if strings.TrimSpace(input.Text) == "" && len(input.Images) > 0 {
		input.Text = "Describe this image."
	}
	var err error
	c.view(func() {
		switch {
		case c.shut:
			err = fail(CodeFailed, "the session is closed")
		case strings.TrimSpace(input.Text) == "":
			err = fail(CodeInvalid, "input requires text or images")
		case c.active != nil:
			err = c.queueLocked(input)
		default:
			if err = c.setupError(); err == nil {
				if err = c.queueLocked(input); err == nil {
					c.deliverLocked()
				}
			}
		}
	})
	return err
}

func (c *Controller) queueLocked(input Input) error {
	if c.closing || c.quitPending {
		return fail(CodeBusy, "Session is changing; wait for the change to complete.")
	}
	if !c.a.Queue().Push(input) {
		return fail(CodeBusy, "Steering queue is full; your message remains in the input.")
	}
	return nil
}

func (c *Controller) Recall() (*Input, error) {
	var input Input
	var ok bool
	c.view(func() {
		if c.active != nil {
			input, ok = c.a.Queue().Recall()
		}
	})
	if !ok {
		return nil, fail(CodeBusy, "That instruction was already accepted and cannot be recalled.")
	}
	return &input, nil
}

// Stop cancels the active operation. The cancellation completes later as a done event.
func (c *Controller) Stop() {
	c.view(func() {
		if c.active != nil {
			c.active.cancelled = true
			c.active.cancel()
		}
	})
}

func (c *Controller) Quit() (closed bool, err error) {
	c.view(func() {
		if c.active != nil {
			c.quitPending = true
			c.active.cancelled = true
			c.active.cancel()
			return
		}
		err = c.closeLocked()
		closed = err == nil
	})
	return closed, err
}

func (c *Controller) NewSession() error { return c.switchRequest("new", "") }

func (c *Controller) Resume(sessionID string) error { return c.switchRequest("resume", sessionID) }

func (c *Controller) switchRequest(kind, id string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before starting or resuming a session."); err != nil {
			return
		}
		if err = c.switchLocked(kind, id); err != nil {
			err = fail(CodeFailed, "%s", err)
		}
	})
	return err
}

func (c *Controller) Reload() {
	c.view(func() {
		if c.active != nil {
			c.reload = true
			c.emit(Event{Kind: "notice", Text: "Instructions will reload after the current turn."})
			return
		}
		c.reloadLocked()
	})
}

func (c *Controller) reloadLocked() {
	if err := c.a.Reload(); err != nil {
		c.emit(Event{Kind: "error", Text: err.Error()})
		return
	}
	c.emit(Event{Kind: "notice", Text: "Settings and init.lua reloaded; the system prompt was rebuilt."})
}

func (c *Controller) Rename(title string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before renaming."); err != nil {
			return
		}
		if err = c.a.Rename(title); err != nil {
			err = fail(CodeFailed, "%s", err)
			return
		}
		text := "Session title reset to the first message."
		if title := c.a.CustomTitle(); title != "" {
			text = "Session renamed: " + title
		}
		c.emit(Event{Kind: "notice", Text: text, Title: c.titleLocked()})
	})
	return err
}

func (c *Controller) Models(ctx context.Context) (models []ModelChoice, failures map[string]string, err error) {
	var settings Settings
	c.view(func() {
		if c.shut {
			err = fail(CodeFailed, "the session is closed")
		}
		settings = c.a.Settings()
	})
	if err != nil {
		return nil, nil, err
	}
	models, errs := AllModels(ctx, settings.Providers)
	if ctx.Err() != nil {
		return nil, nil, fail(CodeFailed, "the model listing was cancelled")
	}
	failures = map[string]string{}
	for name, err := range errs {
		failures[name] = Redact(settings, err.Error())
	}
	return models, failures, nil
}

func (c *Controller) SelectModel(provider, model, effort string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing models."); err != nil {
			return
		}
		if provider == "" || model == "" {
			err = fail(CodeInvalid, "a model and its provider are required")
			return
		}
		err = c.selectLocked(provider, model, effort)
	})
	return err
}

func (c *Controller) selectLocked(provider, model, effort string) error {
	if _, ok := c.a.Settings().Providers[provider]; !ok {
		return fail(CodeInvalid, "provider not found: %s", provider)
	}
	if err := c.a.SelectSession(context.Background(), provider, model, effort); err != nil {
		return fail(CodeFailed, "%s", Redact(c.a.Settings(), err.Error()))
	}
	return nil
}

func (c *Controller) Tasks() (tasks []Task, err error) {
	c.view(func() { tasks, err = c.a.Tasks() })
	return tasks, err
}

// TaskOutput returns the tail of a session task's output log.
func (c *Controller) TaskOutput(id string) (string, error) {
	tasks, err := c.Tasks()
	if err != nil {
		return "", fail(CodeFailed, "%s", err)
	}
	for _, t := range tasks {
		if t.ID == id {
			return TaskOutput(t), nil
		}
	}
	return "", fail(CodeInvalid, "task not found: %s", id)
}

func (c *Controller) ActiveSubagents() (list []SubagentRun) {
	c.view(func() { list = c.a.ActiveSubagents() })
	return list
}

func (c *Controller) StopTask(id string) error {
	var err error
	c.view(func() {
		if err = c.a.StopTask(id); err != nil {
			err = fail(CodeInvalid, "%s", err.Error())
		}
	})
	return err
}

// Trace returns everything the session has done: the system prompt, the messages,
// the tool traffic the chat hides, the tasks and the events log.
func (c *Controller) Trace() []TraceEntry {
	var history []TraceEntry
	var slow func() []TraceEntry
	c.view(func() { history, slow = c.a.traceSources() })
	// The task rows and the events log are read without the controller lock.
	return mergeTrace(history, slow())
}

// LogEvent records a notice or error shown by a client in the session's events log.
func (c *Controller) LogEvent(kind, text string) {
	c.view(func() { c.a.LogEvent(kind, text) })
}

// Info reads the session totals from the history.
func (c *Controller) Info() (info Info, err error) {
	c.view(func() {
		s := c.a.Settings()
		info = Info{Session: c.a.SessionID(), Title: c.titleLocked(), Project: c.a.Project(), DB: s.DBPath,
			Provider: s.Provider, Model: s.Model, Effort: s.Effort, Configs: c.a.ConfigFiles(),
			Messages: c.a.UserMessages(), Tasks: c.a.RunningTasks(), Context: c.a.ContextTokens()}
		info.Usage, info.Requests = c.a.SessionUsage()
	})
	return info, nil
}

// Withdraw cancels a composer message only while it is still queued.
func (c *Controller) Withdraw(id uint64) (removed bool) {
	c.view(func() { removed = c.a.Queue().Withdraw(id) })
	return removed
}

// UpdateSettings persists UI preferences and providers while the session is idle.
func (c *Controller) UpdateSettings(s Settings) (err error) {
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		err = c.a.UpdateSettings(s)
	})
	return err
}

// StartTask runs Lua code as a background task of the current session.
func (c *Controller) StartTask(command string) (id string, err error) {
	c.view(func() { id, err = c.a.StartTask(command) })
	return id, err
}
