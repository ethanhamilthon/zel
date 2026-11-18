package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Connect holds the contents of connect.json.
type Connect struct {
	Provider  string              `json:"provider"`
	Model     string              `json:"model"`
	Effort    string              `json:"effort"`
	Providers map[string]Provider `json:"providers"`
}

// LoadConnect reads connect.json and expands $VAR / ${VAR} from the process
// environment ($$ for a literal $).
func LoadConnect(path string) (Connect, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Connect{}, err
	}
	var c Connect
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &c); err != nil {
			return Connect{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.Provider = expandEnv(c.Provider)
	c.Model = expandEnv(c.Model)
	c.Effort = expandEnv(c.Effort)
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	for name, p := range c.Providers {
		if !entryName.MatchString(name) {
			return Connect{}, fmt.Errorf("%s: invalid provider name %q", path, name)
		}
		p.BaseURL = expandEnv(p.BaseURL)
		p.APIKey = expandEnv(p.APIKey)
		p.API = expandEnv(p.API)
		c.Providers[name] = p
	}
	return c, nil
}

// SaveSelection writes provider, model and effort to connect.json while keeping
// all other keys intact, formatted as pretty JSON.
func SaveSelection(path, provider, model, effort string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	obj := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &obj); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	obj["provider"] = provider
	obj["model"] = model
	obj["effort"] = effort
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}

func expandEnv(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			if end := strings.IndexByte(s[i+2:], '}'); end > 0 {
				name := s[i+2 : i+2+end]
				if envName.MatchString(name) {
					b.WriteString(os.Getenv(name))
					i = i + 2 + end + 1
					continue
				}
			}
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 < len(s) && isEnvStart(s[i+1]) {
			j := i + 2
			for j < len(s) && isEnvCont(s[j]) {
				j++
			}
			b.WriteString(os.Getenv(s[i+1 : j]))
			i = j
			continue
		}
		b.WriteByte('$')
		i++
	}
	return b.String()
}

func isEnvStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isEnvCont(b byte) bool {
	return isEnvStart(b) || (b >= '0' && b <= '9')
}

func writeAtomic(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := fs.FileMode(0600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".connect.json-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
