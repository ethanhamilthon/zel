package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/muesli/termenv"
)

// termOut receives terminal status sequences; Run sets it to stdout, tests leave it nil.
var termOut io.Writer

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// termState reports turn progress to the terminal through OSC 9;4 progress,
// the window title.
type termState struct {
	state, title string // state: idle, working, done or error
	frame        int
	lastTitle    string
	background   string // optional OSC 11 terminal background
	used         bool
}

// setBackground paints the terminal's default background through OSC 11, so every
// cell without its own color follows this background; empty restores the default.
func (t *termState) setBackground(color string) {
	color = oscColor(color)
	if color == t.background {
		return
	}
	if color == "" {
		t.write("\x1b]111\x07")
	} else {
		t.write("\x1b]11;" + color + "\x07")
	}
	t.background = color
}

// oscColor turns a lipgloss color ("#rrggbb" or an ANSI 0-255 index) into an OSC color.
func oscColor(value string) string {
	value = strings.TrimSpace(value)
	if n, err := strconv.Atoi(value); err == nil && n >= 0 && n < 256 {
		return termenv.ConvertToRGB(termenv.ANSI256Color(n)).Hex()
	}
	if strings.HasPrefix(value, "#") && strings.Trim(value[1:], "0123456789abcdefABCDEF") == "" && (len(value) == 4 || len(value) == 7) {
		return value
	}
	return ""
}

func (t *termState) start(title string) {
	t.state, t.title, t.frame = "working", title, 0
	t.write("\x1b]9;4;3\x07")
	t.setTitle()
}

func (t *termState) tick() {
	if t.state == "working" {
		t.frame++
		t.setTitle()
	}
}

// finish ends a turn; status is the controller's done status: ok, error or cancelled.
func (t *termState) finish(status, title string) {
	t.title = title
	switch status {
	case "ok":
		t.state = "done"
		t.write("\x1b]9;4;0\x07")
	case "error":
		t.state = "error"
		t.write("\x1b]9;4;2;100\x07")
	default:
		t.state = "idle"
		t.write("\x1b]9;4;0\x07")
	}
	t.setTitle()
}

// seen drops the finished mark once the user interacts: the result was noticed.
func (t *termState) seen() {
	if t.state != "done" && t.state != "error" {
		return
	}
	if t.state == "error" {
		t.write("\x1b]9;4;0\x07")
	}
	t.state = "idle"
	t.setTitle()
}

func (t *termState) rename(title string) {
	if title != t.title {
		t.title = title
		t.setTitle()
	}
}

// reset clears progress and the title when the interface exits.
func (t *termState) reset() {
	if t.used {
		t.write("\x1b]9;4;0\x07\x1b]2;\x07")
	}
	t.setBackground("")
}

func (t *termState) setTitle() {
	mark := map[string]string{"working": spinnerFrames[t.frame%len(spinnerFrames)] + " ", "done": "✓ ", "error": "✗ "}[t.state]
	if text := "\x1b]2;" + mark + termTitle(t.title) + "\x07"; text != t.lastTitle {
		t.write(text)
		t.lastTitle = text
	}
}

func (t *termState) write(s string) {
	if termOut != nil {
		fmt.Fprint(termOut, s)
	}
	t.used = true
}

// termTitle makes a session title safe and short for OSC payloads.
func termTitle(title string) string {
	title = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || r == ';' {
			return ' '
		}
		return r
	}, title)), " ")
	if title == "" {
		return "zel"
	}
	if r := []rune(title); len(r) > 40 {
		return string(r[:39]) + "…"
	}
	return title
}
