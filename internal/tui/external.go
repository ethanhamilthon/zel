package tui

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"zel/internal/config"
)

// openEditor hands the composer text to the configured editor (Ctrl+E). The
// screen is suspended while the editor owns the terminal; the session keeps
// working, and the UI draws again when the editor exits. The saved text
// replaces the composer; an editor that fails leaves it as it was.
func (m *app) openEditor() {
	f, err := os.CreateTemp("", "zel-input-*.md")
	if err != nil {
		m.notice(err)
		return
	}
	path := f.Name()
	_, err = f.WriteString(m.input.text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		m.notice(err)
		return
	}
	editor := cmp.Or(strings.TrimSpace(m.ctl.Settings().Editor), config.Editor)
	if err := m.screen.Suspend(); err != nil {
		os.Remove(path)
		m.notice(err)
		return
	}
	m.editing = true
	before := m.input.text
	go func() {
		defer os.Remove(path)
		cmd := exec.Command("sh", "-c", editor+` "$1"`, "zel-editor", path)
		cmd.Dir = m.cwd
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		var text string
		if err == nil {
			var data []byte
			data, err = os.ReadFile(path)
			text = strings.TrimRight(string(data), "\n")
		} else {
			err = fmt.Errorf("editor %q: %w; the input is kept", editor, err)
		}
		m.post(result{kind: "editor", value: [2]string{before, text}, err: err})
	}()
}

// editorDone takes the screen back and puts the edited text in the composer.
func (m *app) editorDone(r result) {
	m.editing = false
	if err := m.screen.Resume(); err != nil {
		m.failure = err
		m.cancel()
		return
	}
	m.screen.Sync()
	if r.err != nil {
		m.notice(r.err)
		return
	}
	texts := r.value.([2]string)
	if texts[1] == m.input.text || texts[0] != m.input.text {
		return
	}
	m.input.set(texts[1])
	m.composerChanged()
	m.updateCompletions()
}
