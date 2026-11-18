package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"

	"zel/internal/config"
	"zel/internal/provider"
)

// confHolder keeps the config snapshot of a session. It is shared by pointer
// so a session switch sees the same notice handler.
type confHolder struct {
	mu     sync.RWMutex
	cfg    *config.Config
	notice func(string)
}

func (h *confHolder) get() *config.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *confHolder) set(cfg *config.Config) {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
}

func (h *confHolder) noticeHandler() func(string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.notice
}

func (h *confHolder) setNotice(fn func(string)) {
	h.mu.Lock()
	h.notice = fn
	h.mu.Unlock()
}

func (s *session) notify(msg string) {
	s.LogEvent("notice", msg)
	if fn := s.conf.noticeHandler(); fn != nil {
		fn(msg)
	} else if s.opts.Mode == "headless" {
		fmt.Fprintln(os.Stderr, "zel: "+msg)
	}
}

// configFile is the config file of this session: --config, else the file of
// the parent session (ZEL_CONFIG), else ~/.config/zel/init.lua.
func (s *session) configFile() string {
	switch {
	case s.opts.Config != "":
		return s.opts.Config
	case s.parent != nil && s.parent.config != "":
		return s.parent.config
	}
	return config.DefaultPaths(s.home, s.dir, "").Init
}

func (s *session) paths() config.Paths {
	flag := s.opts.Config
	if flag == "" && s.parent != nil {
		flag = s.parent.config
	}
	p := config.DefaultPaths(s.home, s.dir, flag)
	if flag == "" && os.Getenv("ZEL_NO_LOCAL") == "1" {
		p.Local = ""
	}
	return p
}

func (s *session) watchFiles() []string {
	if cfg := s.conf.get(); cfg != nil && len(cfg.Files) > 0 {
		return cfg.Files
	}
	p := s.paths()
	return []string{p.Init, p.Local, p.Connect}
}

// ConfigFile is the config file of the session.
func (a *Runtime) ConfigFile() string { return a.session().path }

// ConfigErrors are the reasons the config file was skipped; the defaults are in use then.
func (a *Runtime) ConfigErrors() []string {
	if cfg := a.session().conf.get(); cfg != nil {
		return cfg.Errors
	}
	return nil
}

// loadConfigStub builds a Config from connect.json and defaults (Lua execution
// is wired during luart integration).
func (s *session) loadConfigStub() *config.Config {
	p := s.paths()
	cfg := &config.Config{
		Paths:    p,
		Settings: config.DefaultSettings(),
		Files:    []string{p.Init, p.Local, p.Connect},
	}
	conn, err := config.LoadConnect(p.Connect)
	if err == nil {
		cfg.Settings.Providers = conn.Providers
		if conn.Provider != "" {
			cfg.Settings.Provider = conn.Provider
		}
		if conn.Model != "" {
			cfg.Settings.Model = conn.Model
		}
		if conn.Effort != "" {
			cfg.Settings.Effort = conn.Effort
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cfg.Errors = append(cfg.Errors, err.Error())
	}
	return cfg
}

// loadConfig reads the config into a new snapshot and applies its settings.
// A broken file is skipped with a notice.
func (s *session) loadConfig() error {
	cfg := s.loadConfigStub()
	for _, e := range cfg.Errors {
		s.notify("config skipped: " + e)
	}
	for _, w := range cfg.Warnings {
		s.notify("config: " + w)
	}
	return s.applyConfig(cfg)
}

func (s *session) applyConfig(cfg *config.Config) error {
	if s.cfg == nil {
		s.cfg = &settingsState{}
	}
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	err := s.applyLocked(cfg.Settings)
	if err != nil {
		// A stale session model must not block a valid config.
		s.cfg.sel = selection{}
		err = s.applyLocked(cfg.Settings)
	}
	if err == nil {
		s.conf.set(cfg)
	}
	return err
}

// Reload reads the config again for hot reload. If the file is broken, the old
// snapshot stays and a notice says why; the system prompt is rebuilt when it changed.
func (a *Runtime) Reload() error { return a.session().Reload() }

func (s *session) Reload() error {
	cfg := s.loadConfigStub()
	if len(cfg.Errors) > 0 && s.conf.get() != nil {
		msg := "config has errors; the previous config stays: " + strings.Join(cfg.Errors, "; ")
		s.fireHooks("config_changed", map[string]any{"ok": false, "error": msg, "file": s.path})
		return errors.New(msg)
	}
	for _, w := range cfg.Warnings {
		s.notify("config: " + w)
	}
	if err := s.applyConfig(cfg); err != nil {
		return err
	}
	s.histMu.RLock()
	frozen, previous := s.promptFrozen, s.prompt
	s.histMu.RUnlock()
	if frozen && s.assemblePrompt() != previous {
		s.histMu.Lock()
		s.promptFrozen = false
		s.histMu.Unlock()
		if _, err := s.instructions(context.Background()); err != nil {
			s.histMu.Lock()
			s.prompt, s.promptFrozen = previous, true
			s.histMu.Unlock()
			return err
		}
	}
	s.fireHooks("config_changed", map[string]any{"ok": true, "error": "", "file": s.path})
	return nil
}

// settingsState is the live configuration of an agent. The turn goroutine and the
// clients share it, so every access goes through mu.
type settingsState struct {
	mu     sync.Mutex
	base   Settings
	sel    selection
	s      Settings
	client *provider.Client
}

type selection struct{ Provider, Model, Effort string }

func (s *session) applyLocked(base Settings) error {
	st := base
	st.Providers = maps.Clone(base.Providers)
	if sel := s.cfg.sel; sel.Model != "" {
		st.Provider, st.Model, st.Effort = sel.Provider, sel.Model, sel.Effort
	}

	if st.Effort == "" {
		st.Effort = "medium"
	}
	if err := config.Validate(st); err != nil {
		return err
	}
	s.cfg.base, s.cfg.s = base, st
	s.cfg.client = newClient(st.Providers[st.Provider])
	return nil
}

// fixSelection applies a session's stored model; one that no longer validates is dropped.
func (s *session) fixSelection(sel selection) {
	c := s.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sel = sel
	if sel.Model == "" || s.applyLocked(c.base) != nil {
		c.sel = selection{}
		_ = s.applyLocked(c.base)
	}
}

func (s *session) snapshot() (Settings, *provider.Client) {
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	return s.settingsLocked(), s.cfg.client
}

func (s *session) settingsLocked() Settings {
	st := s.cfg.s
	st.Providers = maps.Clone(st.Providers)
	st.Env = maps.Clone(st.Env)
	st.Keys = maps.Clone(st.Keys)
	return st
}

func (a *Runtime) Settings() Settings { return a.session().Settings() }

func (s *session) Settings() Settings {
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	return s.settingsLocked()
}

// Project is the project directory of the session.
func (a *Runtime) Project() string { return a.project }

// saveDefault writes the selected provider, model and effort into connect.json.
func (s *session) saveDefault(sel selection) error {
	return config.SaveSelection(s.paths().Connect, sel.Provider, sel.Model, sel.Effort)
}
