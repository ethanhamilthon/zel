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

func LoadConfig(home, dir string) (Settings, error) {
	db, err := openDB(DBPath(home))
	if err != nil {
		return Settings{}, err
	}
	defer db.Close()
	data, err := db.Preferences()
	if err != nil {
		return Settings{}, err
	}
	return config.Load(home, dir, data)
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
func (a *Runtime) ConfigFiles() []string {
	var files []string
	for _, path := range ConfigPaths(a.home, a.Shell.Dir) {
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
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

func (a *Runtime) applyLocked(base Settings) error {
	s := base
	s.Providers = maps.Clone(base.Providers)
	if sel := a.cfg.sel; sel.Model != "" {
		s.Provider, s.Model, s.Effort = sel.Provider, sel.Model, sel.Effort
	}
	if s.Effort == "" {
		s.Effort = "medium"
	}
	if err := config.Validate(s); err != nil {
		return err
	}
	a.cfg.base, a.cfg.s = base, s
	a.cfg.client = newClient(s.Providers[s.Provider])
	return nil
}

// fixSelection applies a session's stored model; one that no longer validates is dropped.
func (a *Runtime) fixSelection(sel selection) {
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sel = sel
	if sel.Model == "" || a.applyLocked(c.base) != nil {
		c.sel = selection{}
		_ = a.applyLocked(c.base)
	}
}

func (a *Runtime) snapshot() (Settings, *provider.Client) {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked(), a.cfg.client
}

func (a *Runtime) settingsLocked() Settings {
	s := a.cfg.s
	s.Providers = maps.Clone(s.Providers)
	return s
}

func (a *Runtime) Settings() Settings {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked()
}
