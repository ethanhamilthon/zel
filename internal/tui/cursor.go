package tui

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

var caretPosition = regexp.MustCompile(`\x1b\]1337;ZelCaret;([0-9]+);([0-9]+)\x07`)

// cursorOutput overlays a native caret after Bubble Tea finishes each write.
// Save/restore keeps the renderer's own cursor position independent of the caret.
// A terminal caret occupies no text cells and never replaces a character.
type cursorOutput struct {
	// Preserve Read and Close too: Bubble Tea uses term.File to detect a TTY.
	io.ReadWriteCloser
	mu            sync.Mutex
	active, saved bool
	row, column   int
}

func (o *cursorOutput) Fd() uintptr {
	if f, ok := o.ReadWriteCloser.(interface{ Fd() uintptr }); ok {
		return f.Fd()
	}
	return ^uintptr(0)
}

func (o *cursorOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	text := string(p)
	for _, match := range caretPosition.FindAllStringSubmatch(text, -1) {
		o.row, _ = strconv.Atoi(match[1])
		o.column, _ = strconv.Atoi(match[2])
	}
	text = caretPosition.ReplaceAllString(text, "")
	var out strings.Builder
	if o.saved {
		out.WriteString("\x1b8")
		o.saved = false
	}
	out.WriteString(text)
	if strings.Contains(text, "\x1b[?1049h") {
		o.active = true
	}
	if strings.Contains(text, "\x1b[?1049l") {
		o.active = false
		o.row, o.column = 0, 0
		out.WriteString("\x1b[0 q")
	}
	if o.active {
		if o.row > 0 && o.column > 0 {
			// DECSCUSR 6 selects a steady, thin bar cursor.
			fmt.Fprintf(&out, "\x1b7\x1b[6 q\x1b[%d;%dH\x1b[?25h", o.row, o.column)
			o.saved = true
		} else {
			out.WriteString("\x1b[?25l")
		}
	}
	n, err := io.WriteString(o.ReadWriteCloser, out.String())
	if err != nil {
		return 0, err
	}
	if n != out.Len() {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// frame joins screen rows, locating the composer caret and dropping trailing
// blank cells. Padding spaces would otherwise end up in terminal selections and
// paste as extra lines; Bubble Tea clears the rest of each shorter row itself.
func frame(lines []string) string {
	row, column := 0, 0
	for i, line := range lines {
		if at := strings.Index(line, composerCursorMarker); at >= 0 {
			row, column = i+1, ansi.StringWidth(line[:at])+1
		}
		lines[i] = trimTrailingSpaces(strings.ReplaceAll(line, composerCursorMarker, ""))
	}
	return strings.Join(lines, "\n") + fmt.Sprintf("\x1b]1337;ZelCaret;%d;%d\x07", row, column)
}

var escapeSequence = regexp.MustCompile(`\x1b\[[0-9;:?<=>]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[^\[\]]`)

// trimTrailingSpaces removes unstyled spaces at the end of a row while keeping
// its escape sequences. Spaces drawn with reverse video or a background stay.
func trimTrailingSpaces(line string) string {
	if !strings.Contains(line, " ") {
		return line
	}
	escapes := escapeSequence.FindAllStringIndex(line, -1)
	var out strings.Builder
	var tail strings.Builder // escapes seen since the last visible text
	pending, styled := 0, false
	flush := func() {
		out.WriteString(strings.Repeat(" ", pending))
		pending = 0
	}
	text := func(s string) {
		for _, r := range s {
			if r == ' ' && !styled {
				pending++
				continue
			}
			flush()
			out.WriteString(tail.String())
			tail.Reset()
			out.WriteRune(r)
		}
	}
	at := 0
	for _, e := range escapes {
		text(line[at:e[0]])
		seq := line[e[0]:e[1]]
		if strings.HasSuffix(seq, "m") && strings.HasPrefix(seq, "\x1b[") {
			params := strings.Split(seq[2:len(seq)-1], ";")
			for i := 0; i < len(params); i++ {
				switch params[i] {
				case "", "0", "27", "49":
					styled = false
				case "7":
					styled = true
				case "48":
					styled = true
					fallthrough
				case "38", "58":
					if i+1 < len(params) && params[i+1] == "5" {
						i += 2
					} else if i+1 < len(params) && params[i+1] == "2" {
						i += 4
					}
				}
			}
		}
		if pending > 0 || tail.Len() > 0 {
			tail.WriteString(seq)
		} else {
			out.WriteString(seq)
		}
		at = e[1]
	}
	text(line[at:])
	out.WriteString(tail.String())
	return out.String()
}
