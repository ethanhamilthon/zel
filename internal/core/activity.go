package core

import (
	"slices"
	"sync"
)

// activity is one foreground host operation of a session that is in progress.
type activity struct {
	id    uint64
	label string
}

// activities holds the foreground host operations of one session. They can
// overlap (a plugin function calls host functions, two lua calls run together),
// so each operation removes only its own entry when it ends, and the indicator
// shows the most recently started one still running.
type activities struct {
	mu   sync.Mutex
	seq  uint64
	list []activity
}

// beginActivity records a foreground operation and returns the function that
// ends it. Ending twice is harmless.
func (a *Runtime) beginActivity(label string) (end func()) {
	acts := a.acts
	if acts == nil { // a runtime built without a table shows no activity
		return func() {}
	}
	acts.mu.Lock()
	acts.seq++
	id := acts.seq
	acts.list = append(acts.list, activity{id: id, label: label})
	acts.mu.Unlock()
	a.publishActivity()
	var once sync.Once
	return func() {
		once.Do(func() {
			acts.mu.Lock()
			acts.list = slices.DeleteFunc(acts.list, func(x activity) bool { return x.id == id })
			acts.mu.Unlock()
			a.publishActivity()
		})
	}
}

// Activity is the label of the operation the session is waiting on, or "" when
// no foreground operation runs. Background tasks never show here.
func (a *Runtime) Activity() string {
	acts := a.acts
	if acts == nil {
		return ""
	}
	acts.mu.Lock()
	defer acts.mu.Unlock()
	if n := len(acts.list); n > 0 {
		return acts.list[n-1].label
	}
	return ""
}

// publishActivity gives a subagent's current activity to its row in the agent
// tree. The main session has no row: the working indicator reads Activity.
func (a *Runtime) publishActivity() {
	if link := a.link; link != nil {
		link.tree.setActivity(link.id, a.Activity())
	}
}
