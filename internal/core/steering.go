package core

import (
	"slices"
	"strings"
	"sync"
	"time"

	"zel/internal/shell"
)

func (s *session) addInput(input Input, emit func(TurnEvent)) {
	s.commitInput(input)
	e := inputEvent(input)
	s.histMu.RLock()
	e.Meta = s.itemMeta[len(s.History)-1]
	s.histMu.RUnlock()
	emit(e)
}

func (s *session) commitInput(input Input) {
	if input.SentAt.IsZero() {
		input.SentAt = time.Now()
	}
	defer func() {
		s.histMu.RLock()
		seq := len(s.History) - 1
		s.histMu.RUnlock()
		s.setItemMeta(seq, Metadata{At: input.SentAt})
	}()
	text := input.Display()
	if len(input.Images) == 0 {
		s.message("user", text)
		return
	}
	parts := []any{map[string]any{"type": "input_text", "text": text}}
	for _, img := range input.Images {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": img.URL, "detail": "auto"})
	}
	s.histMu.Lock()
	s.History = append(s.History, raw(map[string]any{"role": "user", "content": parts}))
	s.histMu.Unlock()
}

func inputEvent(input Input) TurnEvent {
	kind := "user"
	if input.Notice || IsNotice(input.Text) {
		kind = "notice"
	}
	return TurnEvent{Kind: kind, Text: input.Display()}
}

// notification is the one format of everything that wakes the agent besides
// the user: finished agents, hook output, answers of choices and `zel steer`
// text. The text stays short; the agent fetches details itself.
func notification(text string) Input {
	return Input{Text: "<notification>" + strings.TrimSpace(text) + "</notification>", Notice: true}
}

func withImages(input Input, images ...shell.Image) Input {
	input.Images = append(input.Images, images...)
	return input
}

type Steering struct {
	mu      sync.Mutex
	pending []Input
	wake    func()
	typed   chan struct{}
}

func (s *Steering) typedSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.typed == nil {
		s.typed = make(chan struct{}, 1)
	}
	return s.typed
}

func (s *Steering) drainTyped() {
	select {
	case <-s.typedSignal():
	default:
	}
}

func (s *Steering) hasTyped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.pending, func(input Input) bool { return !input.Notice })
}

func (s *Steering) OnPush(wake func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wake = wake
}

func (s *Steering) wakeHandler() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

const maxQueued = 64

func (a *Runtime) Queue() *Steering { return a.session().queue }

// PushNotice queues a notification, bypassing the limit of user inputs.
func (s *Steering) PushNotice(input Input) {
	input.SentAt = time.Now()
	s.mu.Lock()
	s.pending = append(s.pending, input)
	wake := s.wake
	s.mu.Unlock()
	if wake != nil {
		wake()
	}
}

func (s *Steering) Push(input Input) bool {
	input.SentAt = time.Now()
	s.mu.Lock()
	if len(s.pending) >= 2*maxQueued || (!input.Notice && len(s.pending)-s.noticeCountLocked() >= maxQueued) {
		s.mu.Unlock()
		return false
	}
	s.pending = append(s.pending, input)
	wake := s.wake
	if s.typed != nil && !input.Notice {
		select {
		case s.typed <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	if wake != nil {
		wake()
	}
	return true
}

func (s *Steering) Take() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = nil
	return pending
}

// batch merges runs of consecutive notifications into one input, keeping
// their order relative to typed inputs.
func batch(inputs []Input) []Input {
	var out []Input
	for _, in := range inputs {
		if n := len(out); n > 0 && in.Notice && out[n-1].Notice {
			out[n-1].Text += "\n" + in.Text
			out[n-1].Images = append(slices.Clip(out[n-1].Images), in.Images...)
			continue
		}
		out = append(out, in)
	}
	return out
}

// PopBatch takes the next input to start a turn with; several notifications
// in a row arrive as one.
func (s *Steering) PopBatch() (Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return Input{}, false
	}
	n := 1
	for s.pending[0].Notice && n < len(s.pending) && s.pending[n].Notice {
		n++
	}
	next := batch(s.pending[:n])[0]
	s.pending = s.pending[n:]
	return next, true
}

func (s *Steering) RemoveComposer() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	var composer []Input
	s.pending = slices.DeleteFunc(s.pending, func(input Input) bool {
		if !input.Notice {
			composer = append(composer, input)
		}
		return !input.Notice
	})
	return composer
}

func (s *Steering) Recall() (Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.pending) - 1; i >= 0; i-- {
		if input := s.pending[i]; !input.Notice {
			s.pending = slices.Delete(s.pending, i, i+1)
			return input, true
		}
	}
	return Input{}, false
}

func (s *Steering) Pending() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.pending), func(input Input) bool { return input.Notice })
}

// Withdraw removes only the identified composer input, never notices or other clients' messages.
func (s *Steering) Withdraw(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, input := range s.pending {
		if id != 0 && input.QueueID == id && !input.Notice {
			s.pending = slices.Delete(s.pending, i, i+1)
			return true
		}
	}
	return false
}

func (s *Steering) noticeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noticeCountLocked()
}

func (s *Steering) noticeCountLocked() int {
	n := 0
	for _, input := range s.pending {
		if input.Notice {
			n++
		}
	}
	return n
}

// RemoveNotice drops a queued notice with exactly this text, if it is still queued.
func (s *Steering) RemoveNotice(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, in := range s.pending {
		if in.Notice && in.Text == text {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return true
		}
	}
	return false
}
