package core

import (
	"context"
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
)

// systemIntro opens every system prompt.
const systemIntro = "You are Zel, a coding agent. Use the bash tool to help the user."

//go:embed bash.md
var bashMD string

// BashToolDescription is the description of the one tool the model gets.
const BashToolDescription = "Run a bash script: its input is the raw script. Output is stdout and stderr together."

// instructions returns the session's system prompt. It is assembled once and stored
// in the history, so a resumed session keeps its prompt until the config changes.
func (s *session) instructions(ctx context.Context) (string, error) {
	if s.promptFrozen {
		return s.prompt, nil
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
	s.prompt, s.promptFrozen = prompt, true
	return prompt, nil
}

// assemblePrompt renders the intro, the instructions of the config for this
// session mode, then the built-in part about bash and the zel commands.
func (s *session) assemblePrompt() string {
	parts := []string{systemIntro}
	if cfg := s.conf.get(); cfg != nil {
		for _, in := range cfg.Instructions {
			if (s.mode() == "main" && in.Main) || (s.mode() == "headless" && in.Headless) {
				parts = append(parts, strings.TrimSpace(in.Text))
			}
		}
	}
	return strings.Join(append(parts, s.builtin()), "\n\n")
}

// builtin is bash.md for this session: headless sessions cannot ask the user,
// and the deepest children cannot start more of them.
func (s *session) builtin() string {
	timeout := 30
	if s.cfg != nil {
		timeout = s.Settings().BashTimeout
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(bashMD), "\n") {
		switch {
		case strings.HasPrefix(line, "[main] "):
			if s.mode() != "main" {
				continue
			}
			line = strings.TrimPrefix(line, "[main] ")
		case strings.HasPrefix(line, "[nest] "):
			if s.depth >= MaxDepth {
				continue
			}
			line = strings.TrimPrefix(line, "[nest] ")
		}
		lines = append(lines, strings.ReplaceAll(line, "{timeout}", strconv.Itoa(timeout)))
	}
	text := strings.Join(lines, "\n")
	if s.mode() == "headless" {
		text += "\nThis session is headless (zel -p): there is no user to ask, zel choice is not available."
	}
	return text
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
