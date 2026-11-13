package luart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	lua "github.com/yuin/gopher-lua"
)

const maxDepth = 64

// toGo converts a Lua value to a JSON-compatible Go value. Tables with the keys
// 1..n (and the empty table) become slices, other tables objects.
func toGo(v lua.LValue) (any, error) { return toGoDepth(v, 0) }

func toGoDepth(v lua.LValue, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nested too deeply (or cyclic)")
	}
	switch x := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(x), nil
	case lua.LNumber:
		f := float64(x)
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f), nil
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("cannot encode %v", f)
		}
		return f, nil
	case lua.LString:
		return string(x), nil
	case *lua.LTable:
		if isArray(x) {
			out := make([]any, 0, x.Len())
			for i := 1; i <= x.Len(); i++ {
				item, err := toGoDepth(x.RawGetInt(i), depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, item)
			}
			return out, nil
		}
		out := map[string]any{}
		var failure error
		x.ForEach(func(k, val lua.LValue) {
			if failure != nil {
				return
			}
			var key string
			switch kk := k.(type) {
			case lua.LString:
				key = string(kk)
			case lua.LNumber:
				key = strconv.FormatFloat(float64(kk), 'f', -1, 64)
			default:
				failure = fmt.Errorf("cannot encode a %s table key", k.Type())
				return
			}
			item, err := toGoDepth(val, depth+1)
			if err != nil {
				failure = err
				return
			}
			out[key] = item
		})
		return out, failure
	default:
		return nil, fmt.Errorf("cannot encode a %s value", v.Type())
	}
}

func isArray(t *lua.LTable) bool {
	n := t.Len()
	count := 0
	t.ForEach(func(lua.LValue, lua.LValue) { count++ })
	if count != n {
		return false
	}
	for i := 1; i <= n; i++ {
		if t.RawGetInt(i) == lua.LNil {
			return false
		}
	}
	return true
}

// toLua converts a JSON-compatible Go value to a Lua value.
func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case float64:
		return lua.LNumber(x)
	case json.Number:
		f, _ := x.Float64()
		return lua.LNumber(f)
	case string:
		return lua.LString(x)
	case []string:
		t := L.CreateTable(len(x), 0)
		for _, s := range x {
			t.Append(lua.LString(s))
		}
		return t
	case []any:
		t := L.CreateTable(len(x), 0)
		for _, item := range x {
			t.Append(toLua(L, item))
		}
		return t
	case map[string]any:
		t := L.CreateTable(0, len(x))
		for k, item := range x {
			t.RawSetString(k, toLua(L, item))
		}
		return t
	case map[string]string:
		t := L.CreateTable(0, len(x))
		for k, item := range x {
			t.RawSetString(k, lua.LString(item))
		}
		return t
	case lua.LValue:
		return x
	default:
		// Anything else goes through JSON.
		data, err := json.Marshal(v)
		if err != nil {
			return lua.LString(fmt.Sprint(v))
		}
		var generic any
		if json.Unmarshal(data, &generic) != nil {
			return lua.LString(fmt.Sprint(v))
		}
		return toLua(L, generic)
	}
}

// encodeJSON renders a Lua value as JSON; indent is empty for the compact form.
func encodeJSON(v lua.LValue, indent string) (string, error) {
	g, err := toGo(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(g); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func decodeJSON(L *lua.LState, s string) (lua.LValue, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return toLua(L, v), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
