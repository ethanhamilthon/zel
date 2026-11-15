package core

import (
	"context"
	"errors"
	"sync"
)

// Choice is a question the agent asks the user with choice(name, options): the
// UI shows the options instead of the composer until one is picked or the
// question is cancelled.
type Choice struct {
	ID      int
	Title   string
	Options []string
}

// chooser holds the one question the UI shows at a time. Questions from
// several callers wait for their turn.
type chooser struct {
	turn    sync.Mutex // held by the caller whose question is shown
	mu      sync.Mutex
	nextID  int
	pending *Choice
	reply   chan int
	changed func() // called, outside the lock, when the pending question changes
}

func (c *chooser) notify() {
	c.mu.Lock()
	fn := c.changed
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ask shows the question and waits for the answer: the index of the picked
// option, -1 when cancelled.
func (c *chooser) ask(ctx context.Context, title string, options []string) (int, error) {
	c.mu.Lock()
	ui := c.changed != nil
	c.mu.Unlock()
	if !ui {
		return -1, errors.New("no UI to ask the user in")
	}
	c.turn.Lock()
	defer c.turn.Unlock()
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	reply := make(chan int, 1)
	c.mu.Lock()
	c.nextID++
	c.pending, c.reply = &Choice{ID: c.nextID, Title: title, Options: append([]string(nil), options...)}, reply
	c.mu.Unlock()
	c.notify()
	defer func() {
		c.mu.Lock()
		c.pending, c.reply = nil, nil
		c.mu.Unlock()
		c.notify()
	}()
	select {
	case i := <-reply:
		return i, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// answer delivers the user's pick (index) or cancel (-1) for question id; an
// answer to a question that is gone is ignored.
func (c *chooser) answer(id, index int) {
	c.mu.Lock()
	reply := c.reply
	ok := c.pending != nil && c.pending.ID == id && (index == -1 || (index >= 0 && index < len(c.pending.Options)))
	c.mu.Unlock()
	if !ok {
		return
	}
	select {
	case reply <- index:
	default:
	}
}

// PendingChoice is the question the UI should show, nil when there is none.
func (a *Runtime) PendingChoice() *Choice {
	a.ask.mu.Lock()
	defer a.ask.mu.Unlock()
	if a.ask.pending == nil {
		return nil
	}
	c := *a.ask.pending
	return &c
}

// AnswerChoice answers the question id with the option at index, or cancels it with -1.
func (a *Runtime) AnswerChoice(id, index int) { a.ask.answer(id, index) }

// OnChoiceChanged registers fn to run when the pending question appears or goes
// away; with fn set, choice() has a UI to ask in. fn must not block.
func (a *Runtime) OnChoiceChanged(fn func()) {
	a.ask.mu.Lock()
	a.ask.changed = fn
	a.ask.mu.Unlock()
}

// Choose implements luart.Env: the question goes to the UI of the main session.
func (e luaEnv) Choose(ctx context.Context, title string, options []string) (string, bool, error) {
	i, err := e.s.ask.ask(ctx, title, options)
	if err != nil || i < 0 {
		return "", false, err
	}
	return options[i], true, nil
}
