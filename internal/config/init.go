package config

import (
	"errors"
	"os"
	"path/filepath"
)

// InitProject creates local instructions without replacing an existing file.
func InitProject(dir string) (created bool, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, "ZEL.md")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			created = false
			_ = os.Remove(path)
		}
	}()
	instructions, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	_, err = file.Write(instructions)
	return err == nil, err
}
