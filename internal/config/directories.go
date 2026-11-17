package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// loadDirectories loads sorted snippets beside settings.toml. Markdown files
// are shorthand for a prompt or instruction named after the file.
func (p *parser) loadDirectories(path string) error {
	for _, group := range []struct{ dir, table string }{{"hooks", "hook"}, {"prompts", "completion"}, {"instructions", "instruction"}} {
		files, err := os.ReadDir(filepath.Join(filepath.Dir(path), group.dir))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
				continue
			}
			ext := filepath.Ext(file.Name())
			if ext != ".toml" && !(ext == ".md" && group.dir != "hooks") {
				continue
			}
			name := strings.TrimSuffix(file.Name(), ext)
			full := filepath.Join(filepath.Dir(path), group.dir, file.Name())
			text, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			var data map[string]any
			var md toml.MetaData
			if ext == ".md" {
				var encoded strings.Builder
				err = toml.NewEncoder(&encoded).Encode(map[string]any{group.table: map[string]string{name: string(text)}})
				if err == nil {
					md, err = toml.Decode(encoded.String(), &data)
				}
			} else {
				md, err = toml.Decode(string(text), &data)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", full, err)
			}
			for key := range data {
				if key != group.table {
					return fmt.Errorf("%s: only [%s] entries are allowed", full, group.table)
				}
			}
			entries, ok := data[group.table].(map[string]any)
			if !ok {
				return fmt.Errorf("%s: expected [%s] entries", full, group.table)
			}
			for name := range entries {
				duplicate := false
				switch group.table {
				case "completion":
					_, duplicate = p.cfg.Completions[name]
				case "instruction":
					for _, in := range p.cfg.Instructions {
						duplicate = duplicate || in.Name == name
					}
				case "hook":
					for _, hooks := range p.cfg.Hooks {
						for _, h := range hooks {
							duplicate = duplicate || h.Name == name
						}
					}
				}
				if duplicate {
					return fmt.Errorf("%s: duplicate %s %q", full, group.table, name)
				}
			}
			q := &parser{md: &md, cfg: p.cfg}
			if err := q.file(data); err != nil {
				return fmt.Errorf("%s: %w", full, err)
			}
		}
	}
	return nil
}
