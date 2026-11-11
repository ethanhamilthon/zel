package tui

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type fileItem struct {
	name string
	dir  bool
}
type fileEditorFinished struct{ err error }

func (m *model) readDirectory(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		m.menu.status = err.Error()
		m.ctl.LogEvent("error", err.Error())
		return
	}
	items := []fileItem{{".", true}, {"..", true}}
	var files []fileItem
	for _, e := range entries {
		dir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(path, e.Name())); err == nil {
				dir = info.IsDir()
			}
		}
		files = append(files, fileItem{e.Name(), dir})
	}
	slices.SortFunc(files, func(a, b fileItem) int {
		if a.dir != b.dir {
			if a.dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	m.menu.files = append(items, files...)
	m.menu.directory, m.menu.selected, m.menu.query, m.menu.search, m.menu.status = filepath.Clean(path), 0, "", nil, ""
}
func (m model) editFile(path string) (tea.Model, tea.Cmd) {
	info, err := os.Stat(path)
	if err != nil {
		return m.reject(err)
	}
	if !info.Mode().IsRegular() {
		return m.reject(fmt.Errorf("Not a regular file: %s", path))
	}
	editor := cmp.Or(m.ctl.Settings().UI.Editor, os.Getenv("VISUAL"), os.Getenv("EDITOR"), "nvim")
	command := exec.Command("sh", "-c", "exec "+editor+" \"$1\"", "zel-editor", path)
	command.Dir = m.cwd
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	m.editorActive = true
	return m, tea.ExecProcess(command, func(err error) tea.Msg { return fileEditorFinished{err} })
}
