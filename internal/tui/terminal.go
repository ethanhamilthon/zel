package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea v1.3.10 exposes unknown CSI sequences only through Stringer;
// it has no native Kitty keyboard protocol API. Keep compatibility here.
func normalizeTerminalKey(msg tea.Msg) tea.Msg {
	if sequence, ok := msg.(fmt.Stringer); ok {
		if sequence.String() == fmt.Sprintf("?%+v?", []byte{1}) {
			return tea.KeyMsg{Type: tea.KeyCtrlA}
		}
		if action, super, release, ok := clipboardSequence(sequence.String()); ok {
			switch {
			case release:
				return nil
			case action == 'a':
				return tea.KeyMsg{Type: tea.KeyCtrlA}
			case action == 'v':
				return tea.KeyMsg{Type: tea.KeyCtrlV}
			case super:
				return nil
			case action == 'y':
				return tea.KeyMsg{Type: tea.KeyCtrlY}
			case action == 'd':
				return tea.KeyMsg{Type: tea.KeyCtrlD}
			case action == 'x':
				return tea.KeyMsg{Type: tea.KeyCtrlX}
			case action == 'm':
				return menuShortcut(menuModel)
			case action == 'r':
				return menuShortcut(menuResume)
			case action == 'i':
				return menuShortcut(menuInfo)
			case action == 't':
				return menuShortcut(menuTracing)
			case action == 'e':
				return tea.KeyMsg{Type: tea.KeyCtrlE}
			case action == 'n':
				return tea.KeyMsg{Type: tea.KeyCtrlN}
			case action == 'f':
				return tea.KeyMsg{Type: tea.KeyCtrlF}
			case action == 'q':
				return tea.KeyMsg{Type: tea.KeyCtrlQ}
			case action == 'k':
				return tea.KeyMsg{Type: tea.KeyCtrlK}
			default:
				return tea.KeyMsg{Type: tea.KeyCtrlC}
			}
		}
		if key, ok := kittyKey(sequence.String()); ok {
			return key
		}
		switch sequence.String() {
		case fmt.Sprintf("?CSI%+v?", []byte("13;2u")), fmt.Sprintf("?CSI%+v?", []byte("27;2;13~")), fmt.Sprintf("?CSI%+v?", []byte("13;2~")):
			return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
		}
	}
	return msg
}

func csiU(sequence string) ([]string, []string, bool) {
	if !strings.HasPrefix(sequence, "?CSI[") || !strings.HasSuffix(sequence, "]?") {
		return nil, nil, false
	}
	var payload strings.Builder
	for _, number := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(sequence, "?CSI["), "]?")) {
		value, err := strconv.Atoi(number)
		if err != nil || value < 0 || value > 127 {
			return nil, nil, false
		}
		payload.WriteByte(byte(value))
	}
	text, ok := strings.CutSuffix(payload.String(), "u")
	parts := strings.Split(text, ";")
	if !ok || len(parts) > 2 {
		return nil, nil, false
	}
	var modifiers []string
	if len(parts) == 2 {
		modifiers = strings.Split(parts[1], ":")
	}
	return strings.Split(parts[0], ":"), modifiers, true
}

// kittyKey decodes the keys that the Kitty keyboard protocol's disambiguate
// mode reports as CSI sequences instead of legacy bytes: Esc arrives as
// CSI 27 u, which Bubble Tea does not recognise.
func kittyKey(sequence string) (tea.Msg, bool) {
	codes, modifiers, ok := csiU(sequence)
	if !ok || codes[0] != "27" {
		return nil, false
	}
	if len(modifiers) > 0 {
		if value, err := strconv.Atoi(modifiers[0]); err != nil || value != 1 {
			return nil, false
		}
		if len(modifiers) > 1 && modifiers[1] == "3" {
			return nil, true
		}
	}
	return tea.KeyMsg{Type: tea.KeyEsc}, true
}

// clipboardSequence decodes Kitty CSI-u Ctrl, Cmd or Meta clipboard and menu shortcuts.
// Kitty reports modifiers and optional base-layout keycodes even on Cyrillic layouts.
// action is the Latin shortcut key; super reports Cmd/Meta without Ctrl.
func clipboardSequence(sequence string) (action byte, super, release, ok bool) {
	codes, modifiers, parsed := csiU(sequence)
	if !parsed || len(modifiers) == 0 {
		return 0, false, false, false
	}
	value, err := strconv.Atoi(modifiers[0])
	if err != nil || value < 1 || (value-1)&(4|8|32) == 0 {
		return 0, false, false, false
	}
	for _, code := range codes {
		switch code {
		case "118", "86", "1084", "1052":
			action = 'v'
		case "121", "89", "1085", "1053":
			action = 'y'
		case "100", "68", "1074", "1042":
			action = 'd'
		case "99", "67", "1089", "1057":
			action = 'c'
		case "120", "88", "1095", "1063":
			action = 'x'
		case "109", "77", "1100", "1068":
			action = 'm'
		case "114", "82", "1082", "1050":
			action = 'r'
		case "105", "73", "1096", "1064":
			action = 'i'
		case "116", "84", "1077", "1045":
			action = 't'
		case "101", "69", "1091", "1059":
			action = 'e'
		case "110", "78", "1090", "1058":
			action = 'n'
		case "102", "70", "1072", "1040":
			action = 'f'
		case "113", "81", "1081", "1049":
			action = 'q'
		case "107", "75", "1083", "1051":
			action = 'k'
		case "97", "65", "1092", "1060", "ф", "Ф":
			action = 'a'
		default:
			continue
		}
		super = (value-1)&(8|32) != 0 && (value-1)&4 == 0
		release = len(modifiers) > 1 && modifiers[1] != "1"
		if action == 'a' && ((value-1)&4 == 0 || (value-1)&(8|32) != 0) {
			return 0, false, false, false
		}
		return action, super, release, true
	}
	return 0, false, false, false
}

type keyboardProtocol struct{ enabled bool }

func (k *keyboardProtocol) close() {
	if k.enabled {
		fmt.Fprint(os.Stdout, "\x1b[<u")
		k.enabled = false
	}
}
func (k *keyboardProtocol) filter(current tea.Model, msg tea.Msg) tea.Msg {
	// ExecProcess releases Bubble Tea's terminal modes, but knows nothing about
	// our Kitty keyboard protocol. Pop it before the editor takes over; the
	// editor negotiates its own keyboard mode. Re-enable it after returning.
	if m, ok := current.(model); ok && m.editorActive {
		k.close()
		return msg
	}
	if _, quit := msg.(tea.QuitMsg); quit {
		k.close()
	} else if !k.enabled {
		fmt.Fprint(os.Stdout, "\x1b[>5u")
		k.enabled = true
	}
	return msg
}
