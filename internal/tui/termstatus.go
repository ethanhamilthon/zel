package tui

import (
	"fmt"
	"io"
	"strings"
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
	used         bool
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
