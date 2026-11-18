package luart

import (
	"context"
	"io"

	lua "github.com/yuin/gopher-lua"
)

func (v *vm) registry(L *lua.LState, fn string) Tasks {
	tasks := v.env.Tasks()
	if tasks == nil {
		raise(L, fn, EDENIED, "no task registry")
	}
	return tasks
}

func (v *vm) spawn(L *lua.LState) int {
	const op = "task.spawn"
	tasks := v.registry(L, op)
	name := textArg(L, op, 1)
	fn, ok := L.Get(2).(*lua.LFunction)
	if !ok || fn.IsG {
		raise(L, op, EINVAL, "expected a Lua function")
	}
	timeout := duration(L, op, opts(L, op, 3).RawGetString("timeout"))
	child := newVM(v.env, v.cfg, true)
	copy, err := cloneClosure(v, child, fn)
	if err != nil {
		child.L.Close()
		raise(L, op, EINVAL, "%v", err)
	}
	id, err := tasks.Spawn(name, timeout, func(ctx context.Context, out io.Writer) (string, error) {
		defer child.L.Close()
		defer child.use(ctx, out)()
		child.L.Push(copy)
		if err := child.L.PCall(0, lua.MultRet, nil); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if child.L.GetTop() == 0 || child.L.Get(1) == lua.LNil {
			return "", nil
		}
		return renderValue(child.L.Get(1)), nil
	})
	if err != nil {
		child.L.Close()
		raiseErr(L, op, err)
	}
	L.Push(lua.LString(id))
	return 1
}

func taskRow(L *lua.LState, i TaskInfo, detail bool) lua.LValue {
	row := map[string]any{"id": i.ID, "name": i.Name, "state": i.State, "ms": i.Ms}
	if detail {
		if i.Result != "" {
			row["result"] = i.Result
		}
		if i.Error != "" {
			row["error"] = i.Error
		}
	}
	return toLua(L, row)
}

func (v *vm) taskList(L *lua.LState) int {
	list := v.registry(L, "task.list").List()
	out := L.CreateTable(len(list), 0)
	for _, i := range list {
		out.Append(taskRow(L, i, false))
	}
	L.Push(out)
	return 1
}

func (v *vm) taskGet(L *lua.LState) int {
	const op = "task.get"
	i, ok := v.registry(L, op).Get(textArg(L, op, 1))
	if !ok {
		L.Push(lua.LNil)
	} else {
		L.Push(taskRow(L, i, true))
	}
	return 1
}

func (v *vm) wait(L *lua.LState) int {
	const op = "task.wait"
	tasks := v.registry(L, op)
	var ids []string
	_, multi := L.Get(1).(*lua.LTable)
	if multi {
		ids = textList(L, op, L.Get(1))
	} else {
		ids = []string{textArg(L, op, 1)}
	}
	timeout := duration(L, op, L.Get(2))

	// The registry may return only the tasks that finished; place results by ID.
	results := map[string]string{}
	for _, i := range tasks.Wait(v.ctx, ids, timeout) {
		if i.Error == "" && (i.Result != "" || i.State == "done" || i.State == "finished") {
			results[i.ID] = i.Result
		}
	}
	get := func(id string) lua.LValue {
		if s, ok := results[id]; ok {
			return lua.LString(s)
		}
		return lua.LNil
	}
	if multi {
		out := L.CreateTable(len(ids), 0)
		for i, id := range ids {
			out.RawSetInt(i+1, get(id))
		}
		L.Push(out)
	} else {
		L.Push(get(ids[0]))
	}
	return 1
}

func (v *vm) tail(L *lua.LState) int {
	const op = "task.tail"
	id := textArg(L, op, 1)
	n := 20
	if L.Get(2) != lua.LNil {
		n = intArg(L, op, L.Get(2))
	}
	if n < 0 {
		raise(L, op, EINVAL, "line count must not be negative")
	}
	text, err := v.registry(L, op).Tail(id, n)
	if err != nil {
		raiseErr(L, op, err)
	}
	L.Push(lua.LString(text))
	return 1
}

func (v *vm) stop(L *lua.LState) int {
	const op = "task.stop"
	if err := v.registry(L, op).Stop(textArg(L, op, 1)); err != nil {
		raiseErr(L, op, err)
	}
	return 0
}
