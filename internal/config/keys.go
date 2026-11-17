package config

import (
	"fmt"
	"strings"
)

var DefaultKeys = map[string]string{
	"menu": "ctrl+k", "models": "ctrl+m", "sessions": "ctrl+r", "new_session": "ctrl+n",
	"info": "ctrl+i", "trace": "ctrl+t", "editor": "ctrl+e", "stop": "ctrl+d", "quit": "ctrl+q", "redraw": "ctrl+l",
}

// Key names are deliberately limited to unambiguous modified letters.
func (p *parser) keyBindings(v any) error {
	table, err := asTable(v, "keys")
	if err != nil {
		return err
	}
	keys := map[string]string{}
	for action, key := range DefaultKeys {
		keys[action] = key
	}
	for action, value := range table {
		if _, ok := keys[action]; !ok {
			return fmt.Errorf("keys: unknown action %q", action)
		}
		key, ok := value.(string)
		if !ok {
			return fmt.Errorf("keys.%s: must be a string", action)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" && !(len(key) == 6 && strings.HasPrefix(key, "ctrl+") && key[5] >= 'a' && key[5] <= 'z') && !(len(key) == 5 && strings.HasPrefix(key, "alt+") && key[4] >= 'a' && key[4] <= 'z') {
			return fmt.Errorf("keys.%s: use ctrl+LETTER, alt+LETTER or an empty string", action)
		}
		keys[action] = key
	}
	used := map[string]string{}
	for action, key := range keys {
		if key == "" {
			continue
		}
		if prev, ok := used[key]; ok {
			return fmt.Errorf("keys: %s and %s both use %s", prev, action, key)
		}
		used[key] = action
	}
	p.cfg.Settings.Keys = keys
	return nil
}
