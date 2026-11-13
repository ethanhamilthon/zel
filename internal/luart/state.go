package luart

import (
	"encoding/json"

	lua "github.com/yuin/gopher-lua"
)

// Limits of the persistent state.
const (
	MaxStateKey   = 256
	MaxStateValue = 64 << 10
	MaxStateScope = 1 << 20
)

func (v *vm) scope(L *lua.LState, fn string, n int) string {
	if L.GetTop() < n || L.Get(n) == lua.LNil {
		return "session"
	}
	s := L.CheckString(n)
	if s != "session" && s != "project" {
		raise(L, fn, EINVAL, `scope must be "session" or "project", got %q`, s)
	}
	return s
}

func (v *vm) stateGet(L *lua.LState) int {
	key := L.CheckString(1)
	scope := v.scope(L, "state.get", 2)
	text, ok, err := v.env.State().Get(scope, key)
	if err != nil {
		raise(L, "state.get", EFAIL, "%v", err)
	}
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	val, err := decodeJSON(L, text)
	if err != nil {
		raise(L, "state.get", EFAIL, "stored value is not valid JSON: %v", err)
	}
	L.Push(val)
	return 1
}

func (v *vm) stateSet(L *lua.LState) int {
	key := L.CheckString(1)
	val := L.CheckAny(2)
	scope := v.scope(L, "state.set", 3)
	if key == "" || len(key) > MaxStateKey {
		raise(L, "state.set", ELIMIT, "key must be 1..%d bytes", MaxStateKey)
	}
	store := v.env.State()
	if val == lua.LNil {
		if err := store.Delete(scope, key); err != nil {
			raise(L, "state.set", EFAIL, "%v", err)
		}
		return 0
	}
	g, err := toGo(val)
	if err != nil {
		raise(L, "state.set", EINVAL, "value must be JSON-compatible: %v", err)
	}
	data, _ := json.Marshal(g)
	if len(data) > MaxStateValue {
		raise(L, "state.set", ELIMIT, "value is %d bytes, the limit is %d", len(data), MaxStateValue)
	}
	size, err := store.Size(scope)
	if err != nil {
		raise(L, "state.set", EFAIL, "%v", err)
	}
	if old, ok, _ := store.Get(scope, key); ok {
		size -= len(old) + len(key)
	}
	if size+len(data)+len(key) > MaxStateScope {
		raise(L, "state.set", ELIMIT, "the %s scope would exceed %d bytes", scope, MaxStateScope)
	}
	if err := store.Set(scope, key, string(data)); err != nil {
		raise(L, "state.set", EFAIL, "%v", err)
	}
	return 0
}

func (v *vm) stateList(L *lua.LState) int {
	prefix := ""
	if L.GetTop() >= 1 && L.Get(1) != lua.LNil {
		prefix = L.CheckString(1)
	}
	scope := v.scope(L, "state.list", 2)
	keys, err := v.env.State().List(scope, prefix)
	if err != nil {
		raise(L, "state.list", EFAIL, "%v", err)
	}
	L.Push(toLua(L, keys))
	return 1
}
