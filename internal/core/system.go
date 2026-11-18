package core

import (
	"context"
	"encoding/json"
	"strings"

	"zel/internal/config"
	"zel/internal/luart"
)

// instructions returns the session's system prompt. It is assembled once and stored
// in the history, so a resumed session keeps its prompt until the config changes.
func (s *session) instructions(ctx context.Context) (string, error) {
	s.histMu.RLock()
	frozen, previous := s.promptFrozen, s.prompt
	s.histMu.RUnlock()
	if frozen {
		return previous, nil
	}
	prompt := s.assemblePrompt()
	s.histMu.Lock()
	s.History = append(s.History, raw(map[string]string{"type": "instructions", "text": prompt}))
	s.histMu.Unlock()
	if err := s.saveHistory(); err != nil {
		s.histMu.Lock()
		s.History = s.History[:len(s.History)-1]
		s.histMu.Unlock()
		return "", err
	}
	s.histMu.Lock()
	s.prompt, s.promptFrozen = prompt, true
	s.histMu.Unlock()
	return prompt, nil
}

// assemblePrompt renders the system prompt: only the instructions the user wrote
// in the config for this session mode. Zel adds nothing of its own here.
// It is frozen in the history; the built-in text lives in toolDescription.
func (s *session) assemblePrompt() string {
	var parts []string
	if cfg := s.conf.get(); cfg != nil {
		for _, sys := range cfg.Systems {
			if t := strings.TrimSpace(sys.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolDescription is everything Zel itself tells the model: how the lua tool
// and its API work, and the functions of the config. It is built live for
// every request; zel -p children at the deepest level cannot start more.
func (s *session) toolDescription() string {
	t := luart.ToolText{Headless: s.mode() == "headless", Nest: s.depth < MaxDepth, Timeout: config.Timeout}
	if s.cfg != nil {
		t.Timeout = s.Settings().Timeout
	}
	if cfg := s.conf.get(); cfg != nil {
		t.Functions = cfg.Functions
	}
	return luart.ToolDescription(t)
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
	for i := range s.History {
		if !s.projection(i).sent {
			continue
		}
		if !marked && i >= s.compacted.Drop {
			input = append(input, compactionNoticeItem)
			marked = true
		}
		if item, ok, _ := s.compactedProjection(i); ok {
			input = append(input, item)
		}
	}
	return input
}
