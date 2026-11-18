package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

var DefaultKeys = map[string]string{
	"menu": "ctrl+k", "models": "ctrl+m", "sessions": "ctrl+r", "new_session": "ctrl+n",
	"info": "ctrl+i", "trace": "ctrl+t", "editor": "ctrl+e", "stop": "ctrl+d", "quit": "ctrl+q", "redraw": "ctrl+l",
}

// parseKeys reads keys on top of base. Key names are deliberately limited to
// unambiguous modified letters.
func parseKeys(v any, base map[string]string) (map[string]string, error) {
	table, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("keys: must be a table")
	}
	keys := maps.Clone(DefaultKeys)
	if base != nil {
		keys = maps.Clone(base)
	}
	for _, action := range slices.Sorted(maps.Keys(table)) {
		if _, ok := keys[action]; !ok {
			return nil, fmt.Errorf("keys: unknown action %q", action)
		}
		key, ok := table[action].(string)
		if !ok {
			return nil, fmt.Errorf("keys.%s: must be a string", action)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" && !ValidKey(key) {
			return nil, fmt.Errorf("keys.%s: use ctrl+LETTER, alt+LETTER or an empty string", action)
		}
		keys[action] = key
	}
	used := map[string]string{}
	for _, action := range slices.Sorted(maps.Keys(keys)) {
		key := keys[action]
		if key == "" {
			continue
		}
		if prev, ok := used[key]; ok {
			return nil, fmt.Errorf("keys: %s and %s both use %s", prev, action, key)
		}
		used[key] = action
	}
	return keys, nil
}

// ValidKey reports whether key is ctrl+LETTER or alt+LETTER (lower case).
func ValidKey(key string) bool {
	return (len(key) == 6 && strings.HasPrefix(key, "ctrl+") && key[5] >= 'a' && key[5] <= 'z') ||
		(len(key) == 5 && strings.HasPrefix(key, "alt+") && key[4] >= 'a' && key[4] <= 'z')
}
