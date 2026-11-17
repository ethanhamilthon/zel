package core

import (
	"cmp"
	"context"
	"strings"
)

func (c *Controller) Input(input Input) error {
	session := c.a.session()
	if strings.TrimSpace(input.Text) == "" && len(input.Images) > 0 {
		input.Text = "Describe this image."
	}
	var err error
	c.view(func() {
		switch {
		case c.a.session() != session:
			err = fail(CodeBusy, "Session changed while preparing the input; please retry.")
		case c.shut:
			err = fail(CodeFailed, "the session is closed")
		case strings.TrimSpace(input.Text) == "":
			err = fail(CodeInvalid, "input requires text or images")
		case c.changing:
			if c.switching {
				err = fail(CodeBusy, "Session is changing; please retry.")
			} else {
				err = c.queueLocked(input)
			}
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
		if c.closing {
			return
		}
		if c.changing {
			c.quitPending = true
			return
		}
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
	var snapshot Session
	return c.change("session", func() error {
		var err error
		if kind == "resume" {
			err = c.a.Resume(id)
		} else {
			err = c.a.NewSession()
		}
		if err != nil {
			return fail(CodeFailed, "%s", err)
		}
		snapshot = c.sessionLocked() // stable reservation, no controller mutex held
		return nil
	}, func(err error) {
		if err == nil {
			c.emit(Event{Kind: "session", Session: &snapshot})
		}
	})
}

// Reload applies configuration between turns. The actual work runs outside mu.
func (c *Controller) Reload() {
	c.mu.Lock()
	if c.shut || c.closing {
		c.mu.Unlock()
		return
	}
	if c.active != nil || c.changing {
		c.reload = true
		c.mu.Unlock()
		return
	}
	c.beginChangeLocked(false)
	c.mu.Unlock()
	err := c.a.Reload()
	c.completeChange(err, c.publishReload)
}

func (c *Controller) Rename(title string) error {
	return c.change("settings", func() error { return c.a.Rename(title) }, func(err error) {
		if err != nil {
			return
		}
		text := "Session title reset to the first message."
		if title := c.a.CustomTitle(); title != "" {
			text = "Session renamed: " + title
		}
		c.emit(Event{Kind: "notice", Text: text, Title: c.titleLocked()})
	})
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
		failures[name] = err.Error()
	}
	return models, failures, nil
}

func (c *Controller) SelectModel(provider, model, effort string) error {
	if provider == "" || model == "" {
		return fail(CodeInvalid, "a model and its provider are required")
	}
	return c.change("settings", func() error { return c.selectModel(provider, model, effort) }, nil)
}

func (c *Controller) selectModel(provider, model, effort string) error {
	if _, ok := c.a.Settings().Providers[provider]; !ok {
		return fail(CodeInvalid, "provider not found: %s", provider)
	}
	if err := c.a.SelectSession(context.Background(), provider, model, effort); err != nil {
		return fail(CodeFailed, "%s", err)
	}
	return nil
}

// ActiveTasks lists the running background tasks for the chat rows.
// PendingChoice is the question the agent asks with choice(), nil when there is none.
func (c *Controller) PendingChoice() *Choice { return c.a.PendingChoice() }

// AnswerChoice answers the question id with the option at index, or cancels it with -1.
func (c *Controller) AnswerChoice(id, index int) (Record, bool) { return c.a.AnswerChoice(id, index) }

func (c *Controller) AnswerChoiceText(id int, text string) (Record, bool) {
	return c.a.AnswerChoiceText(id, text)
}

func (c *Controller) ActiveTasks() (list []ActiveTask) {
	c.view(func() { list = c.a.ActiveTasks() })
	return list
}

// Trace returns everything the session has done: the system prompt, the messages,
// the tool traffic the chat hides, the tasks and the events log.
func (c *Controller) Trace() []TraceEntry {
	var history []TraceEntry
	var slow func() []TraceEntry
	history, slow = c.a.session().traceSources()
	// The task rows and the events log are read without the controller lock.
	return mergeTrace(history, slow())
}

// LogEvent records a notice or error shown by a client in the session's events log.
func (c *Controller) LogEvent(kind, text string) {
	c.view(func() { c.a.LogEvent(kind, text) })
}

// Info reads the session totals from the history.
func (c *Controller) Info() (info Info, err error) {
	session := c.a.session()
	s := session.Settings()
	info = Info{Session: session.id, Title: cmp.Or(session.CustomTitle(), session.DefaultTitle()), Project: session.project, Provider: s.Provider, Model: s.Model, Effort: s.Effort, Config: session.path, Messages: session.UserMessages(), Tasks: session.RunningTasks(), Context: session.ContextTokens()}
	info.Usage, info.Requests = session.SessionUsage()
	return info, nil
}

// Withdraw cancels a composer message only while it is still queued.
func (c *Controller) Withdraw(id uint64) (removed bool) {
	c.view(func() { removed = c.a.Queue().Withdraw(id) })
	return removed
}
