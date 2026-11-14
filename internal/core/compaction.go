package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Automatic compaction keeps History intact and append-only. A compaction item
// records two History indices; requestInput applies them to every later request:
//
//   - before Drop (the oldest zone) only user and assistant messages remain,
//     without images; tool calls, their outputs and reasoning are removed;
//   - between Drop and Prune tool outputs are cut to their first line;
//   - from Prune on (the freshest zone) everything is sent unchanged.
//
// Boundaries only move forward, so earlier requests' prefixes stay stable.
type compaction struct {
	Prune int `json:"prune_before"`
	Drop  int `json:"drop_before"`
}

const (
	compactionType   = "compaction"
	prunedMarker     = "\n[tool output pruned]"
	prunedLineLimit  = 200
	compactionNotice = "<context-compacted>\nEarlier parts of this conversation were compacted to save context: " +
		"tool calls, their outputs and reasoning before this point were removed; only user and assistant messages remain.\n</context-compacted>"
)

func compactionSnapshot(item json.RawMessage) (compaction, bool) {
	var v struct {
		Type string
		compaction
	}
	if json.Unmarshal(item, &v) != nil || v.Type != compactionType {
		return compaction{}, false
	}
	return v.compaction, true
}

// historyItem reports whether item is sent to the model and whether it starts a user turn.
func historyItem(item json.RawMessage) (sent, user bool) {
	var v struct{ Type, Role string }
	_ = json.Unmarshal(item, &v)
	if v.Type == "instructions" || v.Type == compactionType || v.Type == turnEndType {
		return false, false
	}
	return true, v.Role == "user"
}

// compactionBounds measures History from the end: the latest quarter of the
// threshold stays fresh, the next quarter keeps its tools but prunes outputs,
// and everything older keeps only the conversation. The oldest zone always ends
// at a user message, so tool calls and their outputs are removed together.
func compactionBounds(history []json.RawMessage, threshold int64, prev compaction) (compaction, bool) {
	fresh, middle := threshold/4, threshold/2
	next := prev
	var total int64
	pruneSet := false
	for i := len(history) - 1; i >= 0; i-- {
		sent, user := historyItem(history[i])
		if !sent {
			continue
		}
		total += estimateTokens(history[i : i+1])
		if !pruneSet && total >= fresh {
			next.Prune, pruneSet = max(prev.Prune, i), true
		}
		if user && total >= middle {
			next.Drop = max(prev.Drop, i)
			break
		}
	}
	return next, next != prev
}

func (s *session) compactedInput(i int, item json.RawMessage) (json.RawMessage, bool) {
	c := s.compacted
	if i < c.Drop {
		return conversationOnly(item)
	}
	if i < c.Prune {
		return pruneOutput(item), true
	}
	return item, true
}

// conversationOnly keeps a user or assistant message without images or item IDs:
// a message ID would refer to the reasoning item removed before it.
func conversationOnly(item json.RawMessage) (json.RawMessage, bool) {
	var v map[string]any
	if json.Unmarshal(item, &v) != nil {
		return nil, false
	}
	role, _ := v["role"].(string)
	if role != "user" && role != "assistant" {
		return nil, false
	}
	delete(v, "id")
	if parts, ok := v["content"].([]any); ok {
		var kept []any
		for _, part := range parts {
			if p, ok := part.(map[string]any); ok && p["type"] == "input_image" {
				continue
			}
			kept = append(kept, part)
		}
		if len(kept) == 0 {
			return nil, false
		}
		v["content"] = kept
	}
	return raw(v), true
}

// pruneOutput cuts a lua call result to its first line.
func pruneOutput(item json.RawMessage) json.RawMessage {
	var v map[string]any
	if json.Unmarshal(item, &v) != nil || v["type"] != "function_call_output" {
		return item
	}
	output, _ := v["output"].(string)
	first, _, multiline := strings.Cut(output, "\n")
	if runes := []rune(first); len(runes) > prunedLineLimit {
		first, multiline = string(runes[:prunedLineLimit]), true
	}
	if !multiline {
		return item
	}
	v["output"] = first + prunedMarker
	return raw(v)
}

// compact runs between model steps, once all tool outputs of a step are saved.
func (s *session) compact(emit func(TurnEvent)) error {
	threshold := int64(s.Settings().CompactionThreshold)
	if threshold <= 0 || s.ContextTokens() < threshold {
		return nil
	}
	s.histMu.RLock()
	next, changed := compactionBounds(s.History, threshold, s.compacted)
	s.histMu.RUnlock()
	if !changed {
		return nil
	}
	item := raw(map[string]any{"type": compactionType, "prune_before": next.Prune, "drop_before": next.Drop})
	s.histMu.Lock()
	s.History = append(s.History, item)
	seq := len(s.History) - 1
	s.compacted = next
	// Recount from the compacted request instead of the last provider usage.
	s.lastRequest.ContextInput = nil
	s.histMu.Unlock()
	s.setItemMeta(seq, Metadata{At: time.Now()})
	for _, r := range RecordsForItem(item) {
		emit(TurnEvent{Kind: r.Kind, Text: r.Text, Final: true})
	}
	emit(TurnEvent{Kind: "notice", Text: fmt.Sprintf("Context compacted at %s tokens; now about %s.", formatTokenCount(threshold), formatTokenCount(s.ContextTokens()))})
	return s.saveHistory()
}

func formatTokenCount(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}
