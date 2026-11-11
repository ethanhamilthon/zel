package config

import (
	"os"
	"path/filepath"
)

func ZelHome(cwd string) (string, error) {
	dir := os.Getenv("ZEL_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".zel")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

// DBPath is the SQLite state path for helper commands that run without an agent.
func DBPath(home string) string { return filepath.Join(home, "zel.db") }
