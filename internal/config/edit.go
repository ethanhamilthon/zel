package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// keyLine matches "key = value" at the start of a line.
var keyLine = regexp.MustCompile(`^([A-Za-z0-9_-]+)[ \t]*=[ \t]*`)

// SetKey sets the top-level string key (provider, model, effort) in the TOML
// file at path and keeps everything else, comments included. A missing key is
// added after the last top-level key; a missing file is created.
func SetKey(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if text := strings.TrimSuffix(string(data), "\n"); text != "" {
		lines = strings.Split(text, "\n")
	}
	line := key + " = " + quote(value)
	lines = setTopLevel(lines, key, line)
	content := strings.Join(lines, "\n") + "\n"
	var check map[string]any
	if _, err := toml.Decode(content, &check); err != nil {
		return fmt.Errorf("%s: the edit would make the file invalid: %w", path, err)
	}
	return writeAtomic(path, []byte(content))
}

// setTopLevel replaces the line of key, or inserts line after the last
// top-level key. Top-level keys come before the first [table].
func setTopLevel(lines []string, key, line string) []string {
	var scan valueScan
	last, header := -1, len(lines)
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || l[0] == '#' {
			continue
		}
		if l[0] == '[' {
			header = i
			break
		}
		m := keyLine.FindStringSubmatchIndex(l)
		if m == nil {
			continue
		}
		start := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t")) + m[1]
		scan = valueScan{}
		end := i
		scan.line(lines[i], start)
		for scan.open() && end+1 < len(lines) {
			end++
			scan.line(lines[end], 0)
		}
		if l[m[2]:m[3]] == key {
			return append(lines[:i:i], append([]string{line}, lines[end+1:]...)...)
		}
		last, i = end, end
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
	f, err := os.CreateTemp(filepath.Dir(path), ".settings.toml-*")
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

// quote encodes s as a TOML basic string.
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

// valueScan follows strings and brackets of a value across lines.
type valueScan struct {
	ml    string // delimiter of an open multi-line string
	depth int    // open [ and {
}

func (v *valueScan) open() bool { return v.ml != "" || v.depth > 0 }

// line scans s from i, up to a comment.
func (v *valueScan) line(s string, i int) {
	for i < len(s) {
		if v.ml != "" {
			switch {
			case v.ml == `"""` && s[i] == '\\':
				i += 2
			case strings.HasPrefix(s[i:], v.ml):
				i += 3
				for n := 0; n < 2 && i < len(s) && s[i] == v.ml[0]; n++ { // quotes right before the closing ones
					i++
				}
				v.ml = ""
			default:
				i++
			}
			continue
		}
		switch c := s[i]; c {
		case '#':
			return
		case '"', '\'':
			if q := strings.Repeat(string(c), 3); strings.HasPrefix(s[i:], q) {
				v.ml, i = q, i+3
				continue
			}
			j := i + 1
			for j < len(s) && s[j] != c {
				if c == '"' && s[j] == '\\' {
					j++
				}
				j++
			}
			i = j + 1
		case '[', '{':
			v.depth++
			i++
		case ']', '}':
			v.depth--
			i++
		default:
			i++
		}
	}
}
