package luart

import (
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

	if v.role == roleConfig {
		v.out = logWriter{v.env}
	}
	L.SetGlobal("print", L.NewFunction(v.print))
	L.SetGlobal("sh", L.NewFunction(v.sh))
	L.SetGlobal("sleep", L.NewFunction(v.sleep))
	L.SetGlobal("view", L.NewFunction(v.view))
	L.SetGlobal("slice", L.NewFunction(slice))
	v.table("fs", map[string]lua.LGFunction{
		"read": v.fsRead, "write": v.fsWrite, "edit": v.fsEdit, "list": v.fsList, "glob": v.fsGlob,
		"grep": v.fsGrep, "exists": v.fsExists, "tmp": v.fsTmp, "patch": v.fsPatch,
	})
	v.table("web", map[string]lua.LGFunction{"request": v.webRequest, "get": v.webGet, "post": v.webPost})
	v.table("json", map[string]lua.LGFunction{"encode": v.jsonEncode, "decode": v.jsonDecode})
	v.installJobs("tasks", func() Jobs { return v.env.Tasks() }, v.tasksSpawn, nil)
	if v.agentsAllowed() {
		v.installJobs("agents", func() Jobs { return v.env.Agents() }, v.agentsSpawn, v.agentsRun)
	} else {
		v.disabled("agents", append([]string{"run"}, jobFns...), "subagents cannot start subagents; use tasks.spawn")
	}
	v.table("state", map[string]lua.LGFunction{"get": v.stateGet, "set": v.stateSet, "list": v.stateList})
	v.table("log", map[string]lua.LGFunction{"info": v.logFn("info"), "warn": v.logFn("warn"), "notice": v.logFn("notice")})
	v.installSession()
}

// logWriter sends what the config vm prints to the session's log.
type logWriter struct{ env Env }

func (w logWriter) Write(p []byte) (int, error) {
	w.env.Log("print", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// table creates a global table of host functions.
func (v *vm) table(name string, fns map[string]lua.LGFunction) {
	t := v.L.NewTable()
	for fn, impl := range fns {
		t.RawSetString(fn, v.L.NewFunction(impl))
	}
	v.L.SetGlobal(name, t)
}

// disabled installs a namespace whose functions all fail with reason.
func (v *vm) disabled(name string, fns []string, reason string) {
	t := v.L.NewTable()
	for _, fn := range fns {
		full := name + "." + fn
		t.RawSetString(fn, v.L.NewFunction(func(L *lua.LState) int {
			raise(L, full, EDENIED, "%s", reason)
			return 0
		}))
	}
	v.L.SetGlobal(name, t)
}

// installSession (re)builds the session table from the current session info.
func (v *vm) installSession() {
	L := v.L
	info := v.env.Info()
	t := L.NewTable()
	for k, val := range map[string]string{"id": info.ID, "cwd": info.Cwd, "root": info.Root, "parent": info.Parent, "model": info.Model, "effort": info.Effort} {
		t.RawSetString(k, lua.LString(val))
	}
	t.RawSetString("depth", lua.LNumber(info.Depth))
	t.RawSetString("models", L.NewFunction(v.sessionModels))
	t.RawSetString("rename", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if strings.TrimSpace(name) == "" {
			raise(L, "session.rename", EINVAL, "name is empty")
		}
		if err := v.env.Rename(name); err != nil {
			raise(L, "session.rename", EFAIL, "%v", err)
		}
		return 0
	}))
	L.SetGlobal("session", t)
}

// sessionModels returns the models as a list of {provider, model, context_window}
// and the failures of providers as {provider = message}.
func (v *vm) sessionModels(L *lua.LState) int {
	models, failures := v.env.Models(v.ctx)
	if v.ctx.Err() != nil {
		v.ctxErr("session.models")
	}
	list := L.NewTable()
	for _, m := range models {
		row := L.NewTable()
		row.RawSetString("provider", lua.LString(m.Provider))
		row.RawSetString("model", lua.LString(m.Model))
		row.RawSetString("context_window", lua.LNumber(m.ContextWindow))
		list.Append(row)
	}
	fails := L.NewTable()
	for name, msg := range failures {
		fails.RawSetString(name, lua.LString(msg))
	}
	L.Push(list)
	L.Push(fails)
	return 2
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

// slice returns count lines of s starting at line from (1-based). A negative
// from counts from the end (-10: the last ten lines); without count it runs to
// the end.
func slice(L *lua.LState) int {
	s := L.CheckString(1)
	from := L.OptInt(2, 1)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if s == "" {
		lines = nil
	}
	start := from - 1
	if from < 0 {
		start = len(lines) + from
	}
	start = max(start, 0)
	if start >= len(lines) {
		L.Push(lua.LString(""))
		return 1
	}
	end := len(lines)
	if L.GetTop() >= 3 && L.Get(3) != lua.LNil {
		if n := L.CheckInt(3); n >= 0 {
			end = min(end, start+n)
		}
	}
	L.Push(lua.LString(strings.Join(lines[start:end], "\n")))
	return 1
}
