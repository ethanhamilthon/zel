package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pager is a vertical viewport over prepared rows. Unlike the bubbles viewport it
// takes the rows as they are, so a refresh does not split or measure the whole
// timeline again.
type pager struct {
	Width, Height, YOffset int
	TopMargin              int
	lines                  []string
}

func (p *pager) SetLines(lines []string) {
	p.lines = lines
	if p.YOffset > len(lines)-1 {
		p.GotoBottom()
	}
}

func (p pager) topMargin() int     { return min(p.TopMargin, max(0, p.Height-1)) }
func (p pager) contentHeight() int { return max(1, p.Height-p.topMargin()) }
func (p pager) maxYOffset() int    { return max(0, len(p.lines)-p.contentHeight()) }

func (p pager) AtBottom() bool { return p.YOffset >= p.maxYOffset() }

func (p *pager) SetYOffset(n int) { p.YOffset = min(max(0, n), p.maxYOffset()) }

func (p *pager) GotoTop() { p.YOffset = 0 }

func (p *pager) GotoBottom() { p.SetYOffset(p.maxYOffset()) }

func (p *pager) ScrollUp(n int) { p.SetYOffset(p.YOffset - n) }

func (p *pager) ScrollDown(n int) { p.SetYOffset(p.YOffset + n) }

func (p pager) Update(msg tea.Msg) (pager, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		switch v.String() {
		case "pgdown":
			p.ScrollDown(p.contentHeight())
		case "pgup":
			p.ScrollUp(p.contentHeight())
		}
	case tea.MouseMsg:
		if v.Action != tea.MouseActionPress {
			break
		}
		switch v.Button {
		case tea.MouseButtonWheelDown:
			p.ScrollDown(3)
		case tea.MouseButtonWheelUp:
			p.ScrollUp(3)
		}
	}
	return p, nil
}

func (p pager) View() string {
	top := min(max(0, p.YOffset), len(p.lines))
	visible := append(make([]string, p.topMargin()), p.lines[top:min(len(p.lines), top+p.contentHeight())]...)
	return lipgloss.NewStyle().Width(p.Width).Height(p.Height).MaxWidth(p.Width).MaxHeight(p.Height).Render(strings.Join(visible, "\n"))
}
