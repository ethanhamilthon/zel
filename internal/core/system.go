package core

import (
	"context"
	"encoding/json"

	"zel/internal/luart"
)

// systemIntro opens every system prompt.
const systemIntro = "You are Zel, a coding agent. Use the lua tool to help the user."

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

// assemblePrompt renders the intro, the instructions and dynamic pieces of every
// init.lua layer, then the Lua API: the built-in part and the registered functions.
func (a *Runtime) assemblePrompt(ctx context.Context) string {
	if cfg := a.lua.get(); cfg != nil {
		return systemIntro + "\n\n" + cfg.SystemPrompt(ctx)
	}
	return systemIntro + "\n\n" + luart.BuiltinPrompt
}

func instructionSnapshot(item json.RawMessage) (string, bool) {
	var v struct{ Type, Text string }
	_ = json.Unmarshal(item, &v)
	return v.Text, v.Type == "instructions"
}
func (a *Runtime) requestInput() []json.RawMessage {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	input := make([]json.RawMessage, 0, len(a.History)+1)
	marked := a.compacted.Drop == 0
	for i, item := range a.History {
		if sent, _ := historyItem(item); !sent {
			continue
		}
		if !marked && i >= a.compacted.Drop {
			input = append(input, raw(map[string]any{"role": "user", "content": compactionNotice}))
			marked = true
		}
		if item, ok := a.compactedInput(i, item); ok {
			input = append(input, item)
		}
	}
	return input
}
