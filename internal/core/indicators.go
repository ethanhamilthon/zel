package core

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// Indicator is an independently timed status line owned by a running Zel.
type Indicator struct {
	ID, Text string
	Started  time.Time
}

type indicatorState struct {
	mu         sync.Mutex
	next       uint64
	lines      []Indicator
	callActive bool
	callText   string
}

func (c *Controller) Indicators() []Indicator {
	state := &c.a.indicators
	state.mu.Lock()
	defer state.mu.Unlock()
	return slices.Clone(state.lines)
}

func (c *Controller) CallStatus() string {
	state := &c.a.indicators
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.callText
}

// beginCallStatus marks a Lua call as running, so `zel status` is accepted.
// The status text outlives the call: it stays until the next status arrives
// or the session changes.
func (s *session) beginCallStatus() func() {
	state := s.indicators
	state.mu.Lock()
	state.callActive = true
	state.mu.Unlock()
	return func() {
		state.mu.Lock()
		state.callActive = false
		state.mu.Unlock()
	}
}

// clearCall forgets the status text, as a new or resumed session starts without one.
func (state *indicatorState) clearCall() {
	state.mu.Lock()
	state.callText = ""
	state.mu.Unlock()
}

func (state *indicatorState) setCall(text string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.callActive {
		return false
	}
	state.callText = text
	return true
}

func (state *indicatorState) add(text string) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.next++
	id := fmt.Sprintf("i%d", state.next)
	state.lines = append(state.lines, Indicator{ID: id, Text: text, Started: time.Now()})
	return id
}

func (state *indicatorState) update(id, text string, done bool) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	for i := range state.lines {
		if state.lines[i].ID == id {
			if done {
				state.lines = slices.Delete(state.lines, i, i+1)
			} else {
				state.lines[i].Text = text
			}
			return true
		}
	}
	return false
}
