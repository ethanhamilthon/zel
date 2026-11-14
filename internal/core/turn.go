package core

import (
	"context"
	"encoding/json"
	"time"
	"zel/internal/entities"
)

type turnHost struct {
	a        *Runtime
	emit     func(TurnEvent)
	accept   func() bool
	turnSeq  int
	injected int
}

func (h *turnHost) AcceptSteering() bool { return h.accept() }

// maxTurnInjections bounds how often turn_end hooks may keep a turn going.
const maxTurnInjections = 3

// TurnEnd runs the turn_end hooks; a returned text continues the turn as a user message.
func (h *turnHost) TurnEnd(ctx context.Context) bool {
	if h.injected >= maxTurnInjections || ctx.Err() != nil {
		return false
	}
	text := h.a.turnEnd(ctx)
	if text == "" {
		return false
	}
	h.injected++
	h.a.addInput(notification(text), h.emit)
	return true
}
func (h *turnHost) Save() error    { return h.a.saveHistory() }
func (h *turnHost) Compact() error { return h.a.compact(h.emit) }
func (h *turnHost) Request(ctx context.Context) ([]json.RawMessage, error) {
	return h.a.request(ctx, h.emit)
}
func (h *turnHost) CommitResponse(items []json.RawMessage) error {
	a, emit := h.a, h.emit
	meta := a.lastRequest
	meta.RequestSeq = len(a.History) + 1
	meta.TurnSeq = h.turnSeq
	meta.At = time.Now()
	for i, item := range items {
		a.histMu.Lock()
		a.History = append(a.History, item)
		a.histMu.Unlock()
		a.setItemMeta(len(a.History)-1, meta)
		for _, r := range RecordsForItem(item) {
			m := meta
			if r.Kind == "assistant" {
				m = a.answerMetadata(len(a.History) - 1)
			}
			emit(TurnEvent{Kind: r.Kind, Text: r.Text, ID: a.itemID(int64(i)), Meta: &m, Final: true})
		}
	}
	// The committed items are history now; their streamed steps must not count twice.
	a.liveSteps = nil
	if e := a.saveHistory(); e != nil {
		return e
	}
	return nil
}
func (h *turnHost) Execute(ctx context.Context, call entities.ToolCall) error {
	a, emit := h.a, h.emit
	started := time.Now()
	output0 := a.runTool(ctx, call.CallID, call.Name, call.Arguments, emit)
	output := callOutput(call.CallID, output0)
	a.histMu.Lock()
	a.History = append(a.History, output)
	a.histMu.Unlock()
	m := Metadata{Duration: time.Since(started), At: time.Now()}
	a.setItemMeta(len(a.History)-1, m)
	for _, r := range RecordsForItem(output) {
		emit(TurnEvent{Kind: r.Kind, Text: r.Text, Meta: &m, Final: true})
	}
	if e := a.saveHistory(); e != nil {
		return e
	}
	return nil
}
