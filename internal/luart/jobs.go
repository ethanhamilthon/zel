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

func (v *vm) installJobs(name string, jobs func() Jobs, spawn, run lua.LGFunction) {
	fn := func(op string) string { return name + "." + op }
	fns := map[string]lua.LGFunction{
		"spawn": spawn,
		"list": func(L *lua.LState) int {
			out := L.NewTable()
			for _, j := range jobs().List() {
				out.Append(v.jobRow(name, j))
			}
			L.Push(out)
			return 1
		},
		"get": func(L *lua.LState) int {
			j, ok := jobs().Get(L.CheckString(1))
			if !ok {
				L.Push(lua.LNil)
				return 1
			}
			L.Push(v.jobRow(name, j))
			return 1
		},
		"status": func(L *lua.LState) int {
			j, ok := jobs().Get(L.CheckString(1))
			if !ok {
				jobErr(L, fn("status"), ErrNoJob)
			}
			L.Push(lua.LString(j.State))
			return 1
		},
		"wait": func(L *lua.LState) int {
			res, ok, err := jobs().Wait(v.ctx, L.CheckString(1), seconds(L, 2))
			if err != nil {
				jobErr(L, fn("wait"), err)
			}
			if !ok {
				v.ctxErr(fn("wait"))
				L.Push(lua.LNil)
				L.Push(lua.LString("timeout"))
				return 2
			}
			L.Push(toLua(L, res))
			return 1
		},
		"wait_all": func(L *lua.LState) int { return v.waitAll(L, fn("wait_all"), jobs()) },
		"result": func(L *lua.LState) int {
			res, ok, err := jobs().Result(L.CheckString(1))
			if err != nil {
				jobErr(L, fn("result"), err)
			}
			if !ok {
				L.Push(lua.LNil)
				return 1
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
				jobErr(L, fn("tail"), err)
			}
			L.Push(lua.LString(text))
			return 1
		},
		"stop": func(L *lua.LState) int {
			if err := jobs().Stop(L.CheckString(1)); err != nil {
				jobErr(L, fn("stop"), err)
			}
			return 0
		},
	}
	if run != nil {
		fns["run"] = run
	}
	v.table(name, fns)
}

func (v *vm) jobRow(namespace string, j JobInfo) *lua.LTable {
	row := v.L.NewTable()
	row.RawSetString("id", lua.LString(j.ID))
	row.RawSetString("state", lua.LString(j.State))
	if namespace == "agents" {
		row.RawSetString("model", lua.LString(j.Model))
		row.RawSetString("prompt", lua.LString(j.Prompt))
	} else {
		row.RawSetString("name", lua.LString(j.Name))
	}
	return row
}

// waitAll waits for every ID of the list against one shared deadline and
// returns the results by position; unfinished jobs leave a hole.
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
			raise(L, fn, EINVAL, "item #%d is not a job id", i)
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

// tasksSpawn starts fn in the background: a fresh task VM on its own
// goroutine gets a copy of fn and everything it captured.
func (v *vm) tasksSpawn(L *lua.LState) int {
	fn := L.CheckFunction(1)
	if fn.IsG {
		raise(L, "tasks.spawn", EINVAL, "expected a Lua function, got a Go function; wrap it: function() return f(...) end")
	}
	o := opts(L, 2)
	var to TaskOpts
	to.Name, _ = optString(o, "name")
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

// CodeTask compiles code into the body of a task with its own VM, for tasks
// started from outside Lua. discard frees the VM when the task never runs.
func CodeTask(env Env, cfg *Config, code string) (run TaskFunc, discard func(), err error) {
	tv := newVM(env, cfg, roleTask)
	fn, err := tv.L.Load(strings.NewReader(code), "task")
	if err != nil {
		tv.close()
		return nil, nil, err
	}
	return tv.task(fn), tv.close, nil
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

// waitFinished pushes the result of a job started by run: it waits for the end
// or raises when the invocation is cancelled or out of time. The job itself
// keeps running then; its result arrives as a message.
func (v *vm) waitFinished(L *lua.LState, fn string, jobs Jobs, id string) int {
	res, done, err := jobs.Wait(v.ctx, id, 0)
	if err != nil {
		jobErr(L, fn, err)
	}
	if !done {
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

func (v *vm) agentsSpawn(L *lua.LState) int {
	id, err := v.env.Agents().Spawn(v.agentOpts(L, "agents.spawn", false))
	if err != nil {
		raise(L, "agents.spawn", EFAIL, "%v", err)
	}
	L.Push(lua.LString(id))
	return 1
}

func (v *vm) agentsRun(L *lua.LState) int {
	agents := v.env.Agents()
	id, err := agents.Spawn(v.agentOpts(L, "agents.run", true))
	if err != nil {
		raise(L, "agents.run", EFAIL, "%v", err)
	}
	return v.waitFinished(L, "agents.run", agents, id)
}
