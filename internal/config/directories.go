package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// loadDirectories loads sorted snippets beside settings.yaml: hooks/*.yaml,
// prompts/*.md and systems/*.md.
func (p *parser) loadDirectories(path string) error {
	base := filepath.Dir(path)
	if err := p.loadHookFiles(filepath.Join(base, "hooks")); err != nil {
		return err
	}
	if err := p.loadMarkdownDir(filepath.Join(base, "prompts"), "completion"); err != nil {
		return err
	}
	return p.loadMarkdownDir(filepath.Join(base, "systems"), "system")
}

func (p *parser) loadHookFiles(dir string) error {
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") || filepath.Ext(file.Name()) != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(file.Name(), ".yaml")
		full := filepath.Join(dir, file.Name())
		if !entryName.MatchString(name) {
			return fmt.Errorf("%s: invalid hook name %q", full, name)
		}
		for _, hooks := range p.cfg.Hooks {
			for _, h := range hooks {
				if h.Name == name {
					return fmt.Errorf("%s: duplicate hook %q", full, name)
				}
			}
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		data, _, err := decodeYAML(full, raw)
		if err != nil {
			return err
		}
		if err := p.addHook(&fields{m: data, where: "hook." + name}, name); err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
	}
	return nil
}

func (p *parser) loadMarkdownDir(dir, kind string) error {
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") || filepath.Ext(file.Name()) != ".md" {
			continue
		}
		name := strings.TrimSuffix(file.Name(), ".md")
		full := filepath.Join(dir, file.Name())
		if !entryName.MatchString(name) {
			return fmt.Errorf("%s: invalid %s name %q", full, kind, name)
		}
		duplicate := false
		if kind == "completion" {
			_, duplicate = p.cfg.Completions[name]
		} else {
			duplicate = slices.ContainsFunc(p.cfg.Instructions, func(in Instruction) bool { return in.Name == name })
		}
		if duplicate {
			return fmt.Errorf("%s: duplicate %s %q", full, kind, name)
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		modes, body, err := parseMarkdown(full, string(raw))
		if err != nil {
			return err
		}
		if kind == "completion" {
			p.cfg.Completions[name] = Completion{Text: body, Modes: modes}
		} else {
			p.cfg.Instructions = append(p.cfg.Instructions, Instruction{Name: name, Text: body, Modes: modes})
		}
	}
	return nil
}

// parseMarkdown splits a .md file into its optional YAML frontmatter (modes)
// and its markdown body.
func parseMarkdown(path, text string) ([]string, string, error) {
	if text != "---" && !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return []string{ModeMain}, text, nil
	}
	if text == "---" {
		return nil, "", fmt.Errorf("%s: unclosed frontmatter ---", path)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(text, "---\r\n"), "---\n")
	var metaLines []string
	body := ""
	closed := false
	for rest != "" {
		line, after, hasNL := strings.Cut(rest, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			body = after
			closed = true
			break
		}
		metaLines = append(metaLines, line)
		if !hasNL {
			break
		}
		rest = after
	}
	if !closed {
		return nil, "", fmt.Errorf("%s: unclosed frontmatter ---", path)
	}
	data, _, err := decodeYAML(path, []byte(strings.Join(metaLines, "\n")))
	if err != nil {
		return nil, "", err
	}
	f := &fields{m: data, where: "frontmatter"}
	f.only("modes")
	modes := f.modes([]string{ModeMain})
	if f.err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, f.err)
	}
	return modes, body, nil
}
