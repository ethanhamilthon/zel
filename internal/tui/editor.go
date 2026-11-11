package tui

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

type editorFinished struct {
	text, original, path string
	err                  error
}

// Only the saved file is imported; it is never submitted automatically.
func (m model) editInput() (tea.Model, tea.Cmd) {
	if m.editorActive {
		return m, nil
	}
	text := m.input.Value()
	m.withdrawSteer()
	m = m.closeMenu()
	file, err := os.CreateTemp("", "zel-input-*.md")
	if err != nil {
		return m.reject(err)
	}
	path := file.Name()
	if _, err = file.WriteString(text); err != nil {
		file.Close()
		os.Remove(path)
		return m.reject(err)
	}
	if err = file.Close(); err != nil {
		os.Remove(path)
		return m.reject(err)
	}
	editor := cmp.Or(m.ctl.Settings().UI.Editor, os.Getenv("VISUAL"), os.Getenv("EDITOR"), "nvim")
	command := exec.Command("sh", "-c", "exec "+editor+" \"$1\"", "zel-editor", path)
	command.Dir = m.cwd
	// Bypass cursorOutput: its Zel caret overlay would hide Neovim's cursor
	// and modify the editor's own alternate-screen and cursor sequences.
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	m.editorActive = true
	return m, tea.ExecProcess(command, func(err error) tea.Msg {
		result := editorFinished{original: text, path: path}
		if err != nil {
			result.err = fmt.Errorf("Editor failed: %w", err)
		} else {
			data, err := os.ReadFile(path)
			result.text, result.err = string(data), err
		}
		return result
	})
}
