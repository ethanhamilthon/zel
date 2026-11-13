package luart

import (
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Handles are plain tables {id, kind} with a shared metatable, so tasks.get(id)
// in a later call yields an equal handle and handles survive JSON copies.

func (v *vm) installHandle() {
	L := v.L
	methods := L.NewTable()
	methods.RawSetString("status", L.NewFunction(v.wrap("h:status", v.hStatus)))
	methods.RawSetString("wait", L.NewFunction(v.wrap("h:wait", v.hWait)))
	methods.RawSetString("result", L.NewFunction(v.wrap("h:result", v.hResult)))
	methods.RawSetString("tail", L.NewFunction(v.wrap("h:tail", v.hTail)))
	methods.RawSetString("stop", L.NewFunction(v.wrap("h:stop", v.hStop)))
	v.mt = L.NewTable()
	v.mt.RawSetString("__index", methods)
}

func (v *vm) handle(j Job) lua.LValue {
	t := v.L.NewTable()
	t.RawSetString("id", lua.LString(j.ID()))
	t.RawSetString("kind", lua.LString(j.Kind()))
	v.L.SetMetatable(t, v.mt)
	return t
}

func (v *vm) jobOf(L *lua.LState, fn string, n int) Job {
	t := L.CheckTable(n)
	kind, _ := optString(t, "kind")
	id, _ := optString(t, "id")
	j, ok := v.env.GetJob(kind, id)
	if !ok {
		raise(L, fn, ENOENT, "no such %s %q", kind, id)
	}
	return j
}

func (v *vm) hStatus(L *lua.LState) int {
	L.Push(lua.LString(v.jobOf(L, "h:status", 1).Status()))
	return 1
}

func (v *vm) hWait(L *lua.LState) int {
	j := v.jobOf(L, "h:wait", 1)
	var timeout time.Duration
	if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
		timeout = time.Duration(float64(L.CheckNumber(2)) * float64(time.Second))
	}
	res, ok := j.Wait(v.ctx, timeout)
	if !ok {
		v.ctxErr("h:wait")
		L.Push(lua.LNil)
		L.Push(lua.LString("timeout"))
		return 2
	}
	L.Push(toLua(L, res))
	return 1
}

func (v *vm) hResult(L *lua.LState) int {
	res, ok := v.jobOf(L, "h:result", 1).Result()
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(toLua(L, res))
	return 1
}

func (v *vm) hTail(L *lua.LState) int {
	j := v.jobOf(L, "h:tail", 1)
	n := 20
	if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
		n = L.CheckInt(2)
	}
	L.Push(lua.LString(j.Tail(n)))
	return 1
}

func (v *vm) hStop(L *lua.LState) int {
	v.jobOf(L, "h:stop", 1).Stop()
	return 0
}

func (v *vm) waitAll(L *lua.LState) int {
	list := L.CheckTable(1)
	var deadline time.Time
	if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
		deadline = time.Now().Add(time.Duration(float64(L.CheckNumber(2)) * float64(time.Second)))
	}
	out := L.NewTable()
	for i := 1; i <= list.Len(); i++ {
		h, ok := list.RawGetInt(i).(*lua.LTable)
		if !ok {
			raise(L, "wait_all", EINVAL, "item #%d is not a handle", i)
		}
		kind, _ := optString(h, "kind")
		id, _ := optString(h, "id")
		j, found := v.env.GetJob(kind, id)
		if !found {
			raise(L, "wait_all", ENOENT, "no such %s %q", kind, id)
		}
		var res map[string]any
		var done bool
		if deadline.IsZero() {
			res, done = j.Wait(v.ctx, 0)
		} else if left := time.Until(deadline); left > 0 {
			res, done = j.Wait(v.ctx, left)
		} else {
			res, done = j.Result()
		}
		v.ctxErr("wait_all")
		if done {
			out.RawSetInt(i, toLua(L, res))
		}
	}
	L.Push(out)
	return 1
}

func (v *vm) taskOpts(L *lua.LState, o *lua.LTable) TaskOpts {
	var t TaskOpts
	t.Name, _ = optString(o, "name")
	if d, ok := optString(o, "cwd"); ok {
		t.Cwd = v.resolve(d)
	}
	if env, ok := o.RawGetString("env").(*lua.LTable); ok {
		t.Env = map[string]string{}
		env.ForEach(func(k, val lua.LValue) { t.Env[k.String()] = val.String() })
	}
	if n, ok := optNumber(o, "timeout"); ok && n > 0 {
		t.Timeout = time.Duration(n * float64(time.Second))
	}
	return t
}

func (v *vm) tasksSpawn(L *lua.LState) int { return v.spawnTask(L, "tasks.spawn", false) }

func (v *vm) tasksRun(L *lua.LState) int {
	if n := v.spawnTask(L, "tasks.run", true); n == 0 {
		return 0
	}
	return v.waitPushed(L, "tasks.run")
}

func (v *vm) spawnTask(L *lua.LState, fn string, waited bool) int {
	cmd := L.CheckString(1)
	if cmd == "" {
		raise(L, fn, EINVAL, "empty command")
	}
	o := v.taskOpts(L, opts(L, 2))
	o.Waited = waited
	j, err := v.env.SpawnTask(cmd, o)
	if err != nil {
		raise(L, fn, EFAIL, "%v", err)
	}
	L.Push(v.handle(j))
	return 1
}

// waitPushed replaces the handle on top of the stack with the job's result.
func (v *vm) waitPushed(L *lua.LState, fn string) int {
	h := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	kind, _ := optString(h, "kind")
	id, _ := optString(h, "id")
	j, ok := v.env.GetJob(kind, id)
	if !ok {
		raise(L, fn, ENOENT, "no such %s %q", kind, id)
	}
	res, done := j.Wait(v.ctx, 0)
	if !done {
		v.ctxErr(fn)
		raise(L, fn, EFAIL, "job did not finish")
	}
	L.Push(toLua(L, res))
	return 1
}

func (v *vm) tasksGet(L *lua.LState) int  { return v.getJob(L, "task") }
func (v *vm) agentsGet(L *lua.LState) int { return v.getJob(L, "agent") }

func (v *vm) getJob(L *lua.LState, kind string) int {
	j, ok := v.env.GetJob(kind, L.CheckString(1))
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(v.handle(j))
	return 1
}

func (v *vm) tasksList(L *lua.LState) int {
	out := L.NewTable()
	for _, j := range v.env.ListJobs("task") {
		row := L.NewTable()
		row.RawSetString("id", lua.LString(j.ID))
		row.RawSetString("name", lua.LString(j.Name))
		row.RawSetString("cmd", lua.LString(j.Cmd))
		row.RawSetString("state", lua.LString(j.State))
		out.Append(row)
	}
	L.Push(out)
	return 1
}

func (v *vm) agentsList(L *lua.LState) int {
	out := L.NewTable()
	for _, j := range v.env.ListJobs("agent") {
		row := L.NewTable()
		row.RawSetString("id", lua.LString(j.ID))
		row.RawSetString("model", lua.LString(j.Model))
		row.RawSetString("state", lua.LString(j.State))
		row.RawSetString("prompt", lua.LString(j.Prompt))
		out.Append(row)
	}
	L.Push(out)
	return 1
}

func (v *vm) agentOpts(L *lua.LState, fn string, waited bool) AgentOpts {
	o, ok := L.Get(1).(*lua.LTable)
	if !ok {
		raise(L, fn, EINVAL, "expected a table {prompt=...}")
	}
	var a AgentOpts
	a.Prompt, _ = optString(o, "prompt")
	if a.Prompt == "" {
		raise(L, fn, EINVAL, "prompt is required")
	}
	a.Model, _ = optString(o, "model")
	a.Effort, _ = optString(o, "effort")
	a.Provider, _ = optString(o, "provider")
	if d, ok := optString(o, "cwd"); ok {
		a.Cwd = v.resolve(d)
	}
	if n, ok := optNumber(o, "timeout"); ok && n > 0 {
		a.Timeout = time.Duration(n * float64(time.Second))
	}
	a.Waited = waited
	return a
}

func (v *vm) agentsSpawn(L *lua.LState) int {
	j, err := v.env.SpawnAgent(v.agentOpts(L, "agents.spawn", false))
	if err != nil {
		raise(L, "agents.spawn", EFAIL, "%v", err)
	}
	L.Push(v.handle(j))
	return 1
}

func (v *vm) agentsRun(L *lua.LState) int {
	j, err := v.env.SpawnAgent(v.agentOpts(L, "agents.run", true))
	if err != nil {
		raise(L, "agents.run", EFAIL, "%v", err)
	}
	L.Push(v.handle(j))
	return v.waitPushed(L, "agents.run")
}
