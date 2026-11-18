package luart

import (
	"context"
	"fmt"
	"io"

	lua "github.com/yuin/gopher-lua"
	"zel/internal/config"
)

// scoped adds snapshot cancellation to the caller's context.
func (c *Config) scoped(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(c.life, cancel)
	return ctx, func() { stop(); cancel() }
}

func (v *vm) callConfig(L *lua.LState, name string) int {
	if L.GetTop() != 1 {
		raise(L, name, EINVAL, "expected one table argument")
	}
	arg := asTable(L, name, L.Get(1))
	values, err := v.cfg.callFunction(v.ctx, v.out, L, name, arg)
	if err != nil {
		raiseErr(L, name, err)
	}
	for _, v := range values {
		L.Push(v)
	}
	return len(values)
}

func (c *Config) callFunction(parent context.Context, out io.Writer, caller *lua.LState, name string, arg *lua.LTable) ([]lua.LValue, error) {
	ctx, cancel := c.scoped(parent)
	defer cancel()
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	incoming := newCopier(caller, c.v.L)
	incoming.dataOnly = true
	value, err := incoming.value(arg, "the argument")
	if err != nil {
		return nil, fmt.Errorf("EINVAL: %w", err)
	}
	defer c.v.use(ctx, out)()
	L := c.v.L
	L.Push(c.funcs[name])
	L.Push(value)
	if err := L.PCall(1, lua.MultRet, nil); err != nil {
		return nil, fmt.Errorf("%s", errorMessage(err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outgoing := newCopier(L, caller)
	outgoing.dataOnly = true
	result := make([]lua.LValue, L.GetTop())
	for i := range result {
		result[i], err = outgoing.value(L.Get(i+1), "a return value")
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

type HookResult struct {
	Text             string
	HasText, Blocked bool
	Reason           string
}

func (c *Config) RunHook(parent context.Context, h config.Hook, event string, data map[string]any) (HookResult, error) {
	ctx, stop := c.scoped(parent)
	ctx, cancel := context.WithTimeout(ctx, config.HookTimeout)
	finish := func() { cancel(); stop() }
	release, err := c.acquire(ctx)
	if err != nil {
		finish()
		return HookResult{}, err
	}
	if h.Ref < 0 || h.Ref >= len(c.hooks) {
		release()
		finish()
		return HookResult{}, fmt.Errorf("invalid hook reference %d", h.Ref)
	}
	fn := c.hooks[h.Ref]
	if h.Async {
		child := newVM(c.v.env, c, false)
		copy, err := cloneClosure(c.v, child, fn)
		if err != nil {
			child.L.Close()
			release()
			finish()
			return HookResult{}, fmt.Errorf("%s: %w", h.Source, err)
		}
		// Convert event data before returning, so the caller may reuse it.
		input := toLua(child.L, data)
		c.asyncMu.Lock()
		if err := c.life.Err(); err != nil {
			c.asyncMu.Unlock()
			child.L.Close()
			release()
			finish()
			return HookResult{}, err
		}
		c.async.Add(1)
		c.asyncMu.Unlock()
		release()
		go func() {
			defer c.async.Done()
			defer finish()
			defer child.L.Close()
			if _, err := child.invokeHook(ctx, copy, event, input); err != nil {
				c.v.env.Log("hook", fmt.Sprintf("%s %s: %s", event, h.Source, errorMessage(err)))
			}
		}()
		return HookResult{}, nil
	}
	input := toLua(c.v.L, data)
	type outcome struct {
		result HookResult
		err    error
	}
	done := make(chan outcome, 1)
	defer finish()
	go func() {
		defer release()
		result, err := c.v.invokeHook(ctx, fn, event, input)
		done <- outcome{result, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return HookResult{}, fmt.Errorf("%s: %s", h.Source, errorMessage(o.err))
		}
		return o.result, nil
	case <-ctx.Done():
		return HookResult{}, fmt.Errorf("%s: %w", h.Source, ctx.Err())
	}
}

func (v *vm) invokeHook(ctx context.Context, fn *lua.LFunction, event string, data lua.LValue) (HookResult, error) {
	defer v.use(ctx, io.Discard)()
	L := v.L
	L.Push(fn)
	L.Push(lua.LString(event))
	L.Push(data)
	if err := L.PCall(2, 2, nil); err != nil {
		return HookResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return HookResult{}, err
	}
	switch val := L.Get(1).(type) {
	case lua.LString:
		return HookResult{Text: string(val), HasText: true}, nil
	case lua.LBool:
		if !bool(val) {
			switch event {
			case "message", "notification", "lua_before":
				reason, _ := L.Get(2).(lua.LString)
				return HookResult{Blocked: true, Reason: string(reason)}, nil
			}
		}
	}
	return HookResult{}, nil
}
