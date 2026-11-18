package core

import (
	"context"
	_ "embed"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"zel/internal/config"
)

//go:embed prompt/zel.md
var zelPrompt string

//go:embed prompt/lua_tool.md
var luaToolPrompt string

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

// assemblePrompt renders the system prompt: the static text about Zel and its
// CLI (prompt/zel.md), the functions of the config, one line each sorted by
// name, then the systems of the configs in file order. It is frozen in the
// history until the config changes.
func (s *session) assemblePrompt() string {
	parts := []string{strings.TrimSpace(zelPrompt)}
	cfg := s.conf.get()
	if cfg == nil {
		return parts[0]
	}
	if len(cfg.Functions) > 0 {
		funcs := slices.Clone(cfg.Functions)
		slices.SortFunc(funcs, func(a, b config.Function) int { return strings.Compare(a.Name, b.Name) })
		var b strings.Builder
		b.WriteString("## Functions")
		for _, f := range funcs {
			desc := strings.TrimSpace(f.Description)
			if !strings.HasPrefix(desc, "`") {
				desc = "`" + f.Name + "` - " + desc
			}
			b.WriteString("\n" + desc)
		}
		parts = append(parts, b.String())
	}
	for _, sys := range cfg.Systems {
		if t := strings.TrimSpace(sys.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolDescription is the description of the lua tool: prompt/lua_tool.md with
// the call timeout.
func (s *session) toolDescription() string {
	timeout := config.Timeout
	if s.cfg != nil {
		timeout = s.Settings().Timeout
	}
	return strings.ReplaceAll(strings.TrimSpace(luaToolPrompt), "{timeout}", strconv.Itoa(timeout))
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
