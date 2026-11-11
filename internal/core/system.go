package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const systemBlockTimeout = 30 * time.Second

// instructions returns the session's system prompt. It is assembled once and stored
// in the history, so a resumed session keeps its prompt until Reload or New session.
func (a *Runtime) instructions(ctx context.Context) (string, error) {
	if a.promptFrozen {
		return a.prompt, nil
	}
	prompt := a.assemblePrompt(ctx)
	a.histMu.Lock()
	a.History = append(a.History, raw(map[string]string{"type": "instructions", "text": prompt}))
	a.histMu.Unlock()
	if err := a.saveHistory(); err != nil {
		a.histMu.Lock()
		a.History = a.History[:len(a.History)-1]
		a.histMu.Unlock()
		return "", err
	}
	a.prompt, a.promptFrozen = prompt, true
	return prompt, nil
}

// assemblePrompt joins project Markdown bodies in layer order.
func (a *Runtime) assemblePrompt(_ context.Context) string {
	var parts []string
	for _, b := range a.Settings().System {
		if strings.TrimSpace(b.Value) != "" {
			parts = append(parts, strings.TrimRight(b.Value, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}

func instructionSnapshot(item json.RawMessage) (string, bool) {
	var v struct{ Type, Text string }
	_ = json.Unmarshal(item, &v)
	return v.Text, v.Type == "instructions"
}

func (a *Runtime) requestInput() []json.RawMessage {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	input := make([]json.RawMessage, 0, len(a.History))
	for _, item := range a.History {
		if _, snapshot := instructionSnapshot(item); !snapshot {
			input = append(input, item)
		}
	}
	return input
}
