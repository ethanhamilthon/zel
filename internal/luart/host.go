package luart

import (
	"fmt"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// installHost registers the whole host API in the vm's globals.
func (v *vm) installHost() {
	L := v.L
	L.OpenLibs()
	if paths := v.env.Info().LuaPath; len(paths) > 0 {
		pkg := L.GetGlobal("package").(*lua.LTable)
		current := lua.LVAsString(pkg.RawGetString("path"))
		for _, p := range paths {
			current += ";" + p + "/?.lua"
		}
		pkg.RawSetString("path", lua.LString(current))
	}

	L.SetGlobal("sh", L.NewFunction(v.wrap("sh", v.sh)))
	L.SetGlobal("sleep", L.NewFunction(v.wrap("sleep", v.sleep)))
	L.SetGlobal("view", L.NewFunction(v.wrap("view", v.view)))
	v.table("fs", map[string]lua.LGFunction{
		"read": v.fsRead, "write": v.fsWrite, "edit": v.fsEdit, "list": v.fsList, "glob": v.fsGlob,
		"grep": v.fsGrep, "exists": v.fsExists, "tmp": v.fsTmp,
	})
	v.table("web", map[string]lua.LGFunction{"request": v.webRequest, "get": v.webGet, "post": v.webPost})
	v.table("json", map[string]lua.LGFunction{"encode": v.jsonEncode, "decode": v.jsonDecode})
	v.installJobs("tasks", func() Jobs { return v.env.Tasks() }, v.tasksSpawn, v.tasksRun)
	v.installJobs("agents", func() Jobs { return v.env.Agents() }, v.agentsSpawn, v.agentsRun)
	v.table("state", map[string]lua.LGFunction{"get": v.stateGet, "set": v.stateSet, "list": v.stateList})
	v.table("log", map[string]lua.LGFunction{"info": v.logFn("info"), "warn": v.logFn("warn"), "notice": v.logFn("notice")})
	v.installSession()
}

// table creates a global table whose functions run through the hook wrapper.
func (v *vm) table(name string, fns map[string]lua.LGFunction) {
	t := v.L.NewTable()
	for fn, impl := range fns {
		t.RawSetString(fn, v.L.NewFunction(v.wrap(name+"."+fn, impl)))
	}
	v.L.SetGlobal(name, t)
}

func (v *vm) installSession() {
	L := v.L
	info := v.env.Info()
	t := L.NewTable()
	for k, val := range map[string]string{"id": info.ID, "cwd": info.Cwd, "root": info.Root, "parent": info.Parent, "model": info.Model, "effort": info.Effort} {
		t.RawSetString(k, lua.LString(val))
	}
	t.RawSetString("depth", lua.LNumber(info.Depth))
	t.RawSetString("rename", L.NewFunction(v.wrap("session.rename", func(L *lua.LState) int {
		name := L.CheckString(1)
		if strings.TrimSpace(name) == "" {
			raise(L, "session.rename", EINVAL, "name is empty")
		}
		if err := v.env.Rename(name); err != nil {
			raise(L, "session.rename", EFAIL, "%v", err)
		}
		return 0
	})))
	L.SetGlobal("session", t)
}

// wrap runs before_call/after_call hooks around a host function.
func (v *vm) wrap(name string, fn lua.LGFunction) lua.LGFunction {
	return func(L *lua.LState) int {
		if v.noHooks || v.cfg == nil || !v.cfg.HasCallHooks() {
			return fn(L)
		}
		n := L.GetTop()
		args := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			g, err := toGo(L.Get(i))
			if err != nil {
				return fn(L) // arguments hooks cannot see (functions...) skip the hooks
			}
			args = append(args, g)
		}
		newArgs, deny := v.cfg.BeforeCall(v.ctx, name, args)
		if deny != "" {
			raise(L, name, EDENIED, "%s", deny)
		}
		if newArgs != nil {
			L.SetTop(0)
			for _, a := range newArgs {
				L.Push(toLua(L, a))
			}
			args = newArgs
		}
		started := time.Now()
		nret, failure := v.protect(L, fn)
		if failure != nil {
			v.cfg.AfterCall(v.ctx, name, args, nil, errorMessage(failure), time.Since(started))
			panic(failure)
		}
		var result any
		if nret > 0 {
			result, _ = toGo(L.Get(-nret))
		}
		if replaced, ok := v.cfg.AfterCall(v.ctx, name, args, result, "", time.Since(started)); ok && nret > 0 {
			L.Replace(-nret, toLua(L, replaced))
		}
		return nret
	}
}

func (v *vm) protect(L *lua.LState, fn lua.LGFunction) (nret int, failure error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				failure = e
			} else {
				failure = fmt.Errorf("%v", r)
			}
		}
	}()
	return fn(L), nil
}

// opts reads the optional table argument at position n.
func opts(L *lua.LState, n int) *lua.LTable {
	if t, ok := L.Get(n).(*lua.LTable); ok {
		return t
	}
	return L.NewTable()
}

func optString(t *lua.LTable, key string) (string, bool) {
	if s, ok := t.RawGetString(key).(lua.LString); ok {
		return string(s), true
	}
	return "", false
}

func optNumber(t *lua.LTable, key string) (float64, bool) {
	if n, ok := t.RawGetString(key).(lua.LNumber); ok {
		return float64(n), true
	}
	return 0, false
}

func optBool(t *lua.LTable, key string) bool {
	b, ok := t.RawGetString(key).(lua.LBool)
	return ok && bool(b)
}

func (v *vm) sleep(L *lua.LState) int {
	sec := float64(L.CheckNumber(1))
	if sec < 0 {
		raise(L, "sleep", EINVAL, "seconds must be >= 0")
	}
	select {
	case <-time.After(time.Duration(sec * float64(time.Second))):
	case <-v.ctx.Done():
		v.ctxErr("sleep")
	}
	return 0
}

func (v *vm) view(L *lua.LState) int {
	if L.GetTop() == 0 {
		raise(L, "view", EINVAL, "no path")
	}
	for i := 1; i <= L.GetTop(); i++ {
		path := v.resolve(L.CheckString(i))
		if err := v.env.View(path); err != nil {
			raise(L, "view", EFAIL, "%v", err)
		}
	}
	return 0
}

func (v *vm) logFn(level string) lua.LGFunction {
	return func(L *lua.LState) int {
		msg := luaString(L, L.Get(1))
		if level == "notice" {
			v.env.Notice(msg)
		} else {
			v.env.Log(level, msg)
		}
		return 0
	}
}

func (v *vm) jsonEncode(L *lua.LState) int {
	val := L.CheckAny(1)
	indent := ""
	if s, ok := optString(opts(L, 2), "indent"); ok {
		indent = s
	} else if n, ok := optNumber(opts(L, 2), "indent"); ok {
		indent = strings.Repeat(" ", int(n))
	}
	text, err := encodeJSON(val, indent)
	if err != nil {
		raise(L, "json.encode", EINVAL, "%v", err)
	}
	L.Push(lua.LString(text))
	return 1
}

func (v *vm) jsonDecode(L *lua.LState) int {
	val, err := decodeJSON(L, L.CheckString(1))
	if err != nil {
		raise(L, "json.decode", EINVAL, "%v", err)
	}
	L.Push(val)
	return 1
}
