package luart

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// The tasks and agents namespaces share their lifecycle operations but not
// their identity: each function below is installed once per namespace and
// talks to that namespace only. IDs are plain strings ("t1", "a1").

// seconds converts an optional timeout argument; nil or zero means no limit.
func seconds(L *lua.LState, n int) time.Duration {
	if L.GetTop() >= n && L.Get(n) != lua.LNil {
		return time.Duration(float64(L.CheckNumber(n)) * float64(time.Second))
	}
	return 0
}

// jobErr raises the Lua error for a failed Jobs call.
func jobErr(L *lua.LState, fn string, err error) {
	if errors.Is(err, ErrNoJob) {
		raise(L, fn, ENOENT, "%v", err)
	}
	raise(L, fn, EFAIL, "%v", err)
}

// installTasks installs the tasks namespace: Lua functions in the background.
func (v *vm) installTasks() {
	jobs := func() Jobs { return v.env.Tasks() }
	fns := map[string]lua.LGFunction{
		"spawn": v.tasksSpawn,
		"list": func(L *lua.LState) int {
			out := L.NewTable()
			for _, j := range jobs().List() {
				out.Append(v.jobRow(j, nil))
			}
			L.Push(out)
			return 1
		},
		"get": func(L *lua.LState) int {
			id := L.CheckString(1)
			j, ok := jobs().Get(id)
			if !ok {
				L.Push(lua.LNil)
				return 1
			}
			res, _, _ := jobs().Result(id)
			L.Push(v.jobRow(j, res))
			return 1
		},
		"wait": func(L *lua.LState) int {
			if _, many := L.Get(1).(*lua.LTable); many {
				return v.waitAll(L, "tasks.wait", jobs())
			}
			res, ok, err := jobs().Wait(v.ctx, L.CheckString(1), seconds(L, 2))
			if err != nil {
				jobErr(L, "tasks.wait", err)
			}
			if !ok {
				v.ctxErr("tasks.wait")
				L.Push(lua.LNil)
				L.Push(lua.LString("timeout"))
				return 2
			}
			L.Push(toLua(L, res))
			return 1
		},
		"tail": func(L *lua.LState) int {
			n := 20
			if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
				n = L.CheckInt(2)
			}
			text, err := jobs().Tail(L.CheckString(1), n)
			if err != nil {
				jobErr(L, "tasks.tail", err)
			}
			L.Push(lua.LString(text))
			return 1
		},
		"stop": func(L *lua.LState) int {
			if err := jobs().Stop(L.CheckString(1)); err != nil {
				jobErr(L, "tasks.stop", err)
			}
			return 0
		},
	}
	v.table("tasks", fns)
}

// jobRow is a task as tasks.list and tasks.get show it; res is its result once it ended.
func (v *vm) jobRow(j JobInfo, res map[string]any) *lua.LTable {
	row := v.L.NewTable()
	row.RawSetString("id", lua.LString(j.ID))
	row.RawSetString("name", lua.LString(j.Name))
	row.RawSetString("state", lua.LString(j.State))
	if res != nil {
		row.RawSetString("result", toLua(v.L, res))
	}
	return row
}

// waitAll waits for every ID of the list against one shared deadline and
// returns the results by position; unfinished tasks leave a hole.
func (v *vm) waitAll(L *lua.LState, fn string, jobs Jobs) int {
	list := L.CheckTable(1)
	var deadline time.Time
	if d := seconds(L, 2); d > 0 {
		deadline = time.Now().Add(d)
	}
	out := L.NewTable()
	for i := 1; i <= list.Len(); i++ {
		id, ok := list.RawGetInt(i).(lua.LString)
		if !ok {
			raise(L, fn, EINVAL, "item #%d is not a task id", i)
		}
		var left time.Duration
		if !deadline.IsZero() {
			if left = time.Until(deadline); left <= 0 {
				res, done, err := jobs.Result(string(id))
				if err != nil {
					jobErr(L, fn, err)
				}
				if done {
					out.RawSetInt(i, toLua(L, res))
				}
				continue
			}
		}
		res, done, err := jobs.Wait(v.ctx, string(id), left)
		if err != nil {
			jobErr(L, fn, err)
		}
		v.ctxErr(fn)
		if done {
			out.RawSetInt(i, toLua(L, res))
		}
	}
	L.Push(out)
	return 1
}

// tasksSpawn starts fn in the background: tasks.spawn(name, fn, o?). A fresh
// task VM on its own goroutine gets a copy of fn and everything it captured.
func (v *vm) tasksSpawn(L *lua.LState) int {
	name, ok := L.Get(1).(lua.LString)
	if !ok || strings.TrimSpace(string(name)) == "" {
		raise(L, "tasks.spawn", EINVAL, "expected a name first: tasks.spawn(\"build checks\", function() ... end)")
	}
	fn, ok := L.Get(2).(*lua.LFunction)
	if !ok {
		raise(L, "tasks.spawn", EINVAL, "arg #2 must be a function")
	}
	if fn.IsG {
		raise(L, "tasks.spawn", EINVAL, "expected a Lua function, got a Go function; wrap it: function() return f(...) end")
	}
	o := opts(L, 3)
	to := TaskOpts{Name: strings.Join(strings.Fields(string(name)), " ")}
	if n, ok := optNumber(o, "timeout"); ok && n > 0 {
		to.Timeout = time.Duration(n * float64(time.Second))
	}
	tv := newVM(v.env, v.cfg, roleTask)
	copied, err := newCopier(L, tv.L).copyFunction(fn)
	if err != nil {
		tv.close()
		raise(L, "tasks.spawn", EINVAL, "%v", err)
	}
	id, err := v.env.Tasks().Spawn(tv.task(copied), to)
	if err != nil {
		tv.close()
		raise(L, "tasks.spawn", EFAIL, "%v", err)
	}
	L.Push(lua.LString(id))
	return 1
}

// task returns the body of a task: call fn in this VM, then close it. What
// it prints goes to out; its first return value is the result.
func (v *vm) task(fn *lua.LFunction) TaskFunc {
	return func(ctx context.Context, out io.Writer) (any, error) {
		defer v.close()
		v.ctx, v.out = WithBackground(ctx), out
		v.L.SetContext(ctx)
		if err := v.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			msg := errorMessage(err)
			if trace := traceback(err); trace != "" {
				msg += "\n" + trace
			}
			return nil, errors.New(msg)
		}
		ret := v.L.Get(-1)
		if g, err := toGo(ret); err == nil {
			return g, nil
		}
		return renderValue(ret), nil
	}
}

// waitFinished pushes the result of a job started by agent{}: it waits for the end,
// or stops the job and raises when the call is cancelled or out of time. Nobody
// else waits for that job, so it must not run on unseen.
func (v *vm) waitFinished(L *lua.LState, fn string, jobs Jobs, id string) int {
	res, done, err := jobs.Wait(v.ctx, id, 0)
	if err != nil {
		jobErr(L, fn, err)
	}
	if !done {
		_ = jobs.Stop(id)
		v.ctxErr(fn)
		raise(L, fn, EFAIL, "job did not finish")
	}
	L.Push(toLua(L, res))
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

// agentRun runs a subagent and waits for its answer: agent{prompt, ...}.
func (v *vm) agentRun(L *lua.LState) int {
	agents := v.env.Agents()
	id, err := agents.Spawn(v.agentOpts(L, "agent", true))
	if err != nil {
		raise(L, "agent", EFAIL, "%v", err)
	}
	return v.waitFinished(L, "agent", agents, id)
}
