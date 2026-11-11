package tui

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
