package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"zel/internal/provider"
)

// Stream handling lives at the provider boundary; only complete tool calls
// become display records. Partial arguments belong to the provider adapter.
func (s *session) request(ctx context.Context, emit func(TurnEvent)) (items []json.RawMessage, err error) {
	settings, client := s.snapshot()
	if hint := settings.SetupHint(); hint != "" {
		return nil, errors.New(hint)
	}
	prompt, err := s.instructions(ctx)
	if err != nil {
		return nil, err
	}
	s.histMu.RLock()
	for i := range s.History {
		if !s.projection(i).valid {
			s.histMu.RUnlock()
			return nil, fmt.Errorf("invalid history item %d", i)
		}
	}
	s.histMu.RUnlock()
	// The system prompt is only what the user wrote in the config; our own text goes into the bash tool description.
	request := provider.Request{Model: settings.Model, Instructions: prompt, Input: s.requestInput(), Effort: settings.Effort,
		Tools: []provider.Tool{{Name: "bash", Description: s.toolDescription(), Parameters: bashParameters(), Freeform: "command"}}}
	started := time.Now()
	s.histMu.Lock()
	s.lastRequest = Metadata{RequestID: fmt.Sprintf("r-%d", len(s.History))}
	s.requestedLen = len(s.History)
	s.histMu.Unlock()
	s.slots.sent()
	emit = s.noteSteps(emit)
	var completed []json.RawMessage

	err = client.Stream(ctx, request, func(event provider.StreamEvent) error {
		switch event.Type {
		case "retry":
			emit(TurnEvent{Kind: "notice", Text: event.Message})
		case "response.output_item.added", "response.output_item.done":
			var item struct{ Type string }
			_ = json.Unmarshal(event.Item, &item)
			if item.Type == "function_call" {
				// Count started calls even if the stream fails, but never render
				// incomplete arguments. CommitResponse publishes complete calls.
				s.histMu.Lock()
				if s.liveSteps == nil {
					s.liveSteps = map[string]bool{}
				}
				s.liveSteps[s.itemID(event.OutputIndex)] = true
				s.histMu.Unlock()
				return nil
			}
			if event.Type == "response.output_item.added" {
				for _, record := range RecordsForItem(event.Item) {
					emit(TurnEvent{Kind: record.Kind, Text: record.Text, ID: s.itemID(event.OutputIndex), Final: true, Provisional: true})
				}
			}
		case "response.output_text.delta":
			emit(TurnEvent{Kind: "delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(TurnEvent{Kind: "reasoning_delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.refusal.delta":
			emit(TurnEvent{Kind: "refusal_delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.completed":
			s.histMu.Lock()
			s.lastRequest.Duration = time.Since(started)
			if u := event.Response.Usage; u != nil {
				n := u.InputTokens
				s.lastRequest.ContextInput = &n
				cached := int64(-1)
				if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
					cached = *u.InputTokensDetails.CachedTokens
				}
				s.lastRequest.Usage = &Usage{Input: n, Output: u.OutputTokens, Cached: cached}
			}
			s.histMu.Unlock()
			completed = append([]json.RawMessage{}, event.Response.Output...)
			return provider.ErrStreamDone
		case "response.failed", "response.incomplete", "error":
			return provider.StreamFailure(event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, provider.ErrIncompleteStream
	}
	return completed, nil
}
