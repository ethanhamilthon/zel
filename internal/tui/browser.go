package tui

import (
	"fmt"
	"os"
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

type directoryResult struct {
	generation uint64
	path       string
	files      []fileItem
	err        error
}

func (m *model) readDirectory(path string) tea.Cmd {
	m.directoryGen++
	generation := m.directoryGen
	m.menu.directory, m.menu.status = filepath.Clean(path), "Loading…"
	m.menu.files, m.menu.listCache = nil, nil
	m.menu.selected, m.menu.query, m.menu.search = 0, "", nil
	return func() tea.Msg {
		files, err := directoryFiles(path)
		return directoryResult{generation, path, files, err}
	}
}

func directoryFiles(path string) ([]fileItem, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
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
	return append([]fileItem{{".", true}, {"..", true}}, files...), nil
}

func (m *model) applyDirectory(v directoryResult) {
	if v.generation != m.directoryGen || m.menu == nil || m.menu.page != menuFiles {
		return
	}
	if v.err != nil {
		m.menu.status = v.err.Error()
		m.ctl.LogEvent("error", v.err.Error())
		return
	}
	m.menu.files, m.menu.listCache = v.files, nil
	m.menu.directory, m.menu.status = filepath.Clean(v.path), ""
}

func (m model) editFile(path string) (tea.Model, tea.Cmd) {
	info, err := os.Stat(path)
	if err != nil {
		return m.reject(err)
	}
	if !info.Mode().IsRegular() {
		return m.reject(fmt.Errorf("Not a regular file: %s", path))
	}
	command := m.editorCommand(path)
	m.editorActive = true
	return m, tea.ExecProcess(command, func(err error) tea.Msg { return fileEditorFinished{err} })
}
