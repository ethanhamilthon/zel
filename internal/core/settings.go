package core

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"zel/internal/config"
	"zel/internal/provider"
	"zel/internal/shell"
	"zel/internal/store"
)

func openDB(home string) (*store.Store, error) { return store.Open(home) }

// confHolder keeps the zel.toml snapshot of a session. It is shared by pointer
// so a session switch sees the same notice handler.
type confHolder struct {
	mu     sync.RWMutex
	cfg    *config.Config
	notice func(string)
	models modelListing
}

// modelListing is the session's list of provider models for zel models.
// mu guards the fields; it is never held while providers are asked.
type modelListing struct {
	mu       sync.Mutex
	key      string // the providers the list was made for
	ready    bool
	choices  []ModelChoice
	failures map[string]string
	flight   chan struct{} // closed when the listing in progress ends
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

// SetNoticeHandler routes config notices (broken layers, failed ${{ }}) to the UI.
func (a *Runtime) SetNoticeHandler(fn func(string)) { a.session().conf.setNotice(fn) }

func (s *session) notify(msg string) {
	s.LogEvent("notice", msg)
	if fn := s.conf.noticeHandler(); fn != nil {
		fn(msg)
	} else if s.opts.Mode == "headless" {
		fmt.Fprintln(os.Stderr, "zel: "+msg)
	}
}

// configLayers are the zel.toml layers of this session: ~/.zel/zel.toml,
// ./zel.toml, then the --config files. A child of another session uses the
// layers of its parent (ZEL_CONFIG) unless it was given its own.
func (s *session) configLayers(stored []string) []config.Layer {
	extra := s.opts.Configs
	if len(extra) == 0 && len(stored) > 0 {
		return layersFromPaths(s.home, stored)
	}
	if len(extra) == 0 && s.parent != nil && len(s.parent.config) > 0 {
		return layersFromPaths(s.home, s.parent.config)
	}
	return config.Layers(s.home, s.dir, extra)
}

func layersFromPaths(home string, paths []string) []config.Layer {
	global := filepath.Join(home, "zel.toml")
	var layers []config.Layer
	for i, p := range paths {
		name := "config"
		switch {
		case p == global:
			name = "global"
		case i <= 1 && filepath.Base(p) == "zel.toml":
			name = "local"
		}
		layers = append(layers, config.Layer{Name: name, Path: p})
	}
	if len(layers) == 0 || layers[0].Path != global {
		layers = append([]config.Layer{{Name: "global", Path: global}}, layers...)
	}
	return layers
}

// configPaths are the layer files that exist, in merge order (ZEL_CONFIG).
func (s *session) configPaths() []string {
	var paths []string
	for _, l := range s.layers {
		if _, err := os.Stat(l.Path); err == nil {
			paths = append(paths, l.Path)
		}
	}
	return paths
}

// expander runs a ${{ }} expression: bash in the session folder with the
// session environment and env on top.
func (s *session) expander(env map[string]string) config.Expander {
	return func(ctx context.Context, script string) (string, error) {
		vars := s.baseEnv()
		maps.Copy(vars, env)
		cmd := shell.Command(ctx, s.dir, vars, script)
		var out, errs bytes.Buffer
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := cmd.Start(); err != nil {
			return "", err
		}
		if err := shell.Reap(cmd); err != nil {
			if ctx.Err() != nil {
				return out.String(), fmt.Errorf("timed out")
			}
			return out.String(), fmt.Errorf("%v%s", err, cmp.Or(": "+FirstLine(errs.String()), ""))
		}
		return out.String(), nil
	}
}

// loadConfig reads the layers into a new snapshot and applies its settings.
// Broken layers are skipped with a notice.
func (s *session) loadConfig() error {
	cfg := config.Load(context.Background(), s.layers, s.expander)
	for _, e := range cfg.Errors {
		s.notify("zel.toml skipped: " + e)
	}
	for _, w := range cfg.Warnings {
		s.notify("zel.toml: " + w)
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

// Reload reads the config again for hot reload. If a layer is broken, the old
// snapshot stays and a notice says why; the system prompt is rebuilt when it changed.
func (a *Runtime) Reload() error { return a.session().Reload() }

func (s *session) Reload() error {
	cfg := config.Load(context.Background(), s.layers, s.expander)
	if len(cfg.Errors) > 0 && s.conf.get() != nil {
		msg := "zel.toml has errors; the previous config stays: " + strings.Join(cfg.Errors, "; ")
		s.fireHooks("config_changed", map[string]any{"ok": false, "error": msg, "files": s.configPaths()})
		return errors.New(msg)
	}
	for _, w := range cfg.Warnings {
		s.notify("zel.toml: " + w)
	}
	if err := s.applyConfig(cfg); err != nil {
		return err
	}
	if s.promptFrozen && s.assemblePrompt() != s.prompt {
		previous := s.prompt
		s.promptFrozen = false
		if _, err := s.instructions(context.Background()); err != nil {
			s.prompt, s.promptFrozen = previous, true
			return err
		}
	}
	s.fireHooks("config_changed", map[string]any{"ok": true, "error": "", "files": s.configPaths()})
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

// ConfigFiles are the config layers that exist, in merge order.
func (a *Runtime) ConfigFiles() []string { return a.session().configPaths() }

// target is the layer file a menu change of key goes to: the layer it comes
// from now, else the layer of fallback, else ~/.zel/zel.toml.
func (s *session) target(key string, fallback ...string) string {
	if cfg := s.conf.get(); cfg != nil {
		if p := cfg.Sources[key]; p != "" {
			return p
		}
		for _, f := range fallback {
			if p := cfg.Sources[f]; p != "" {
				return p
			}
		}
	}
	return filepath.Join(s.home, "zel.toml")
}

// UpdateSettings writes what the settings and provider menus changed into the
// zel.toml layers the values come from, then reloads. Call while idle.
func (a *Runtime) UpdateSettings(next Settings) error { return a.session().UpdateSettings(next) }

func (s *session) UpdateSettings(next Settings) error {
	if err := config.Validate(next); err != nil {
		return err
	}
	cur := s.Settings()
	type change struct {
		key   string
		value any
	}
	var set []change
	add := func(key string, changed bool, value any) {
		if changed {
			set = append(set, change{key, value})
		}
	}
	add("settings.compaction", next.CompactionThreshold != cur.CompactionThreshold, next.CompactionThreshold)
	add("settings.reasoning_summary", next.ReasoningSummary != cur.ReasoningSummary, next.ReasoningSummary)
	add("settings.accent", next.UI.Accent != cur.UI.Accent, next.UI.Accent)
	add("settings.slash_menu", next.UI.SlashMenu != cur.UI.SlashMenu, next.UI.SlashMenu)
	add("settings.bash_timeout", next.BashTimeout != cur.BashTimeout, next.BashTimeout)
	for _, c := range set {
		if err := config.SetKey(s.target(c.key), c.key, c.value); err != nil {
			return err
		}
	}
	for name, p := range next.Providers {
		old, ok := cur.Providers[name]
		file := s.target("providers." + name)
		if !ok || old.BaseURL != p.BaseURL {
			if err := config.SetKey(file, "providers."+name+".base_url", p.BaseURL); err != nil {
				return err
			}
		}
		if !ok || old.API != p.API {
			if err := config.SetKey(file, "providers."+name+".api", cmp.Or(p.API, config.APIResponses)); err != nil {
				return err
			}
		}
		if !ok || old.APIKey != p.APIKey {
			if err := config.SetKey(file, "providers."+name+".api_key", p.APIKey); err != nil {
				return err
			}
		}
	}
	for name := range cur.Providers {
		if _, ok := next.Providers[name]; !ok {
			if err := config.DeleteKey(s.target("providers."+name), "providers."+name); err != nil {
				return err
			}
		}
	}
	if next.Provider != cur.Provider || next.Model != cur.Model || next.Effort != cur.Effort {
		if err := s.saveDefault(selection{next.Provider, next.Model, next.Effort}); err != nil {
			return err
		}
	}
	return s.loadConfig()
}

// saveDefault writes provider, model and effort each into the layer it comes
// from (a key that no layer sets goes where the model is set).
func (s *session) saveDefault(sel selection) error {
	cfg := s.conf.get()
	for _, kv := range [][2]string{{"provider", sel.Provider}, {"model", sel.Model}, {"effort", sel.Effort}} {
		if kv[1] == "" {
			continue
		}
		if cfg != nil {
			cur := map[string]string{"provider": cfg.Settings.Provider, "model": cfg.Settings.Model, "effort": cfg.Settings.Effort}[kv[0]]
			if cur == kv[1] && cfg.Sources[kv[0]] != "" {
				continue
			}
		}
		if err := config.SetKey(s.target(kv[0], "model"), kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// WriteConfig writes settings into ~/.zel/zel.toml (onboarding, tests).
func WriteConfig(home string, s Settings) error {
	path := filepath.Join(home, "zel.toml")
	for name, p := range s.Providers {
		for k, v := range map[string]string{"base_url": p.BaseURL, "api": cmp.Or(p.API, config.APIResponses), "api_key": p.APIKey} {
			if err := config.SetKey(path, "providers."+name+"."+k, v); err != nil {
				return err
			}
		}
	}
	for k, v := range map[string]string{"provider": s.Provider, "model": s.Model, "effort": s.Effort} {
		if v != "" {
			if err := config.SetKey(path, k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadConfig is the settings of ~/.zel/zel.toml and ./zel.toml of dir, without ${{ }}.
func LoadConfig(home, dir string) Settings {
	return config.Load(context.Background(), config.Layers(home, dir, nil), nil).Settings
}
