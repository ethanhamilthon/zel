package luart

import (
	"fmt"
	"math"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// Type is a parsed parameter type: string integer number boolean table any,
// T[], {k:T}, "a"|"b" literals and unions; a trailing ? makes it optional.
type Type struct {
	Kind     string // name | array | map | literal | union
	Name     string // basic name or literal text
	Elem     *Type
	Alts     []*Type
	Optional bool
	Raw      string // source text without the optional mark
}

var basicTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "table": true, "any": true}

// ParseType parses a type expression.
func ParseType(src string) (*Type, error) {
	src = strings.TrimSpace(src)
	optional := false
	if strings.HasSuffix(src, "?") {
		optional, src = true, strings.TrimSpace(strings.TrimSuffix(src, "?"))
	}
	t, err := parseUnion(src)
	if err != nil {
		return nil, err
	}
	t.Optional = optional
	t.Raw = src
	return t, nil
}

func splitTop(s, sep string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '{':
			depth++
		case c == '}':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			parts = append(parts, s[start:i])
			start = i + len(sep)
		}
	}
	return append(parts, s[start:])
}

func parseUnion(s string) (*Type, error) {
	parts := splitTop(s, "|")
	if len(parts) == 1 {
		return parseSingle(strings.TrimSpace(parts[0]))
	}
	u := &Type{Kind: "union"}
	for _, p := range parts {
		alt, err := parseSingle(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		u.Alts = append(u.Alts, alt)
	}
	return u, nil
}

func parseSingle(s string) (*Type, error) {
	switch {
	case s == "":
		return nil, fmt.Errorf("empty type")
	case len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0]:
		return &Type{Kind: "literal", Name: s[1 : len(s)-1]}, nil
	case strings.HasSuffix(s, "[]"):
		elem, err := parseUnion(strings.TrimSuffix(s, "[]"))
		if err != nil {
			return nil, err
		}
		return &Type{Kind: "array", Elem: elem}, nil
	case s[0] == '{' && s[len(s)-1] == '}':
		inner := s[1 : len(s)-1]
		key, val, ok := strings.Cut(inner, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("bad map type %q (want {k:T})", s)
		}
		elem, err := parseUnion(strings.TrimSpace(val))
		if err != nil {
			return nil, err
		}
		return &Type{Kind: "map", Elem: elem}, nil
	case basicTypes[s]:
		return &Type{Kind: "name", Name: s}, nil
	}
	return nil, fmt.Errorf("unknown type %q", s)
}

func (t *Type) String() string {
	if t.Raw != "" {
		return t.Raw
	}
	switch t.Kind {
	case "literal":
		return fmt.Sprintf("%q", t.Name)
	case "array":
		return t.Elem.String() + "[]"
	case "map":
		return "{k:" + t.Elem.String() + "}"
	case "union":
		var parts []string
		for _, a := range t.Alts {
			parts = append(parts, a.String())
		}
		return strings.Join(parts, "|")
	}
	return t.Name
}

func luaTypeName(v lua.LValue) string { return v.Type().String() }

// Check reports why v is not a t, or "" when it is.
func (t *Type) Check(v lua.LValue) string {
	switch t.Kind {
	case "name":
		ok := false
		switch t.Name {
		case "any":
			ok = v != lua.LNil
		case "string":
			_, ok = v.(lua.LString)
		case "boolean":
			_, ok = v.(lua.LBool)
		case "number":
			_, ok = v.(lua.LNumber)
		case "integer":
			n, isNum := v.(lua.LNumber)
			ok = isNum && float64(n) == math.Trunc(float64(n))
		case "table":
			_, ok = v.(*lua.LTable)
		}
		if !ok {
			return fmt.Sprintf("expected %s, got %s", t.Name, luaTypeName(v))
		}
	case "literal":
		if s, ok := v.(lua.LString); !ok || string(s) != t.Name {
			return fmt.Sprintf("expected %q, got %s", t.Name, describe(v))
		}
	case "array":
		tbl, ok := v.(*lua.LTable)
		if !ok || !isArray(tbl) {
			return fmt.Sprintf("expected %s, got %s", t, luaTypeName(v))
		}
		for i := 1; i <= tbl.Len(); i++ {
			if msg := t.Elem.Check(tbl.RawGetInt(i)); msg != "" {
				return fmt.Sprintf("[%d]: %s", i, msg)
			}
		}
	case "map":
		tbl, ok := v.(*lua.LTable)
		if !ok {
			return fmt.Sprintf("expected %s, got %s", t, luaTypeName(v))
		}
		msg := ""
		tbl.ForEach(func(k, val lua.LValue) {
			if msg == "" {
				if m := t.Elem.Check(val); m != "" {
					msg = fmt.Sprintf("[%s]: %s", k, m)
				}
			}
		})
		return msg
	case "union":
		for _, a := range t.Alts {
			if a.Check(v) == "" {
				return ""
			}
		}
		return fmt.Sprintf("expected %s, got %s", t, describe(v))
	}
	return ""
}

func describe(v lua.LValue) string {
	if s, ok := v.(lua.LString); ok {
		return fmt.Sprintf("%q", string(s))
	}
	return luaTypeName(v)
}
