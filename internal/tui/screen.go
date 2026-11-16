package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"github.com/muesli/termenv"
	"zel/internal/config"
)

var (
	bgColor, accentColor, agentColor, userColor, userHighlight, grayColor, darkGray, errorColor lipgloss.Color
	traceColors                                                                                 map[string]lipgloss.Color
	markdownStyle                                                                               string
)

func init() { lipgloss.SetColorProfile(termenv.TrueColor); applyTheme(config.DefaultColors()) }
func style(color lipgloss.Color) tcell.Style {
	return tcell.StyleDefault.Foreground(tcell.GetColor(string(color))).Background(tcell.GetColor(string(bgColor)))
}

type span struct {
	text  string
	width int
	style tcell.Style
	link  string
}
type row []span

// Glamour only formats Markdown. All terminal state and drawing belong to tcell.
// DecodeSequence yields graphemes and ANSI metadata, never raw terminal output.
func styledRows(text string, width int, base tcell.Style) []row {
	width = max(1, width)
	rows := []row{{}}
	x, runWidth := 0, 0
	s := base
	link := ""
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			rows[len(rows)-1] = append(rows[len(rows)-1], span{run.String(), runWidth, s, link})
			run.Reset()
			runWidth = 0
		}
	}
	newline := func() { flush(); rows = append(rows, row{}); x = 0 }
	p := ansi.NewParser()
	var state byte
	for len(text) > 0 {
		seq, w, n, next := ansi.DecodeSequence(text, state, p)
		if n == 0 {
			break
		}
		state = next
		text = text[n:]
		switch {
		case strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m"):
			flush()
			s = sgrStyle(s, base, seq[2:len(seq)-1]).Url(link)
		case strings.HasPrefix(seq, "\x1b]8;"):
			flush()
			data := strings.TrimSuffix(strings.TrimSuffix(seq, "\x1b\\"), "\a")
			if _, url, ok := strings.Cut(data[4:], ";"); ok {
				link = url
				s = s.Url(url)
			}
		case seq == "\n":
			newline()
		case seq == "\t":
			n := min(4-x%4, width-x)
			run.WriteString(strings.Repeat(" ", n))
			x += n
			runWidth += n
		case w == 0 && seq != "" && seq[0] >= 0x20 && seq[0] != 0x7f:
			run.WriteString(seq)
		case w > 0:
			if x+w > width && x > 0 {
				newline()
			}
			if w > width {
				seq = "�"
				w = 1
			}
			run.WriteString(seq)
			x += w
			runWidth += w
		}
	}
	flush()
	return rows
}
func sgrStyle(s, base tcell.Style, params string) tcell.Style {
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		n, _ := strconv.Atoi(parts[i])
		fg, bg, _ := base.Decompose()
		switch {
		case n == 0:
			s = base
		case n == 1:
			s = s.Bold(true)
		case n == 2:
			s = s.Dim(true)
		case n == 3:
			s = s.Italic(true)
		case n == 4:
			s = s.Underline(true)
		case n == 7:
			s = s.Reverse(true)
		case n == 9:
			s = s.StrikeThrough(true)
		case n == 22:
			s = s.Bold(false).Dim(false)
		case n == 23:
			s = s.Italic(false)
		case n == 24:
			s = s.Underline(false)
		case n == 27:
			s = s.Reverse(false)
		case n == 29:
			s = s.StrikeThrough(false)
		case n == 39:
			s = s.Foreground(fg)
		case n == 49:
			s = s.Background(bg)
		case n >= 30 && n <= 37:
			s = s.Foreground(tcell.PaletteColor(n - 30))
		case n >= 40 && n <= 47:
			s = s.Background(tcell.PaletteColor(n - 40))
		case n >= 90 && n <= 97:
			s = s.Foreground(tcell.PaletteColor(n - 90 + 8))
		case n >= 100 && n <= 107:
			s = s.Background(tcell.PaletteColor(n - 100 + 8))
		case (n == 38 || n == 48) && i+2 < len(parts):
			mode, _ := strconv.Atoi(parts[i+1])
			c := tcell.ColorDefault
			if mode == 5 {
				v, _ := strconv.Atoi(parts[i+2])
				c = tcell.PaletteColor(v)
				i += 2
			} else if mode == 2 && i+4 < len(parts) {
				r, _ := strconv.Atoi(parts[i+2])
				g, _ := strconv.Atoi(parts[i+3])
				b, _ := strconv.Atoi(parts[i+4])
				c = tcell.NewRGBColor(int32(r), int32(g), int32(b))
				i += 4
			}
			if n == 38 {
				s = s.Foreground(c)
			} else {
				s = s.Background(c)
			}
		}
	}
	return s
}
func putSpan(screen tcell.Screen, x, y int, c span) {
	text := c.text
	for len(text) > 0 {
		rest, w := screen.Put(x, y, text, c.style)
		if len(rest) >= len(text) {
			break
		}
		x += w
		text = rest
	}
}
func drawRow(screen tcell.Screen, y int, r row, width int) {
	x := 0
	for _, c := range r {
		if x+c.width > width {
			break
		}
		putSpan(screen, x, y, c)
		x += c.width
	}
}
func displayText(text string) string {
	const limit = 1 << 20
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n… %d bytes not shown (full text is saved)", len(text)-cut)
}
func plainRows(text string, width int, color lipgloss.Color) []row {
	return styledRows(cleanInput(displayText(text)), width, style(color))
}
func rowText(r row) string {
	var b strings.Builder
	for _, c := range r {
		b.WriteString(c.text)
	}
	return b.String()
}
