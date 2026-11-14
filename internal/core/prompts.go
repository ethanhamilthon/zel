package core

import (
	"context"
	"time"

	"zel/internal/luart"
)

// promptTimeout bounds one expansion; a completion waits for the config lock like any plugin call.
const promptTimeout = 10 * time.Second

// PromptAt finds the #name token at the cursor; see luart.PromptAt.
func PromptAt(runes []rune, cursor int) (start, end int, query string, ok bool) {
	return luart.PromptAt(runes, cursor)
}

// promptSnapshot is the current init.lua snapshot, nil when none is loaded.
func (c *Controller) promptSnapshot() (cfg *luart.Config) {
	c.view(func() { cfg = c.a.session().lua.get() })
	return cfg
}

// PromptNames lists the names registered with zel.complete in the current snapshot.
func (c *Controller) PromptNames() []string {
	if cfg := c.promptSnapshot(); cfg != nil {
		return cfg.PromptNames()
	}
	return nil
}

// HasPromptTokens reports whether text names a registered completion; only then does sending run callbacks.
func (c *Controller) HasPromptTokens(text string) bool {
	cfg := c.promptSnapshot()
	for _, tok := range luart.PromptTokens(text) {
		if cfg != nil && cfg.HasPrompt(tok.Name) {
			return true
		}
	}
	return false
}

// ExpandPrompt expands the registered names of text outside the controller lock.
// ctx is the caller's: when it ends first, the expansion stops and no text is
// returned. The caller keeps its own text on any error.
func (c *Controller) ExpandPrompt(ctx context.Context, text string) (string, error) {
	return expandPrompt(ctx, c.promptSnapshot(), text)
}

// ExpandPrompt is the headless variant, for prompts that run outside a controller.
func (a *Runtime) ExpandPrompt(ctx context.Context, text string) (string, error) {
	return expandPrompt(ctx, a.session().lua.get(), text)
}

// expandPrompt runs the completions of one snapshot within promptTimeout, and no
// longer than parent lasts.
func expandPrompt(parent context.Context, cfg *luart.Config, text string) (string, error) {
	if parent.Err() != nil {
		return "", fail(CodeFailed, "the prompt expansion was cancelled")
	}
	if cfg == nil {
		return text, nil
	}
	ctx, cancel := context.WithTimeout(parent, promptTimeout)
	defer cancel()
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, err := cfg.Expand(ctx, text)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if ctx.Err() == nil {
			return r.text, r.err
		}
	case <-ctx.Done():
	}
	// Once the expansion ended by cancellation or timeout, its text is never used.
	if parent.Err() != nil {
		return "", fail(CodeFailed, "the prompt expansion was cancelled")
	}
	return "", fail(CodeFailed, "the prompt completion timed out after %s", promptTimeout)
}
