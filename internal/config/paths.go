package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath expands a leading home marker using the current user's actual home.
func ResolvePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// DisplayPath shortens absolute paths under the current user's home to ~ paths.
func DisplayPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "~"
	}
	return "~/" + filepath.ToSlash(rel)
}

func ZelHome(cwd string) (string, error) {
	dir := os.Getenv("ZEL_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "zel")
	} else {
		var err error
		dir, err = ResolvePath(dir)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

// File is the config file of a Zel home.
func File(home string) string { return filepath.Join(home, "settings.toml") }

const defaultConfig = `# Zel settings. Edits apply by themselves. Reference: zel docs config
# Snippets live beside this file: hooks/*.toml, prompts/*, instructions/*.
# [keys] binds UI actions, e.g. menu = "alt+k"; see zel docs config.
#
# model  = "openai/gpt-5"        # PROVIDER/MODEL
#
# [provider.openai]
# base_url = "https://api.openai.com/v1"
# api_key  = "${{ echo $OPENAI_API_KEY }}"
`

// EnsureHome creates the Zel home with the default settings.toml and a
// .gitignore that keeps secrets and sessions out of git. Existing files stay.
func EnsureHome(home string) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	for _, dir := range []string{"hooks", "prompts", "instructions"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			return err
		}
	}
	for name, text := range map[string]string{"settings.toml": defaultConfig, ".gitignore": ".env\n.sessions/\n"} {
		f, err := os.OpenFile(filepath.Join(home, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
