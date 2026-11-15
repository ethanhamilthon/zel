package luart

import (
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// copier moves a Lua function with everything it captured from one state into
// another, by value: numbers, strings, booleans and nil as they are, tables
// deeply (keeping shared references and cycles), Lua functions recursively
// with fresh upvalues. Go functions (sh, string.format, plugin functions...)
// are found by their global name in the source and taken from the target.
// Anything else (userdata, coroutines, channels) cannot be copied.
type copier struct {
	src, dst *lua.LState
	tables   map[*lua.LTable]*lua.LTable
	fns      map[*lua.LFunction]*lua.LFunction
	uvs      map[*lua.Upvalue]*lua.Upvalue
	gnames   map[*lua.LFunction]string
}

func newCopier(src, dst *lua.LState) *copier {
	return &copier{src: src, dst: dst, tables: map[*lua.LTable]*lua.LTable{},
		fns: map[*lua.LFunction]*lua.LFunction{}, uvs: map[*lua.Upvalue]*lua.Upvalue{}}
}

// copyFunction copies fn into the target state.
func (c *copier) copyFunction(fn *lua.LFunction) (*lua.LFunction, error) {
	v, err := c.value(fn, "the function")
	if err != nil {
		return nil, err
	}
	return v.(*lua.LFunction), nil
}

func (c *copier) value(v lua.LValue, what string) (lua.LValue, error) {
	switch x := v.(type) {
	case *lua.LNilType, lua.LBool, lua.LNumber, lua.LString:
		return v, nil
	case *lua.LTable:
		return c.table(x, what)
	case *lua.LFunction:
		if x.IsG {
			return c.goFunction(x, what)
		}
		return c.function(x, what)
	}
	return nil, fmt.Errorf("cannot copy %s (a %s) into the task", what, v.Type())
}

func (c *copier) table(t *lua.LTable, what string) (lua.LValue, error) {
	if t == c.src.G.Global || t == c.src.Env {
		return c.dst.G.Global, nil
	}
	if done, ok := c.tables[t]; ok {
		return done, nil
	}
	out := c.dst.NewTable()
	c.tables[t] = out
	var err error
	t.ForEach(func(k, val lua.LValue) {
		if err != nil {
			return
		}
		name := what + "." + k.String()
		var ck, cv lua.LValue
		if ck, err = c.value(k, "a key of "+what); err != nil {
			return
		}
		if cv, err = c.value(val, name); err != nil {
			return
		}
		out.RawSet(ck, cv)
	})
	if err != nil {
		return nil, err
	}
	if mt, ok := c.src.GetMetatable(t).(*lua.LTable); ok {
		cmt, err := c.table(mt, "the metatable of "+what)
		if err != nil {
			return nil, err
		}
		c.dst.SetMetatable(out, cmt)
	}
	return out, nil
}

func (c *copier) function(f *lua.LFunction, what string) (lua.LValue, error) {
	if done, ok := c.fns[f]; ok {
		return done, nil
	}
	out := c.dst.NewFunctionFromProto(f.Proto)
	c.fns[f] = out
	if f.Env != nil && f.Env != c.src.G.Global && f.Env != c.src.Env {
		env, err := c.table(f.Env, "the environment of "+what)
		if err != nil {
			return nil, err
		}
		out.Env = env.(*lua.LTable)
	}
	for i, uv := range f.Upvalues {
		if uv == nil {
			continue
		}
		if done, ok := c.uvs[uv]; ok {
			out.Upvalues[i] = done
			continue
		}
		fresh := &lua.Upvalue{}
		c.uvs[uv] = fresh
		out.Upvalues[i] = fresh
		name := fmt.Sprintf("upvalue #%d", i+1)
		if i < len(f.Proto.DbgUpvalues) {
			name = "upvalue '" + f.Proto.DbgUpvalues[i] + "'"
		}
		val, err := c.value(uv.Value(), name)
		if err != nil {
			return nil, err
		}
		fresh.SetValue(val)
	}
	return out, nil
}

// goFunction maps a Go function to the one with the same global name in the
// target state ("sh", "read", "string.format", a tool from init.lua...).
func (c *copier) goFunction(f *lua.LFunction, what string) (lua.LValue, error) {
	if c.gnames == nil {
		c.gnames = map[*lua.LFunction]string{}
		c.src.G.Global.ForEach(func(k, v lua.LValue) {
			name, ok := k.(lua.LString)
			if !ok {
				return
			}
			switch x := v.(type) {
			case *lua.LFunction:
				if x.IsG {
					c.gnames[x] = string(name)
				}
			case *lua.LTable:
				x.ForEach(func(k2, v2 lua.LValue) {
					if fn, ok := v2.(*lua.LFunction); ok && fn.IsG {
						if s, ok := k2.(lua.LString); ok {
							c.gnames[fn] = string(name) + "." + string(s)
						}
					}
				})
			}
		})
	}
	name, ok := c.gnames[f]
	if !ok {
		return nil, fmt.Errorf("cannot copy %s (a Go function without a global name) into the task", what)
	}
	ns, fn, dotted := strings.Cut(name, ".")
	var found lua.LValue = c.dst.G.Global.RawGetString(ns)
	if dotted {
		t, ok := found.(*lua.LTable)
		if !ok {
			return nil, fmt.Errorf("cannot copy %s: %s is not available in tasks", what, name)
		}
		found = t.RawGetString(fn)
	}
	if g, ok := found.(*lua.LFunction); ok {
		return g, nil
	}
	return nil, fmt.Errorf("cannot copy %s: %s is not available in tasks", what, name)
}
