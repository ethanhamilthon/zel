package core

import (
	"context"
	"errors"
	"fmt"
	"zel/internal/entities"
	"zel/internal/provider"
)

type ModelChoice = entities.ModelChoice

func AllModels(ctx context.Context, providers map[string]Provider) ([]ModelChoice, map[string]error) {
	clients := make(map[string]*provider.Client, len(providers))
	for name, p := range providers {
		clients[name] = newClient(p)
	}
	return provider.AllModels(ctx, clients)
}

func (a *Runtime) SelectSession(ctx context.Context, provider, model, effort string) error {
	return a.session().SelectSession(ctx, provider, model, effort)
}

func (s *session) SelectSession(ctx context.Context, provider, model, effort string) error {
	if err := s.ConfigureSession(ctx, provider, model, effort); err != nil {
		return err
	}
	c := s.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := SaveDefault(s.home, c.base, c.sel); err != nil {
		return fmt.Errorf("model selected for this session, but the default was not saved: %w", err)
	}
	if base, err := LoadConfig(s.home, s.dir); err == nil {
		_ = s.applyLocked(base)
	}
	return nil
}

func (a *Runtime) ConfigureSession(ctx context.Context, provider, model, effort string) error {
	return a.session().ConfigureSession(ctx, provider, model, effort)
}

// ConfigureSession overrides this session without changing saved defaults. Call while idle.
func (s *session) ConfigureSession(ctx context.Context, provider, model, effort string) error {
	if effort == "" {
		effort = "medium"
	}
	if provider == "" || model == "" {
		return errors.New("provider and model are required")
	}
	c := s.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.sel
	c.sel = selection{provider, model, effort}
	err := s.applyLocked(c.base)
	if err == nil {
		err = s.db.SelectModel(ctx, s.id, provider, model, effort)
	}
	if err != nil {
		c.sel = previous
		_ = s.applyLocked(c.base)
		return err
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
