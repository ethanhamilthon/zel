package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type ModelChoice struct {
	Provider, Model string
	ContextWindow   int64
}

func AllModels(ctx context.Context, providers map[string]Provider) (choices []ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []modelInfo
		err    error
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p Provider) {
			models, err := newClient(p).modelCatalog(ctx)
			if err != nil && p.key() != "" {
				err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
			}
			results <- result{name, models, err}
		}(name, p)
	}
	errs = map[string]error{}
	for range providers {
		r := <-results
		if r.err != nil {
			errs[r.name] = r.err
			continue
		}
		for _, m := range r.models {
			choices = append(choices, ModelChoice{Provider: r.name, Model: m.ID, ContextWindow: m.window()})
		}
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Provider != choices[j].Provider {
			return choices[i].Provider < choices[j].Provider
		}
		return choices[i].Model < choices[j].Model
	})
	return choices, errs
}

func ProviderModels(ctx context.Context, p Provider) ([]string, error) {
	models, err := newClient(p).models(ctx)
	if err != nil && p.key() != "" {
		err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
	}
	sort.Strings(models)
	return models, err
}

// SelectSession changes the model of this session and makes it the default for new
// sessions by persisting the selection in SQLite (see SaveDefault).
// Saved sessions keep their own model. Call between turns. Failed validation or
// persistence of the session restores the old selection.
func (a *Agent) SelectSession(ctx context.Context, provider, model, effort string) error {
	if effort == "" {
		effort = "medium"
	}
	if provider == "" || model == "" {
		return errors.New("provider and model are required")
	}
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.sel
	c.sel = selection{provider, model, effort}
	err := a.applyLocked(c.base)
	if err == nil {
		_, err = a.db.ExecContext(ctx, `UPDATE sessions SET provider = ?, model = ?, effort = ? WHERE id = ?`, provider, model, effort, a.id)
	}
	if err != nil {
		c.sel = previous
		_ = a.applyLocked(c.base)
		return err
	}
	if err := SaveDefault(a.home, c.base, c.sel); err != nil {
		return fmt.Errorf("model selected for this session, but the default was not saved: %w", err)
	}
	if base, err := LoadConfig(a.home, a.Shell.Dir); err == nil {
		_ = a.applyLocked(base)
	}
	return nil
}

// SaveDefault persists the default model in SQLite.
func SaveDefault(home string, base Settings, sel selection) error {
	base.Provider, base.Model, base.Effort = sel.Provider, sel.Model, sel.Effort
	if base.Effort == "" {
		base.Effort = "medium"
	}
	return WriteConfig(home, base)
}
