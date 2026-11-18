package tui

import (
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"zel/internal/config"
	"zel/internal/core"
)

func latinKey(r rune) rune {
	r = unicode.ToLower(r)
	ru, en := []rune("йцукенгшщзфывапролдячсмить"), []rune("qwertyuiopasdfghjklzxcvbnm")
	for i, c := range ru {
		if c == r {
			return en[i]
		}
	}
	return r
}

func normalizeControl(k *tcell.EventKey) *tcell.EventKey {
	if k.Key() == tcell.KeyRune && k.Modifiers() == tcell.ModCtrl {
		r := latinKey(k.Rune())
		if r >= 'a' && r <= 'z' {
			return tcell.NewEventKey(tcell.KeyCtrlA+tcell.Key(r-'a'), 0, tcell.ModCtrl)
		}
	}
	return k
}

func keyName(k *tcell.EventKey) string {
	if k.Key() == tcell.KeyRune {
		r := latinKey(k.Rune())
		switch k.Modifiers() {
		case tcell.ModCtrl:
			return "ctrl+" + string(r)
		case tcell.ModAlt:
			return "alt+" + string(r)
		}
	} else if k.Key() >= tcell.KeyCtrlA && k.Key() <= tcell.KeyCtrlZ && k.Modifiers()&(tcell.ModAlt|tcell.ModShift) == 0 {
		// Enter and Tab keep their editor meaning unless the protocol explicitly
		// reports Ctrl, as these share byte codes in legacy terminals.
		if (k.Key() == tcell.KeyEnter || k.Key() == tcell.KeyTab) && k.Modifiers()&tcell.ModCtrl == 0 {
			return ""
		}
		return "ctrl+" + string(rune('a'+k.Key()-tcell.KeyCtrlA))
	}
	return ""
}

func shortcut(k *tcell.EventKey, keys map[string]string) string {
	name := keyName(k)
	if name == "" {
		return ""
	}
	if keys == nil {
		keys = core.DefaultKeys
	}
	for action, key := range keys {
		if strings.EqualFold(key, name) {
			return action
		}
	}
	return ""
}

func commandShortcut(k *tcell.EventKey, cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "", false
	}
	name := keyName(k)
	if name == "" {
		return "", false
	}
	for _, cmd := range cfg.Commands {
		if cmd.Key != "" && strings.EqualFold(cmd.Key, name) {
			return cmd.Name, true
		}
	}
	return "", false
}
