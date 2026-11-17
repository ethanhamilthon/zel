package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetKey sets the top-level string key (provider, model, effort) in the YAML
// file at path and keeps everything else, comments included. A missing key is
// added after the last top-level key before the first mapping section; a
// missing file is created.
func SetKey(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if text := strings.TrimSuffix(string(data), "\n"); text != "" {
		lines = strings.Split(text, "\n")
	}
	line := key + ": " + quote(value)
	lines = setTopLevel(lines, key, line)
	content := strings.Join(lines, "\n") + "\n"
	if _, _, err := decodeYAML(path, []byte(content)); err != nil {
		return fmt.Errorf("%s: the edit would make the file invalid: %w", path, err)
	}
	return writeAtomic(path, []byte(content))
}

// setTopLevel replaces the lines of key, or inserts line after the last
// top-level key before the first multi-line mapping section.
func setTopLevel(lines []string, key, line string) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return append(lines, line)
	}
	root := doc.Content[0]
	last, header := -1, len(lines)
	for i := 0; i+1 < len(root.Content); i += 2 {
		kn, vn := root.Content[i], root.Content[i+1]
		start := kn.Line - 1
		limit := len(lines)
		if i+2 < len(root.Content) {
			limit = root.Content[i+2].Line - 1
		}
		end := start
		for j := limit - 1; j > start; j-- {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" || strings.HasPrefix(lines[j], "#") {
				continue
			}
			end = j
			break
		}
		if kn.Value == key {
			return append(lines[:start:start], append([]string{line}, lines[end+1:]...)...)
		}
		if vn.Kind == yaml.MappingNode && vn.Style != yaml.FlowStyle && end > start {
			if header == len(lines) {
				header = start
			}
			continue
		}
		if header == len(lines) {
			last = end
		}
	}
	switch {
	case last >= 0:
		return append(lines[:last+1:last+1], append([]string{line}, lines[last+1:]...)...)
	case header < len(lines):
		return append(lines[:header:header], append([]string{line, ""}, lines[header:]...)...)
	}
	return append(lines, line)
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
	f, err := os.CreateTemp(filepath.Dir(path), ".settings.yaml-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly after the rename
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

// quote encodes s as a double-quoted YAML string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
