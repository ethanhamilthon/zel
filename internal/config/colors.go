package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
)

// ColorKeys are the keys of colors in documentation order; "trace.x" is x in
// colors.trace.
var ColorKeys = [...]string{
	"bg", "accent", "agent", "user", "user_highlight", "gray", "dark_gray", "error",
	"trace.system", "trace.user", "trace.agent", "trace.lua", "trace.notice", "trace.output", "trace.error",
}

// colorDefaults is the default theme, in ColorKeys order.
var colorDefaults = Colors{
	"", "#d97757", "#d6d6d6", "#606060", "#8c8c8c", "#929292", "#606060", "#e27878",
	"#606060", "#d97757", "#ffffff", "#a0a0a0", "#808080", "#404040", "#e27878",
}

// Colors are the TUI colors, one "#rrggbb" per ColorKeys entry; an empty bg is
// the terminal's own background. It is a value: copies do not share state.
type Colors [len(ColorKeys)]string

// DefaultColors is the default theme.
func DefaultColors() Colors { return colorDefaults }

func colorIndex(key string) int {
	for i, k := range ColorKeys {
		if k == key {
			return i
		}
	}
	return -1
}

// Get returns the color of key, or "" for an unknown key.
func (c Colors) Get(key string) string {
	if i := colorIndex(key); i >= 0 {
		return c[i]
	}
	return ""
}

// Set changes the color of key; it reports false for an unknown key.
func (c *Colors) Set(key, value string) bool {
	i := colorIndex(key)
	if i >= 0 {
		c[i] = value
	}
	return i >= 0
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// CheckColor validates the value of a color key: "#rrggbb", or "" for bg.
func CheckColor(key, value string) error {
	if colorIndex(key) < 0 {
		return fmt.Errorf("unknown key %q", "colors."+key)
	}
	if hexColor.MatchString(value) || (key == "bg" && value == "") {
		return nil
	}
	if key == "bg" {
		return fmt.Errorf("colors.%s: must be a #rrggbb hex color or \"\", got %q", key, value)
	}
	return fmt.Errorf("colors.%s: must be a #rrggbb hex color, got %q", key, value)
}

// parseColors reads colors and its nested trace table on top of base.
func parseColors(v any, base Colors) (Colors, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return base, fmt.Errorf("colors: must be a table")
	}
	c := base
	var read func(m map[string]any, prefix string) error
	read = func(m map[string]any, prefix string) error {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			key := prefix + k
			if prefix == "" && k == "trace" {
				sub, ok := m[k].(map[string]any)
				if !ok {
					return fmt.Errorf("colors.trace: must be a table")
				}
				if err := read(sub, "trace."); err != nil {
					return err
				}
				continue
			}
			if colorIndex(key) < 0 || key == "trace" {
				return fmt.Errorf("unknown key %q", "colors."+key)
			}
			s, ok := m[k].(string)
			if !ok {
				return fmt.Errorf("colors.%s: must be a string", key)
			}
			if err := CheckColor(key, s); err != nil {
				return err
			}
			c.Set(key, s)
		}
		return nil
	}
	if err := read(m, ""); err != nil {
		return base, err
	}
	return c, nil
}
