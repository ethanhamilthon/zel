package luart

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
	"zel/docs"
)

const (
	EINVAL  = "EINVAL"
	ENOENT  = "ENOENT"
	ELIMIT  = "ELIMIT"
	EFAIL   = "EFAIL"
	EDENIED = "EDENIED"
)

func raise(L *lua.LState, fn, code, format string, args ...any) {
	// Level zero keeps the host error contract literal in pcall.
	L.Error(lua.LString(fn+": "+code+": "+fmt.Sprintf(format, args...)), 0)
}

func raiseErr(L *lua.LState, fn string, err error) {
	code := EFAIL
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = ENOENT
	case errors.Is(err, fs.ErrPermission):
		code = EDENIED
	default:
		prefix, msg, ok := strings.Cut(err.Error(), ": ")
		switch prefix {
		case "EINVAL", "ENOENT", "ENOMATCH", "EAMBIG", "ELIMIT", "EFAIL", "EDENIED", "ETIMEOUT", "ECANCELED":
			if ok {
				raise(L, fn, prefix, "%s", msg)
			}
		}
	}
	raise(L, fn, code, "%v", err)
}

func textArg(L *lua.LState, fn string, arg int) string {
	return asString(L, fn, L.Get(arg))
}

func asString(L *lua.LState, fn string, val lua.LValue) string {
	if s, ok := val.(lua.LString); ok {
		return string(s)
	}
	raise(L, fn, EINVAL, "expected string, got %s", val.Type())
	return ""
}

func opts(L *lua.LState, fn string, arg int) *lua.LTable {
	if L.Get(arg) == lua.LNil {
		return L.NewTable()
	}
	return asTable(L, fn, L.Get(arg))
}

func asTable(L *lua.LState, fn string, val lua.LValue) *lua.LTable {
	if t, ok := val.(*lua.LTable); ok {
		return t
	}
	raise(L, fn, EINVAL, "expected table, got %s", val.Type())
	return nil
}

func boolOpt(L *lua.LState, fn string, val lua.LValue) bool {
	if val == lua.LNil {
		return false
	}
	if b, ok := val.(lua.LBool); ok {
		return bool(b)
	}
	raise(L, fn, EINVAL, "expected boolean")
	return false
}

func intArg(L *lua.LState, fn string, val lua.LValue) int {
	if n, ok := val.(lua.LNumber); ok && math.Abs(float64(n)) < 1<<53 && math.Trunc(float64(n)) == float64(n) {
		return int(n)
	}
	raise(L, fn, EINVAL, "expected integer")
	return 0
}

func duration(L *lua.LState, fn string, val lua.LValue) time.Duration {
	if val == lua.LNil {
		return 0
	}
	if n, ok := val.(lua.LNumber); ok && n > 0 && float64(n)*float64(time.Second) < math.MaxInt64 {
		return max(time.Nanosecond, time.Duration(float64(n)*float64(time.Second)))
	}
	raise(L, fn, EINVAL, "timeout must be a positive number of seconds")
	return 0
}

func (v *vm) resolve(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(v.env.Info().Cwd, path)
}

func (v *vm) install(globals bool) {
	L := v.L
	L.SetGlobal("print", L.NewFunction(v.print))
	v.api = L.NewTable()
	L.SetFuncs(v.api, map[string]lua.LGFunction{
		"sh": v.sh, "read": v.fsRead, "slice": slice, "write": v.fsWrite, "edit": v.fsEdit,
		"list": v.fsList, "glob": v.fsGlob, "grep": v.fsGrep, "exists": v.fsExists,
		"choice": v.choice, "cd": v.cd, "docs": getDocs, "models": v.models,
	})
	tasks := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"spawn": v.spawn, "list": v.taskList, "get": v.taskGet, "wait": v.wait, "tail": v.tail, "stop": v.stop,
	})
	v.api.RawSetString("task", tasks)
	session := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{"rename": v.rename})
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(v.sessionField))
	L.SetMetatable(session, mt)
	v.api.RawSetString("session", session)
	L.PreloadModule("zel", func(L *lua.LState) int { L.Push(v.api); return 1 })
	if globals {
		v.api.ForEach(func(k, val lua.LValue) { L.SetGlobal(k.String(), val) })
		if v.cfg != nil {
			for _, f := range v.cfg.Data().Functions {
				L.SetGlobal(f.Name, L.NewFunction(func(L *lua.LState) int { return v.callConfig(L, f.Name) }))
			}
		}
	}
}

func (v *vm) sessionField(L *lua.LState) int {
	i := v.env.Info()
	switch L.Get(2).String() {
	case "id":
		L.Push(lua.LString(i.ID))
	case "name":
		L.Push(lua.LString(i.Name))
	case "cwd":
		L.Push(lua.LString(i.Cwd))
	case "root":
		L.Push(lua.LString(i.Root))
	case "mode":
		L.Push(lua.LString(i.Mode))
	case "modes":
		L.Push(toLua(L, i.Modes))
	case "depth":
		L.Push(lua.LNumber(i.Depth))
	case "model":
		L.Push(lua.LString(i.Model))
	case "effort":
		L.Push(lua.LString(i.Effort))
	case "provider":
		L.Push(lua.LString(i.Provider))
	default:
		L.Push(lua.LNil)
	}
	return 1
}

func (v *vm) cd(L *lua.LState) int {
	dir := ""
	if L.Get(1) != lua.LNil {
		dir = v.resolve(textArg(L, "cd", 1))
	}
	if err := v.env.Cd(dir); err != nil {
		raiseErr(L, "cd", err)
	}
	return 0
}

func (v *vm) rename(L *lua.LState) int {
	if err := v.env.Rename(textArg(L, "session.rename", 1)); err != nil {
		raiseErr(L, "session.rename", err)
	}
	return 0
}

func (v *vm) choice(L *lua.LState) int {
	if v.env.Info().Mode != "main" {
		raise(L, "choice", EDENIED, "not available in headless mode")
	}
	question := textArg(L, "choice", 1)
	options := textList(L, "choice", L.Get(2))
	if len(options) == 0 {
		raise(L, "choice", EINVAL, "expected at least one option")
	}
	id, err := v.env.Choice(question, options)
	if err != nil {
		raiseErr(L, "choice", err)
	}
	L.Push(lua.LString(id))
	return 1
}

func textList(L *lua.LState, fn string, val lua.LValue) []string {
	t := asTable(L, fn, val)
	if !isArray(t) {
		raise(L, fn, EINVAL, "expected a list")
	}
	out := make([]string, t.Len())
	for i := range out {
		out[i] = asString(L, fn, t.RawGetInt(i+1))
	}
	return out
}

func getDocs(L *lua.LState) int {
	text := docs.Index()
	if L.Get(1) != lua.LNil {
		topic := textArg(L, "docs", 1)
		var ok bool
		text, ok = docs.Get(topic)
		if !ok {
			raise(L, "docs", ENOENT, "unknown topic %q", topic)
		}
	}
	L.Push(lua.LString(text))
	return 1
}

func (v *vm) models(L *lua.LState) int {
	list, failures := v.env.Models(v.ctx)
	out := L.CreateTable(len(list), 0)
	for _, m := range list {
		out.Append(toLua(L, map[string]any{"provider": m.Provider, "model": m.Model, "context_window": m.ContextWindow}))
	}
	L.Push(out)
	L.Push(toLua(L, failures))
	return 2
}
