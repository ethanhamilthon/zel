package core

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"zel/internal/config"
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
	timeout, limit := 30, config.BashOutput
	if s.cfg != nil {
		timeout, limit = s.Settings().BashTimeout, s.Settings().BashOutput
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
		line = strings.ReplaceAll(line, "{timeout}", strconv.Itoa(timeout))
		if strings.HasPrefix(line, "Output = ") && limit > 0 {
			line = strings.Replace(line, "never truncated", fmt.Sprintf("cut to %d bytes (head and tail; the marker gives a file with the full output)", limit), 1)
		}
		lines = append(lines, line)
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
