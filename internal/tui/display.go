package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// display draws frames in place of Bubble Tea's renderer. When the chat moves it
// scrolls the terminal (SU/SD) and repaints only the rows that still differ. A
// terminal moves a text selection along with scrolled content but drops it when
// its rows are redrawn, so the user can select text while the agent works.
type display struct {
	out           io.Writer
	rows          []string
	caret         string
	width, height int
}

// displayFrame is what View hands Bubble Tea while display draws; cursorOutput
// drops the renderer writes that carry it. The renderer still owns raw input,
// the alternate screen, bracketed paste and mouse modes.
const displayFrame = "\x1b]1337;ZelDisplay\x07"

// draw shows view on a width x height screen.
func (d *display) draw(view string, width, height int) {
	caret := ""
	if at := strings.LastIndex(view, "\x1b]1337;ZelCaret;"); at >= 0 {
		view, caret = view[:at], view[at:]
	}
	next := strings.Split(view, "\n")
	next = next[max(0, len(next)-height):]
	for len(next) < height {
		next = append(next, "")
	}
	for i, row := range next {
		next[i] = ansi.Truncate(row, width, "")
	}
	var out strings.Builder
	prev := d.rows
	if width != d.width || height != d.height || len(prev) != height {
		out.WriteString("\x1b[m\x1b[2J")
		prev = make([]string, height)
	} else if shift := scrollShift(prev, next); shift != 0 {
		out.WriteString("\x1b[m")
		if shift > 0 {
			fmt.Fprintf(&out, "\x1b[%dS", shift)
			prev = append(prev[shift:], make([]string, shift)...)
		} else {
			fmt.Fprintf(&out, "\x1b[%dT", -shift)
			prev = append(make([]string, -shift), prev[:height+shift]...)
		}
	}
	for i, row := range next {
		if row == prev[i] {
			continue
		}
		fmt.Fprintf(&out, "\x1b[%d;1H%s", i+1, row)
		if ansi.StringWidth(row) < width {
			out.WriteString("\x1b[m\x1b[K")
		}
	}
	if out.Len() == 0 && caret == d.caret {
		return
	}
	out.WriteString(caret)
	d.rows, d.caret, d.width, d.height = next, caret, width, height
	io.WriteString(d.out, out.String())
}

// scrollShift returns how many rows the content moved up (negative: down) from
// prev to next, or 0 when scrolling would not save repainting any rows.
func scrollShift(prev, next []string) int {
	best, score := 0, -1
	for k := 0; k < len(next) && score < len(next); k++ {
		for _, shift := range []int{k, -k} {
			n := 0
			for i, row := range next {
				if j := i + shift; j >= 0 && j < len(prev) && prev[j] == row {
					n++
				}
			}
			if n > score {
				best, score = shift, n
			}
		}
	}
	return best
}
