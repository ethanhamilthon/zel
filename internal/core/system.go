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
func (s *session) instructions(ctx context.Context) (string, error) {
	if s.promptFrozen {
		return s.prompt, nil
	}
	prompt := s.assemblePrompt(ctx)
	s.histMu.Lock()
	s.History = append(s.History, raw(map[string]string{"type": "instructions", "text": prompt}))
	s.histMu.Unlock()
	if err := s.saveHistory(); err != nil {
		s.histMu.Lock()
		s.History = s.History[:len(s.History)-1]
		s.histMu.Unlock()
		return "", err
	}
	s.prompt, s.promptFrozen = prompt, true
	return prompt, nil
}

// assemblePrompt renders the intro, the instructions and dynamic pieces of every
// init.lua layer, then the Lua API: the built-in part and the registered functions.
func (s *session) assemblePrompt(ctx context.Context) string {
	if cfg := s.lua.get(); cfg != nil {
		return systemIntro + "\n\n" + cfg.SystemPrompt(ctx)
	}
	return systemIntro + "\n\n" + luart.Builtin(s.depth)
}

func instructionSnapshot(item json.RawMessage) (string, bool) {
	var v struct{ Type, Text string }
	_ = json.Unmarshal(item, &v)
	return v.Text, v.Type == "instructions"
}
func (s *session) requestInput() []json.RawMessage {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	return s.requestInputLocked()
}

func (s *session) requestInputLocked() []json.RawMessage {
	input := make([]json.RawMessage, 0, len(s.History)+1)
	marked := s.compacted.Drop == 0
	for i, item := range s.History {
		if sent, _ := historyItem(item); !sent {
			continue
		}
		if !marked && i >= s.compacted.Drop {
			input = append(input, raw(map[string]any{"role": "user", "content": compactionNotice}))
			marked = true
		}
		if item, ok := s.compactedInput(i, item); ok {
			input = append(input, item)
		}
	}
	return input
}
