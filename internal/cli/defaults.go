package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"zel/internal/core"
	"zel/internal/defaults"
)

func defaultsCommand(args []string) error {
	if len(args) != 1 || args[0] != "--reset" {
		return usageError("defaults", "defaults --reset")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := core.ZelHome(cwd)
	if err != nil {
		return err
	}
	return resetDefaults(home, defaults.Files())
}

func resetDefaults(home string, files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("zel defaults: EFAIL: this build contains no embedded defaults; nothing was reset")
	}
	for path := range files {
		if !filepath.IsLocal(path) || filepath.Clean(path) != path || path == "." {
			return fmt.Errorf("zel defaults: invalid embedded path %q", path)
		}
	}
	dir := filepath.Join(home, "defaults")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for path, text := range files {
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			return err
		}
	}
	return nil
}
