package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// SetKey sets the dotted key ("model", "settings.accent", "env.FOO",
// "providers.openai.base_url") to value (string, bool or int) in the TOML file
// at path, keeping its comments and formatting. A missing file is created.
func SetKey(path, key string, value any) error {
	val, err := encodeValue(value)
	if err != nil {
		return err
	}
	segs, err := splitKey(key)
	if err != nil {
		return err
	}
	table, name := segs[:len(segs)-1], segs[len(segs)-1]
	return editFile(path, func(lines []string, info []tomlLine) ([]string, error) {
		header, last, first := -1, -1, -1
		for i, l := range info {
			switch l.kind {
			case lineHeader:
				if first < 0 {
					first = i
				}
				if slices.Equal(l.table, table) {
					if l.arr {
						return nil, byHand(key, "is in an array of tables")
					}
					header, last = i, i
				}
			case lineKey:
				if l.arr {
					continue
				}
				full := l.full()
				switch {
				case slices.Equal(full, segs):
					if len(l.key) > 1 {
						return nil, byHand(key, "is set with a dotted key")
					}
					lines[i] = lines[i][:l.valStart] + val + lines[l.end][l.valEnd:]
					return slices.Delete(lines, i+1, l.end+1), nil
				case hasPrefix(segs, full):
					return nil, byHand(key, "is inside an inline table")
				case len(l.table) < len(table) && hasPrefix(full, table):
					return nil, byHand(key, "belongs to a table defined with dotted keys")
				case slices.Equal(l.table, table):
					last = l.end
				}
			}
		}
		line := encodeKey(name) + " = " + val
		switch {
		case len(table) > 0 && header < 0:
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
			return append(lines, "["+joinKey(table)+"]", line), nil
		case last >= 0:
			return slices.Insert(lines, last+1, line), nil
		case first >= 0: // a top-level key in a file that starts with a table
			return slices.Insert(lines, first, line, ""), nil
		}
		return append(lines, line), nil
	})
}

func byHand(key, why string) error { return fmt.Errorf("%s %s: edit it by hand", key, why) }

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func hasPrefix(s, prefix []string) bool {
	return len(prefix) <= len(s) && slices.Equal(s[:len(prefix)], prefix)
}

func splitKey(key string) ([]string, error) {
	segs := strings.Split(key, ".")
	if slices.Contains(segs, "") {
		return nil, fmt.Errorf("invalid key %q", key)
	}
	return segs, nil
}

// editFile applies fn to the lines of the file and writes the result
// atomically if it is valid TOML. fn returns nil lines for "no change".
func editFile(path string, fn func([]string, []tomlLine) ([]string, error)) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if text := strings.TrimSuffix(string(data), "\n"); text != "" {
		lines = strings.Split(text, "\n")
	}
	out, err := fn(lines, parseLines(lines))
	if err != nil || out == nil {
		return err
	}
	content := strings.Join(out, "\n")
	if len(out) > 0 {
		content += "\n"
	}
	var check map[string]any
	if _, err := toml.Decode(content, &check); err != nil {
		return fmt.Errorf("%s: the edit would make the file invalid: %w", path, err)
	}
	return writeAtomic(path, []byte(content))
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
	f, err := os.CreateTemp(filepath.Dir(path), ".zel.toml-*")
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

func encodeValue(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return quote(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("unsupported value type %T", v)
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

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func encodeKey(s string) string {
	if bareKey.MatchString(s) {
		return s
	}
	return quote(s)
}

func joinKey(segs []string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = encodeKey(s)
	}
	return strings.Join(out, ".")
}

// Line kinds of the line-based TOML reader.
const (
	lineOther  = iota // blank, comment or unparsable
	lineHeader        // [table] or [[array]]
	lineKey           // key = value
	lineCont          // inside a multi-line value
)

type tomlLine struct {
	kind     int
	table    []string // the table the line belongs to (headers: their own path)
	key      []string // key lines: the (dotted) key
	arr      bool     // in or at an array of tables
	end      int      // key lines: index of the line the value ends on
	valStart int      // key lines: offset of the value on the first line
	valEnd   int      // key lines: offset after the value on the last line
}

func (l tomlLine) full() []string { return append(slices.Clone(l.table), l.key...) }

// parseLines classifies the lines of a TOML document well enough for point edits.
func parseLines(lines []string) []tomlLine {
	info := make([]tomlLine, len(lines))
	var table []string
	arr := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		start := len(l) - len(strings.TrimLeft(l, " \t"))
		if start == len(l) || l[start] == '#' || l[start] == '\r' {
			continue
		}
		if l[start] == '[' {
			a := strings.HasPrefix(l[start:], "[[")
			off := start + 1
			if a {
				off++
			}
			segs, j, ok := parseKey(l, off)
			if ok && j < len(l) && l[j] == ']' {
				table, arr = segs, a
				info[i] = tomlLine{kind: lineHeader, table: table, arr: arr}
			}
			continue
		}
		segs, j, ok := parseKey(l, start)
		if !ok || j >= len(l) || l[j] != '=' {
			continue
		}
		vs := skipBlank(l, j+1)
		var sc valueScan
		end, k := sc.line(l, vs), i
		for sc.open() && k+1 < len(lines) {
			k++
			info[k].kind = lineCont
			end = sc.line(lines[k], 0)
		}
		info[i] = tomlLine{kind: lineKey, table: table, key: segs, arr: arr, end: k, valStart: vs, valEnd: end}
		i = k
	}
	return info
}

func skipBlank(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// parseKey reads a dotted key (bare or quoted segments) of s at i and returns
// the offset after it and any following blanks.
func parseKey(s string, i int) ([]string, int, bool) {
	var segs []string
	for {
		i = skipBlank(s, i)
		if i >= len(s) {
			return nil, i, false
		}
		var seg string
		switch s[i] {
		case '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, i, false
			}
			var err error
			if seg, err = strconv.Unquote(s[i : j+1]); err != nil {
				seg = s[i+1 : j]
			}
			i = j + 1
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, i, false
			}
			seg, i = s[i+1:i+1+j], i+2+j
		default:
			j := i
			for j < len(s) && isBare(s[j]) {
				j++
			}
			if j == i {
				return nil, i, false
			}
			seg, i = s[i:j], j
		}
		segs = append(segs, seg)
		if i = skipBlank(s, i); i < len(s) && s[i] == '.' {
			i++
			continue
		}
		return segs, i, true
	}
}

// valueScan follows strings and brackets of a value across lines.
type valueScan struct {
	ml    string // delimiter of an open multi-line string
	depth int    // open [ and {
}

func (v *valueScan) open() bool { return v.ml != "" || v.depth > 0 }

// line scans s from i and returns where the value text ends on this line
// (before a comment and trailing blanks).
func (v *valueScan) line(s string, i int) int {
	end := len(s)
scan:
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
			end = i
			break scan
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
	return len(strings.TrimRight(s[:end], " \t\r"))
}
