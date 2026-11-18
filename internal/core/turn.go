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
	skipCalls bool // discard the rest of this response after typed steering
}

// run is the model/tool loop of one turn: steering is accepted only at safe
// boundaries, a response is committed before its tools execute, and
// cancellation is checked before every request and tool call.
func (h *turnHost) run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.accept()
		if err := h.Save(); err != nil {
			return err
		}
		items, err := h.Request(ctx)
		if err != nil {
			return err
		}
		if err := h.CommitResponse(items); err != nil {
			return err
		}
		calls := 0
		for _, item := range items {
			var call entities.ToolCall
			if err := json.Unmarshal(item, &call); err != nil {
				return err
			}
			if call.Type != "function_call" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			calls++
			if err := h.Execute(ctx, call); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if calls > 0 {
			if err := h.Compact(); err != nil {
				return err
			}
		}
		if steered := h.accept(); calls == 0 && !steered {
			if h.TurnEnd(ctx) {
				continue // a turn_end hook added a message
			}
			return nil
		}
	}
}

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
	h.skipCalls = false
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
	s.reportUsage()
	return nil
}
func (h *turnHost) Execute(ctx context.Context, call entities.ToolCall) error {
	s, emit := h.s, h.emit
	started := time.Now()
	h.skipCalls = h.skipCalls || s.queue.hasTyped()
	output0 := "[skipped: superseded by steering]"
	if call.Name != "lua" || !h.skipCalls {
		output0 = s.runTool(ctx, call.CallID, call.Name, call.Arguments, emit)
	}
	h.skipCalls = h.skipCalls || s.queue.hasTyped()
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
	s.reportUsage()
	return nil
}
