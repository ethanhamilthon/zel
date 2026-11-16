package core

import (
	"cmp"
	"context"
	"errors"
	"sync"
	"time"
	"zel/internal/config"
)

// Controller is the single entry point for the TUI. One turn is
// active at a time; inputs arriving meanwhile become steering. It keeps no copy of
// session state: clients follow events and read snapshots from the agent while idle.
// Events are emitted under the controller lock, so emit must not block and must
// not call back into the controller.
type Controller struct {
	a    *Runtime
	emit func(Event)

	mu          sync.Mutex
	active      *operation
	idle        chan struct{}
	quitPending bool
	reload      bool
	closing     bool
	shut        bool
	closed      chan struct{}
}

type operation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	cancelled bool
}

// NewController coordinates a runtime, which must outlive the controller.
func NewController(a *Runtime, emit func(Event)) *Controller {
	c := &Controller{a: a, emit: emit, closed: make(chan struct{})}
	c.mu.Lock()
	defer c.mu.Unlock()
	a.Queue().OnPush(func() { go c.view(func() { c.deliverLocked() }) })
	a.OnConfigChanged(func() { go c.Reload() })
	c.deliverLocked()
	return c
}

func (c *Controller) view(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

func (c *Controller) Closed() <-chan struct{} { return c.closed }

func (c *Controller) Status() (s Status) {
	c.view(func() {
		s = Status{Tasks: c.a.RunningTasks(), Session: c.a.SessionID()}
		if c.active != nil {
			s.Busy, s.Cancelling = true, c.active.cancelled
			s.Started = c.active.started
			s.Queued = c.a.Queue().Pending()
			live, since := c.a.LiveActivity()
			s.Activity, s.Live, s.LiveSince = cmp.Or(live, c.a.LastActivity()), live != "", since
		}
	})
	return s
}

// Session returns the current session snapshot. The agent changes its history
// during a turn, so the snapshot is only available while idle.
func (c *Controller) Session() (s Session, err error) {
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before reading the session."); err == nil {
			s = c.sessionLocked()
		}
	})
	return s, err
}

func (c *Controller) sessionLocked() Session {
	return Session{ID: c.a.SessionID(), Title: c.titleLocked(), Context: c.a.ContextTokens(), Records: c.a.Records()}
}

// titleLocked reads the agent, so it is only called while no turn runs.
func (c *Controller) titleLocked() string {
	return cmp.Or(c.a.CustomTitle(), c.a.DefaultTitle())
}

func (c *Controller) Sessions() (sessions []SessionSummary, err error) {
	c.view(func() { sessions, err = c.a.Sessions() })
	return sessions, err
}

// Appearance is an immutable, map-free snapshot for the hot UI path.
func (c *Controller) Appearance() (config.Colors, UI) {
	s := c.a.session()
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	return s.cfg.s.Colors, s.cfg.s.UI
}

func (c *Controller) Settings() (s Settings) {
	c.view(func() { s = c.a.Settings() })
	return s
}

// Close cancels any active operation and waits for it to finish. It does not save.
func (c *Controller) Close() {
	c.view(func() {
		c.closing = true
		if c.active != nil {
			c.active.cancelled = true
			c.active.cancel()
		}
	})
	c.wait()
}

func (c *Controller) wait() {
	for {
		var idle chan struct{}
		c.view(func() { idle = c.idle })
		if idle == nil {
			return
		}
		<-idle
	}
}

func (c *Controller) closeLocked() error {
	if c.shut {
		return nil
	}
	c.closing = true
	if c.a.HasSavedSession() {
		if err := c.a.Save(); err != nil {
			c.closing = false
			return err
		}
	}
	c.shut = true
	close(c.closed)
	return nil
}

func (c *Controller) beginLocked() *operation {
	ctx, cancel := context.WithCancel(context.Background())
	c.a.ResetActivity()
	c.active = &operation{ctx: ctx, cancel: cancel, started: time.Now()}
	c.idle = make(chan struct{})
	return c.active
}

func (c *Controller) beginTurnLocked(input Input) {
	title := c.titleLocked()
	op := c.beginLocked()
	c.emit(Event{Kind: "started", Text: input.Text, Title: cmp.Or(title, FirstLine(input.Text))})
	go func() { c.finishTurn(op, c.a.RunInput(op.ctx, input, func(e TurnEvent) { c.observe(e) })) }()
}

func (c *Controller) endLocked(op *operation) {
	op.cancel()
	c.active = nil
	close(c.idle)
	c.idle = nil
}

func (c *Controller) observe(e TurnEvent) {
	c.view(func() {
		if isStatus(e) {
			c.emit(Event{Kind: e.Kind, Text: e.Text})
		} else {
			c.emit(Event{Kind: "agent", Agent: &e})
		}
	})
}

func (c *Controller) finishTurn(op *operation, err error) {
	c.view(func() {
		status, text := "ok", ""
		switch {
		case op.cancelled || errors.Is(err, context.Canceled):
			status = "cancelled"
		case err != nil:
			status, text = "error", err.Error()
		}
		if status == "cancelled" {
			c.emit(Event{Kind: "notice", Text: "Turn cancelled."})
		}
		var unsent []Input
		if status != "ok" || c.quitPending || c.closing {
			unsent = c.a.Queue().RemoveComposer()
		}
		c.endLocked(op)
		final := ""
		if status == "ok" {
			final = c.a.FinalText()
		}
		c.emit(Event{Kind: "done", Status: status, Error: text, Unsent: unsent, Text: final, Title: c.titleLocked(), Context: c.a.ContextTokens()})
		c.settleLocked()
	})
}

func (c *Controller) settleLocked() {
	if c.reload {
		c.reload = false
		c.reloadLocked()
	}
	if c.quitPending && !c.closing {
		c.quitPending = false
		if err := c.closeLocked(); err != nil {
			c.emit(Event{Kind: "error", Text: err.Error()})
		}
		return
	}
	c.deliverLocked()
	if c.active == nil {
		c.a.session().checkIdle()
	}
}

// switchLocked changes the session. Callers guarantee that no operation runs.
func (c *Controller) switchLocked(kind, id string) error {
	var err error
	if kind == "resume" {
		err = c.a.Resume(id)
	} else {
		err = c.a.NewSession()
	}
	if err != nil {
		return err
	}
	session := c.sessionLocked()
	c.emit(Event{Kind: "session", Session: &session})
	return nil
}

func (c *Controller) deliverLocked() {
	if c.active != nil || c.shut || c.closing {
		return
	}
	if next, ok := c.a.Queue().PopBatch(); ok {
		c.beginTurnLocked(next)
	}
}

func isStatus(e TurnEvent) bool {
	return (e.Kind == "notice" || e.Kind == "error") && !e.Final && !IsNotice(e.Text)
}

func (c *Controller) setupError() error {
	if hint := c.a.Settings().SetupHint(); hint != "" {
		return fail(CodeSetup, "%s", hint)
	}
	return nil
}

func (c *Controller) requireIdle(msg string) error {
	if c.active != nil {
		return fail(CodeBusy, "%s", msg)
	}
	return nil
}
