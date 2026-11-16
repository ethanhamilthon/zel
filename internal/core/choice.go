package core

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"zel/internal/entities"
	"zel/internal/store"
)

// Choice is a question the agent asks the user with zel choice: the UI shows
// the options instead of the composer until one is picked or Esc cancels it.
// Waiting is how many questions are queued, this one included.
type Choice struct {
	ID      int
	Title   string
	Options []string
	Waiting int
}

type question struct {
	Choice
	s *session // the session that asked; its answer goes there as a notification
}

// chooser queues the questions; the UI shows the first. Asking never blocks:
// the answer arrives to the agent as <notification>c1 Title: Option</notification>.
type chooser struct {
	mu      sync.Mutex
	nextID  int
	queue   []*question
	changed func() // called, outside the lock, when the queue changes
}

func (c *chooser) notify() {
	c.mu.Lock()
	fn := c.changed
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ask queues a question of s and returns its id.
func (c *chooser) ask(s *session, title string, options []string) (int, error) {
	c.mu.Lock()
	if c.changed == nil {
		c.mu.Unlock()
		return 0, fail(CodeDenied, "EDENIED: no UI to ask the user in")
	}
	c.nextID++
	q := &question{Choice: Choice{ID: c.nextID, Title: title, Options: slices.Clone(options)}, s: s}
	c.queue = append(c.queue, q)
	c.mu.Unlock()
	c.notify()
	return q.ID, nil
}

// answer takes the user's pick (index) or cancel (-1) for question id and
// sends it to the session that asked.
func (c *chooser) answer(id, index int) (title, picked string, ok bool) {
	return c.resolve(id, index, "")
}

// resolve delivers the answer and reports the question and the answer text.
func (c *chooser) resolve(id, index int, custom string) (title, picked string, ok bool) {
	c.mu.Lock()
	i := slices.IndexFunc(c.queue, func(q *question) bool { return q.ID == id })
	if i < 0 || (custom == "" && (index < -1 || index >= len(c.queue[i].Options))) {
		c.mu.Unlock()
		return "", "", false
	}
	q := c.queue[i]
	c.queue = slices.Delete(c.queue, i, i+1)
	c.mu.Unlock()
	c.notify()
	picked = "cancelled"
	if custom != "" {
		picked = custom
	} else if index >= 0 {
		picked = q.Options[index]
	}
	if !q.s.ended.Load() {
		q.s.queue.PushNotice(notification(fmt.Sprintf("c%d %s: %s", q.ID, q.Title, picked)))
	}
	return q.Title, picked, true
}

// drop forgets the questions of a session that ends.
func (c *chooser) drop(s *session) {
	c.mu.Lock()
	n := len(c.queue)
	c.queue = slices.DeleteFunc(c.queue, func(q *question) bool { return q.s == s })
	changed := n != len(c.queue)
	c.mu.Unlock()
	if changed {
		c.notify()
	}
}

// PendingChoice is the question the UI should show, nil when there is none.
func (a *Runtime) PendingChoice() *Choice {
	a.ask.mu.Lock()
	defer a.ask.mu.Unlock()
	if len(a.ask.queue) == 0 {
		return nil
	}
	c := a.ask.queue[0].Choice
	c.Options = slices.Clone(c.Options)
	c.Waiting = len(a.ask.queue)
	return &c
}

// AnswerChoice answers the question id with the option at index, or cancels it with -1.
// It reports the chat record of the answer; ok is false when id is not pending.
func (a *Runtime) AnswerChoice(id, index int) (Record, bool) {
	return a.session().recordChoice(a.ask.answer(id, index))
}

// AnswerChoiceText delivers a non-empty custom answer without changing the options.
func (a *Runtime) AnswerChoiceText(id int, text string) (Record, bool) {
	if text = strings.TrimSpace(text); text != "" && len([]rune(text)) <= 4096 {
		return a.session().recordChoice(a.ask.resolve(id, -1, text))
	}
	return Record{}, false
}

// recordChoice keeps an answered question in the chat of s and in its session folder.
func (s *session) recordChoice(title, answer string, ok bool) (Record, bool) {
	if !ok {
		return Record{}, false
	}
	s.histMu.Lock()
	c := store.Choice{Seq: len(s.History), Title: title, Answer: answer, At: time.Now().UTC()}
	s.choices = append(s.choices, c)
	s.histMu.Unlock()
	if s.db != nil && s.id != "" {
		if err := s.db.AppendChoice(s.project, s.id, c); err != nil {
			s.LogEvent("error", "Could not save the answer: "+err.Error())
		}
	}
	return entities.ChoiceRecord(title, answer), true
}

// OnChoiceChanged registers fn to run when the queue of questions changes; with
// fn set, zel choice has a UI to ask in. fn must not block.
func (a *Runtime) OnChoiceChanged(fn func()) {
	a.ask.mu.Lock()
	a.ask.changed = fn
	a.ask.mu.Unlock()
}

// Ask queues a question of the main session, as zel choice does.
func (a *Runtime) Ask(title string, options []string) (int, error) {
	s := a.session()
	return s.ask.ask(s, title, options)
}
