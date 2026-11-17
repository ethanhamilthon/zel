package tui

import (
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
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

func shortcut(k *tcell.EventKey, keys map[string]string) string {
	name := ""
	if k.Key() == tcell.KeyRune {
		r := latinKey(k.Rune())
		switch k.Modifiers() {
		case tcell.ModCtrl:
			name = "ctrl+" + string(r)
		case tcell.ModAlt:
			name = "alt+" + string(r)
		}
	} else if k.Key() >= tcell.KeyCtrlA && k.Key() <= tcell.KeyCtrlZ && k.Modifiers()&(tcell.ModAlt|tcell.ModShift) == 0 {
		// Enter and Tab keep their editor meaning unless the protocol explicitly
		// reports Ctrl, as these share byte codes in legacy terminals.
		if (k.Key() == tcell.KeyEnter || k.Key() == tcell.KeyTab) && k.Modifiers()&tcell.ModCtrl == 0 {
			return ""
		}
		name = "ctrl+" + string(rune('a'+k.Key()-tcell.KeyCtrlA))
	}
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
