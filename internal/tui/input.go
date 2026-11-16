package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"zel/internal/core"
)

type submission struct {
	draft, text string
	serial      int
}
type pasted struct {
	image, text string
	target      *editor
	draft       string
}

func (m *app) key(k *tcell.EventKey) {
	m.dirty = true
	if m.pasting {
		switch k.Key() {
		case tcell.KeyRune:
			m.paste.WriteRune(k.Rune())
		case tcell.KeyEnter, tcell.KeyCtrlJ:
			m.paste.WriteByte('\n')
		case tcell.KeyTab:
			m.paste.WriteByte('\t')
		}
		return
	}
	switch k.Key() {
	case tcell.KeyCtrlQ:
		m.cancelPrompt()
		closed, err := m.ctl.Quit()
		m.notice(err)
		if closed {
			m.cancel()
		}
		return
	case tcell.KeyCtrlD:
		m.cancelPrompt()
		m.ctl.Stop()
		return
	case tcell.KeyCtrlC:
		if m.selected != nil {
			m.copySelection()
		}
		return
	case tcell.KeyCtrlL:
		m.screen.Sync()
		return
	case tcell.KeyCtrlV:
		m.pasteClipboard()
		return
	}
	m.selected = nil
	if m.operation {
		return
	}
	if m.menu != nil {
		m.menuKey(k)
		return
	}
	if m.choice != nil {
		m.choiceKey(k)
		return
	}
	switch k.Key() {
	case tcell.KeyCtrlK:
		m.openMenu("main")
		return
	case tcell.KeyCtrlM:
		m.openMenu("models")
		return
	case tcell.KeyCtrlR:
		m.openMenu("sessions")
		return
	case tcell.KeyCtrlN:
		m.switchSession("")
		return
	case tcell.KeyCtrlI:
		m.openMenu("info")
		return
	case tcell.KeyCtrlT:
		m.openMenu("trace")
		return
	case tcell.KeyPgUp:
		m.scroll += max(1, m.chatHeight-2)
		return
	case tcell.KeyPgDn:
		m.scroll = max(0, m.scroll-max(1, m.chatHeight-2))
		return
	case tcell.KeyEscape:
		if m.promptCancel != nil {
			m.cancelPrompt()
		} else {
			m.completionHidden = m.input.text
			m.completions = list{}
			m.selected = nil
		}
		return
	case tcell.KeyTab, tcell.KeyBacktab:
		if len(m.completions.items) > 0 {
			m.pickCompletion()
			return
		}
	case tcell.KeyUp, tcell.KeyDown:
		if len(m.completions.items) > 0 {
			delta := 1
			if k.Key() == tcell.KeyUp {
				delta = -1
			}
			m.completions.move(delta)
			return
		}
		if k.Key() == tcell.KeyUp && m.input.text == "" {
			in, err := m.ctl.Recall()
			if err == nil {
				m.input.set(in.Text)
				m.queued = 0
			}
			return
		}
	case tcell.KeyEnter:
		// Ctrl+M and Enter are identical in legacy terminals. tcell reports a
		// distinguishable Ctrl+M only when the terminal's keyboard protocol does.
		if k.Modifiers()&(tcell.ModShift|tcell.ModAlt) != 0 {
			m.editKey(tcell.NewEventKey(tcell.KeyCtrlJ, 0, 0))
			return
		}
		if len(m.completions.items) > 0 {
			m.pickCompletion()
			return
		}
		m.submit()
		return
	}
	if k.Key() == tcell.KeyRune && k.Rune() == '/' && m.input.text == "" && m.ctl.Settings().UI.SlashMenu {
		m.openMenu("main")
		return
	}
	m.editKey(k)
}
func (m *app) editKey(k *tcell.EventKey) {
	width, _ := m.screen.Size()
	before := m.input.text
	m.input.key(k, max(1, width-2), true)
	if m.input.text != before {
		m.composerChanged()
	}
	m.updateCompletions()
}
func (m *app) composerChanged() {
	m.cancelPrompt()
	if m.queued != 0 {
		m.ctl.Withdraw(m.queued)
		m.queued = 0
		m.queuedText = ""
	}
	m.completionHidden = ""
	m.scroll = 0
}
func (m *app) cancelPrompt() {
	if m.promptCancel != nil {
		m.promptCancel()
		m.promptCancel = nil
		m.promptSerial++
	}
}
func (m *app) submit() {
	draft := m.input.text
	text := strings.TrimSpace(draft)
	if text == "" || m.queued != 0 || m.promptCancel != nil {
		return
	}
	if !m.ctl.HasPromptTokens(text) {
		m.deliver(text, draft)
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.promptCancel = cancel
	m.promptSerial++
	serial := m.promptSerial
	m.work("submit", func() (any, error) {
		out, err := m.ctl.ExpandPrompt(ctx, text)
		return submission{draft, out, serial}, err
	})
}
func (m *app) deliver(text, draft string) {
	m.serial++
	id := m.serial
	if err := m.ctl.Input(core.Input{Text: text, QueueID: id}); err != nil {
		m.notice(err)
		return
	}
	if slices.ContainsFunc(m.ctl.Status().Queued, func(in core.Input) bool { return in.QueueID == id }) {
		m.queued = id
		m.queuedText = draft
	} else {
		m.input.set("")
		m.pending = draft
	}
	m.resetCompletions()
	m.scroll = 0
}
func (m *app) syncQueued() {
	if m.queued == 0 {
		return
	}
	for _, in := range m.ctl.Status().Queued {
		if in.QueueID == m.queued {
			return
		}
	}
	if m.input.text == m.queuedText {
		m.input.set("")
	}
	if m.ctl.Status().Busy {
		m.pending = m.queuedText
	}
	m.queued = 0
	m.queuedText = ""
	m.resetCompletions()
}
func (m *app) choiceKey(k *tcell.EventKey) {
	width, _ := m.screen.Size()
	if m.writing {
		switch k.Key() {
		case tcell.KeyEscape:
			m.writing = false
		case tcell.KeyEnter:
			if k.Modifiers()&(tcell.ModAlt|tcell.ModShift) != 0 {
				m.answer.insert("\n")
				return
			}
			if text := strings.TrimSpace(m.answer.text); text != "" {
				m.keepChoice(m.ctl.AnswerChoiceText(m.choice.ID, text))
			}
		default:
			m.answer.key(k, max(1, width-2), true)
		}
		return
	}
	n := len(m.choice.Options) + 1
	switch k.Key() {
	case tcell.KeyUp, tcell.KeyBacktab:
		m.choiceIndex = (m.choiceIndex + n - 1) % n
	case tcell.KeyDown, tcell.KeyTab:
		m.choiceIndex = (m.choiceIndex + 1) % n
	case tcell.KeyEscape:
		m.keepChoice(m.ctl.AnswerChoice(m.choice.ID, -1))
	case tcell.KeyEnter:
		if m.choiceIndex == n-1 {
			m.writing = true
		} else {
			m.keepChoice(m.ctl.AnswerChoice(m.choice.ID, m.choiceIndex))
		}
	case tcell.KeyRune:
		if k.Rune() == 'j' {
			m.choiceIndex = (m.choiceIndex + 1) % n
			return
		}
		if k.Rune() == 'k' {
			m.choiceIndex = (m.choiceIndex + n - 1) % n
			return
		}
		if k.Rune() >= '1' && k.Rune() <= '9' && int(k.Rune()-'1') < len(m.choice.Options) {
			m.keepChoice(m.ctl.AnswerChoice(m.choice.ID, int(k.Rune()-'1')))
		}
	}
}
func (m *app) keepChoice(r core.Record, ok bool) {
	if ok {
		m.records = append(m.records, r)
	}
	m.syncChoice()
	m.scroll = 0
}
func (m *app) pasteClipboard() {
	if m.clipboardBusy {
		return
	}
	m.clipboardBusy = true
	target := m.activeEditor()
	draft := target.text
	directory := m.ctl.AttachmentsDir()
	if directory == "" {
		directory = filepath.Join(m.runtime.Home(), "attachments")
	}
	m.work("clipboard", func() (any, error) {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
		defer cancel()
		image, text, err := readClipboard(ctx, directory)
		return pasted{image, text, target, draft}, err
	})
}
func (m *app) accept(r result) {
	m.dirty = true
	if r.kind == "choice" {
		m.syncChoice()
		return
	}
	if r.kind == "tasks" {
		m.tasks = m.ctl.ActiveTasks()
		return
	}
	if r.generation != m.generation && r.kind != "operation" {
		if p, ok := r.value.(pasted); ok && p.image != "" {
			os.Remove(p.image)
		}
		return
	}
	switch r.kind {
	case "submit":
		in := r.value.(submission)
		if in.serial != m.promptSerial {
			return
		}
		m.cancelPrompt()
		if r.err != nil {
			m.notice(fmt.Errorf("prompt not sent: %w; your draft is kept", r.err))
			return
		}
		if in.draft != m.input.text {
			return
		}
		m.deliver(in.text, in.draft)
	case "clipboard":
		m.clipboardBusy = false
		p := r.value.(pasted)
		if p.target != m.activeEditor() || p.draft != p.target.text {
			if p.image != "" {
				os.Remove(p.image)
			}
			return
		}
		if r.err != nil {
			m.notice(r.err)
			return
		}
		if p.image != "" {
			p.text = p.image + " "
		}
		p.target.insert(p.text)
		if p.target == &m.input {
			m.composerChanged()
			m.updateCompletions()
		}
	case "files":
		m.fileLoading = false
		loaded := r.value.(fileResult)
		if loaded.root != m.fileRoot {
			m.updateCompletions()
			return
		}
		m.fileIndex = loaded.files
		m.fileAt = time.Now()
		m.updateCompletions()
	default:
		m.acceptMenu(r)
	}
}

func runeOffset(s string, bytes int) int { return utf8.RuneCountInString(s[:bytes]) }
func byteOffset(s string, runes int) int {
	for i := range s {
		if runes == 0 {
			return i
		}
		runes--
	}
	return len(s)
}
