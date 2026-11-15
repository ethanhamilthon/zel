package core

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"zel/internal/config"
	"zel/internal/provider"
)

func LoadConfig(home string) (Settings, error) {
	db, err := openDB(DBPath(home))
	if err != nil {
		return Settings{}, err
	}
	defer db.Close()
	data, err := db.Preferences()
	if err != nil {
		return Settings{}, err
	}
	return config.Load(home, data)
}

// WriteConfig persists only application preferences, never project instructions.
func WriteConfig(home string, s Settings) error {
	if err := config.Validate(s); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	db, err := openDB(filepath.Join(home, "zel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.SetPreferences(data)
	return err
}

// UpdateSettings is called between turns by the settings/provider UI.
func (a *Runtime) UpdateSettings(s Settings) error {
	if err := WriteConfig(a.home, s); err != nil {
		return err
	}
	return a.ReloadConfig()
}
func (a *Runtime) DBPath() string { return a.Settings().DBPath }

// Project is the project directory of the session.
func (a *Runtime) Project() string { return a.project }

// ConfigFiles are the config layers that exist, in merge order.
func (a *Runtime) ConfigFiles() []string { return a.session().ConfigFiles() }

func (s *session) ConfigFiles() []string {
	var files []string
	for _, layer := range LuaLayers(s.home, s.dir) {
		if _, err := os.Stat(layer.Path); err == nil {
			files = append(files, layer.Path)
		}
	}
	return files
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

func (a *Runtime) fixSelection(sel selection) { a.session().fixSelection(sel) }

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
	return st
}

func (a *Runtime) Settings() Settings { return a.session().Settings() }

func (s *session) Settings() Settings {
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	return s.settingsLocked()
}
