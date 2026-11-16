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
	sel := c.sel
	c.mu.Unlock()
	if err := s.saveDefault(sel); err != nil {
		return fmt.Errorf("model selected for this session, but zel.toml was not updated: %w", err)
	}
	if err := s.loadConfig(); err != nil {
		return fmt.Errorf("model selected for this session, but zel.toml does not load: %w", err)
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
	previous := c.sel
	c.sel = selection{provider, model, effort}
	if err := s.applyLocked(c.base); err != nil {
		c.sel = previous
		_ = s.applyLocked(c.base)
		c.mu.Unlock()
		return err
	}
	changed := previous != c.sel
	c.mu.Unlock()
	if changed {
		s.fireHooks("model_changed", map[string]any{"provider": provider, "model": model, "effort": effort})
	}
	if s.HasSavedSession() {
		return s.Save()
	}
	return nil
}
