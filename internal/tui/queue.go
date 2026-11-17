package tui

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"zel/internal/core"
)

// eventQueue hands controller events to one consumer. Push never blocks, because the controller
// calls it under its lock.
type eventQueue struct {
	mu            sync.Mutex
	items         []core.Event
	wake          chan struct{}
	spill         *os.File
	read, written int64
	failure       error
	stopped       bool
}

func newEventQueue() *eventQueue {
	return &eventQueue{wake: make(chan struct{}, 1)}
}

func (q *eventQueue) Push(item core.Event) {
	q.mu.Lock()
	if !q.stopped {
		// Adjacent deltas share one bounded chunk; final events are never coalesced.
		n := len(q.items)
		if q.read == q.written && n > 0 && mergeEvent(&q.items[n-1], item) {
		} else if q.read == q.written && n < 256 {
			q.items = append(q.items, item)
		} else if err := q.spillEvent(item); err != nil {
			q.failure, q.stopped = fmt.Errorf("UI event spool failed: %w", err), true
		}
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) TryPop() (item core.Event, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) > 0 {
		item = q.items[0]
		q.items[0] = core.Event{}
		q.items = q.items[1:]
		return item, true
	}
	if q.read < q.written {
		var size [8]byte
		_, err := q.spill.ReadAt(size[:], q.read)
		if err == nil {
			data := make([]byte, binary.LittleEndian.Uint64(size[:]))
			_, err = q.spill.ReadAt(data, q.read+8)
			if err == nil {
				err = json.Unmarshal(data, &item)
				q.read += int64(8 + len(data))
			}
		}
		if err != nil {
			q.failure, q.stopped = fmt.Errorf("UI event spool read failed: %w", err), true
			q.read = q.written
		}
		if q.read == q.written {
			q.spill.Close()
			q.spill = nil
			q.read, q.written = 0, 0
		}
		if err == nil {
			return item, true
		}
	}
	if q.failure != nil {
		item = core.Event{Kind: "fatal", Text: q.failure.Error()}
		q.failure = nil
		return item, true
	}
	return item, false
}

// Overflow is spooled to an unlinked private file, bounding queue memory while
// retaining event order and final results without waiting for the UI consumer.
func (q *eventQueue) spillEvent(item core.Event) error {
	if q.spill == nil {
		f, err := os.CreateTemp("", "zel-events-*")
		if err != nil {
			return err
		}
		if err := os.Remove(f.Name()); err != nil {
			f.Close()
			return err
		}
		q.spill = f
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(data)))
	if _, err = q.spill.WriteAt(size[:], q.written); err != nil {
		return err
	}
	if _, err = q.spill.WriteAt(data, q.written+8); err != nil {
		return err
	}
	q.written += int64(8 + len(data))
	return nil
}

func mergeEvent(last *core.Event, next core.Event) bool {
	if last.Kind != "agent" || next.Kind != "agent" || last.Agent == nil || next.Agent == nil {
		return false
	}
	a, b := last.Agent, next.Agent
	if a.Final || b.Final || a.ID != b.ID || a.Kind != b.Kind || a.Provisional != b.Provisional || len(a.Text)+len(b.Text) > 32768 {
		return false
	}
	if a.Kind != "delta" && a.Kind != "refusal_delta" && a.Kind != "reasoning_delta" {
		return false
	}
	copy := *a
	copy.Text += b.Text
	if b.Meta != nil {
		copy.Meta = b.Meta
	}
	last.Agent = &copy
	return true
}

func (q *eventQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
	if q.spill != nil {
		q.spill.Close()
		q.spill = nil
	}
	q.items, q.read, q.written = nil, 0, 0
}
