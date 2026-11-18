package luart

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// copier moves a Lua function with everything it captured from one state into
// another, by value: numbers, strings, booleans and nil as they are, tables
// deeply (keeping shared references and cycles), Lua functions recursively
// with fresh upvalues. Go functions (sh, string.format, plugin functions...)
// are found by their global name in the source and taken from the target.
// Anything else (userdata, coroutines, channels) cannot be copied.
type copier struct {
	src, dst       *lua.LState
	srcAPI, dstAPI *lua.LTable
	dataOnly       bool
	depth          int
	tables         map[*lua.LTable]*lua.LTable
	fns            map[*lua.LFunction]*lua.LFunction
	uvs            map[*lua.Upvalue]*lua.Upvalue
	gfuncs         map[*lua.LFunction]*lua.LFunction
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
	if c.depth >= 256 {
		return nil, fmt.Errorf("%s nested too deeply", what)
	}
	c.depth++
	defer func() { c.depth-- }()
	switch x := v.(type) {
	case *lua.LNilType, lua.LBool, lua.LNumber, lua.LString:
		return v, nil
	case *lua.LTable:
		return c.table(x, what)
	case *lua.LFunction:
		if c.dataOnly {
			break
		}
		if x.IsG {
			return c.goFunction(x, what)
		}
		return c.function(x, what)
	}
	return nil, fmt.Errorf("cannot copy %s (a %s) across Lua states", what, v.Type())
}

func (c *copier) table(t *lua.LTable, what string) (lua.LValue, error) {
	if !c.dataOnly {
		if t == c.src.G.Global || t == c.src.Env {
			return c.dst.G.Global, nil
		}
		if c.srcAPI != nil && t == c.srcAPI {
			return c.dstAPI, nil
		}
		for _, k := range []string{"session", "task"} {
			if c.srcAPI != nil && t == c.srcAPI.RawGetString(k) {
				return c.dstAPI.RawGetString(k), nil
			}
		}
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
	if mt, ok := t.Metatable.(*lua.LTable); ok && !c.dataOnly {
		cmt, err := c.value(mt, "the metatable of "+what)
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
		env, err := c.value(f.Env, "the environment of "+what)
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

// Match native functions by their corresponding standard-library or host
// binding, never by copying a Go closure that references the old VM.
func (c *copier) goFunction(f *lua.LFunction, what string) (lua.LValue, error) {
	if c.gfuncs == nil {
		c.gfuncs = map[*lua.LFunction]*lua.LFunction{}
		seen := map[[2]*lua.LTable]bool{}
		var match func(a, b *lua.LTable)
		match = func(a, b *lua.LTable) {
			pair := [2]*lua.LTable{a, b}
			if seen[pair] {
				return
			}
			seen[pair] = true
			a.ForEach(func(k, x lua.LValue) {
				if _, ok := k.(lua.LString); !ok {
					return
				}
				y := b.RawGet(k)
				switch x := x.(type) {
				case *lua.LFunction:
					if y, ok := y.(*lua.LFunction); x.IsG && ok && y.IsG {
						c.gfuncs[x] = y
					}
				case *lua.LTable:
					if y, ok := y.(*lua.LTable); ok {
						match(x, y)
					}
				}
			})
		}
		match(c.src.G.Global, c.dst.G.Global)
		if c.srcAPI != nil {
			match(c.srcAPI, c.dstAPI)
		}
	}
	if bound := c.gfuncs[f]; bound != nil {
		return bound, nil
	}
	return nil, fmt.Errorf("cannot copy %s (a native function without a corresponding binding)", what)
}

func cloneClosure(src, dst *vm, fn *lua.LFunction) (*lua.LFunction, error) {
	c := newCopier(src.L, dst.L)
	c.srcAPI, c.dstAPI = src.api, dst.api
	return c.copyFunction(fn)
}
