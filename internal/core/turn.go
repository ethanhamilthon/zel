package core

import (
	"context"
	"encoding/json"
	"time"
	"zel/internal/entities"
)

type turnHost struct {
	s        *session
	emit     func(TurnEvent)
	accept   func() bool
	turnSeq  int
	injected int
	skipBash bool // discard the rest of this response after typed steering
}

func (h *turnHost) AcceptSteering() bool { return h.accept() }

// maxTurnInjections bounds how often turn_end hooks may keep a turn going.
const maxTurnInjections = 3

// TurnEnd runs the turn_end hooks; a returned text continues the turn as a user message.
func (h *turnHost) TurnEnd(ctx context.Context) bool {
	if h.injected >= maxTurnInjections || ctx.Err() != nil {
		return false
	}
	text := h.s.turnEnd(ctx)
	if text == "" {
		return false
	}
	h.injected++
	h.s.addInput(notification(text), h.emit)
	return true
}
func (h *turnHost) Save() error    { return h.s.saveHistory() }
func (h *turnHost) Compact() error { return h.s.compact(h.emit) }
func (h *turnHost) Request(ctx context.Context) ([]json.RawMessage, error) {
	return h.s.request(ctx, h.emit)
}
func (h *turnHost) CommitResponse(items []json.RawMessage) error {
	h.skipBash = false
	s, emit := h.s, h.emit
	s.histMu.RLock()
	meta := s.lastRequest
	meta.RequestSeq = len(s.History) + 1
	s.histMu.RUnlock()
	meta.TurnSeq = h.turnSeq
	meta.At = time.Now()
	for i, item := range items {
		s.histMu.Lock()
		s.History = append(s.History, item)
		seq := len(s.History) - 1
		s.histMu.Unlock()
		s.setItemMeta(seq, meta)
		for _, r := range RecordsForItem(item) {
			m := meta
			if r.Kind == "assistant" {
				s.histMu.RLock()
				m = s.answerMetadata(seq)
				s.histMu.RUnlock()
			}
			emit(TurnEvent{Kind: r.Kind, Text: r.Text, ID: s.itemID(int64(i)), Meta: &m, Final: true})
		}
	}
	// The committed items are history now; their streamed steps must not count twice.
	s.histMu.Lock()
	s.liveSteps = nil
	s.histMu.Unlock()
	if e := s.saveHistory(); e != nil {
		return e
	}
	return nil
}
func (h *turnHost) Execute(ctx context.Context, call entities.ToolCall) error {
	s, emit := h.s, h.emit
	started := time.Now()
	h.skipBash = h.skipBash || s.queue.hasTyped()
	output0 := "[skipped: superseded by steering]"
	if call.Name != "bash" || !h.skipBash {
		output0 = s.runTool(ctx, call.CallID, call.Name, call.Arguments, emit)
	}
	h.skipBash = h.skipBash || s.queue.hasTyped()
	output := callOutput(call.CallID, output0)
	s.histMu.Lock()
	s.History = append(s.History, output)
	seq := len(s.History) - 1
	s.histMu.Unlock()
	m := Metadata{Duration: time.Since(started), At: time.Now()}
	s.setItemMeta(seq, m)
	for _, r := range RecordsForItem(output) {
		emit(TurnEvent{Kind: r.Kind, Text: r.Text, Meta: &m, Final: true})
	}
	if e := s.saveHistory(); e != nil {
		return e
	}
	return nil
}
