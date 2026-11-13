package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// systemBlockTimeout bounds each ```bash block a ZEL.md runs.
const systemBlockTimeout = 30 * time.Second

// bashBlock matches a fenced ```bash block; its output replaces it in the prompt.
var bashBlock = regexp.MustCompile("(?ms)^```bash[ \t]*\n(.*?)^```[ \t]*$")

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

// assemblePrompt joins project Markdown bodies in layer order. Each ```bash block
// runs in the working directory and its output takes the block's place.
func (a *Runtime) assemblePrompt(ctx context.Context) string {
	var parts []string
	for _, b := range a.Settings().System {
		if strings.TrimSpace(b.Value) != "" {
			parts = append(parts, strings.TrimRight(a.runBashBlocks(ctx, b.Value), "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}

func (a *Runtime) runBashBlocks(ctx context.Context, body string) string {
	return bashBlock.ReplaceAllStringFunc(body, func(block string) string {
		result, _ := a.Shell.Run(ctx, bashBlock.FindStringSubmatch(block)[1], systemBlockTimeout, a.Settings().ShellOutput, nil)
		out := strings.TrimRight(result.Output, "\n")
		if result.Error && result.ExitCode != 124 && result.ExitCode != 130 {
			out += fmt.Sprintf("\n[zel: exit status %d]", result.ExitCode)
		}
		return out
	})
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
