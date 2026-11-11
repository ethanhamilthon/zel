package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

const systemBlockTimeout = 30 * time.Second

// instructions returns the session's system prompt. It is assembled once and stored
// in the history, so a resumed session keeps its prompt until /reload or /new.
func (a *Agent) instructions(ctx context.Context) (string, error) {
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

// assemblePrompt joins the [system.*] blocks in file order: text blocks as-is,
// bash blocks as their stdout when they exit 0.
func (a *Agent) assemblePrompt(ctx context.Context) string {
	var parts []string
	for _, b := range a.Settings().System {
		text := b.Value
		if b.Type == "bash" {
			text = a.runSystemBlock(ctx, b.Value)
		}
		if strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimRight(text, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}

func (a *Agent) runSystemBlock(ctx context.Context, script string) string {
	ctx, cancel := context.WithTimeout(ctx, systemBlockTimeout)
	defer cancel()
	out, err := a.shCommand(ctx, script, map[string]string{"DASHY_MODE": a.opts.Mode}).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// shCommand runs script with sh -c in the session directory with DASHY_* variables.
func (a *Agent) shCommand(ctx context.Context, script string, extra map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = a.Shell.Dir
	cmd.Env = os.Environ()
	for k, v := range a.shellEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "DASHY_HOME="+a.home, "DASHY_CWD="+a.Shell.Dir)
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func instructionSnapshot(item json.RawMessage) (string, bool) {
	var v struct{ Type, Text string }
	_ = json.Unmarshal(item, &v)
	return v.Text, v.Type == "instructions"
}

func (a *Agent) requestInput() []json.RawMessage {
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
