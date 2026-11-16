package core

import (
	"slices"
	"sync"
	"time"
)

// activity is one foreground host operation of a session that is in progress.
type activity struct {
	id    uint64
	label string
	at    time.Time // when it began
}

// activities holds the foreground operations of one session. They overlap, so each
// one removes only its own entry, and the indicator shows the newest still running.
type activities struct {
	mu   sync.Mutex
	seq  uint64
	list []activity
	last string // label of the newest operation, kept after it ends until a new turn
}

func (a *Runtime) beginActivity(label string) (end func()) {
	return a.session().beginActivity(label)
}

// beginActivity records a foreground operation; the returned function ends it
// safely and at most once, even under concurrent abandonment and late host return.
func (s *session) beginActivity(label string) (end func()) {
	acts := s.acts
	if acts == nil { // a session built without a table shows no activity
		return func() {}
	}
	acts.mu.Lock()
	acts.seq++
	id := acts.seq
	acts.list = append(acts.list, activity{id: id, label: label, at: time.Now()})
	acts.last = label
	acts.mu.Unlock()
	s.publishActivity()
	var once sync.Once
	return func() {
		once.Do(func() {
			acts.mu.Lock()
			acts.list = slices.DeleteFunc(acts.list, func(x activity) bool { return x.id == id })
			acts.mu.Unlock()
			s.publishActivity()
		})
	}
}

// Activity is the label of the operation the session waits on, or "" when none runs.
func (a *Runtime) Activity() string { return a.session().Activity() }

func (s *session) Activity() string {
	label, _ := s.liveActivity()
	return label
}

// LiveActivity is the operation the session waits on and when it began; "" when none runs.
func (a *Runtime) LiveActivity() (string, time.Time) { return a.session().liveActivity() }

func (s *session) liveActivity() (string, time.Time) {
	acts := s.acts
	if acts == nil {
		return "", time.Time{}
	}
	acts.mu.Lock()
	defer acts.mu.Unlock()
	if n := len(acts.list); n > 0 {
		return acts.list[n-1].label, acts.list[n-1].at
	}
	return "", time.Time{}
}

// LastActivity is the label of the newest foreground operation of the turn, also
// after it ended; "" until the turn does its first one.
func (a *Runtime) LastActivity() string { return a.session().lastActivity() }

func (s *session) lastActivity() string {
	if s.acts == nil {
		return ""
	}
	s.acts.mu.Lock()
	defer s.acts.mu.Unlock()
	return s.acts.last
}

// ResetActivity forgets the last operation when a new turn starts.
func (a *Runtime) ResetActivity() {
	if acts := a.session().acts; acts != nil {
		acts.mu.Lock()
		acts.last = ""
		acts.mu.Unlock()
	}
}

// publishActivity is a no-op: the working indicator reads Activity.
func (s *session) publishActivity() {}
