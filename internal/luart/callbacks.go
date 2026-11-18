package luart

import (
	"context"
	"errors"
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
	"zel/internal/config"
)

// scoped adds snapshot cancellation to the caller's context.
func (c *Config) scoped(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(c.life, cancel)
	return ctx, func() { stop(); cancel() }
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
		child := c.freshVM()
		copy, err := newCopier(c.v.L, child.L).copyFunction(fn)
		if err != nil {
			child.close()
			release()
			finish()
			return HookResult{}, fmt.Errorf("%s: %w", h.Source, err)
		}
		// Convert event data before returning, so the caller may reuse it.
		input := toLua(child.L, data)
		c.asyncMu.Lock()
		if err := c.life.Err(); err != nil {
			c.asyncMu.Unlock()
			child.close()
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
			defer child.close()
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
	defer v.use(ctx)()
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

// freshVM is a VM for an async hook or a command: the standard library, the
// package.path and the settings.env of the config, and nothing else.
func (c *Config) freshVM() *vm {
	v := newVM(c.env, c.pkgPath)
	v.logPrint()
	v.settingsEnv = c.settingsEnv
	return v
}

// RunCommand runs a command of the config in a fresh VM with a copy of its fn,
// without a timeout (ctx ends it). It returns the first returned value when it
// is a string.
func (c *Config) RunCommand(parent context.Context, cmd config.Command) (string, error) {
	ctx, stop := c.scoped(parent)
	defer stop()
	release, err := c.acquire(ctx)
	if err != nil {
		return "", err
	}
	if cmd.Ref < 0 || cmd.Ref >= len(c.cmds) {
		release()
		return "", fmt.Errorf("invalid command reference %d", cmd.Ref)
	}
	child := c.freshVM()
	defer child.close()
	fn, err := newCopier(c.v.L, child.L).copyFunction(c.cmds[cmd.Ref])
	release()
	if err != nil {
		return "", fmt.Errorf("commands.%s: %w", cmd.Name, err)
	}
	c.asyncMu.Lock()
	if err := c.life.Err(); err != nil {
		c.asyncMu.Unlock()
		return "", err
	}
	c.async.Add(1)
	c.asyncMu.Unlock()
	defer c.async.Done()
	defer child.use(ctx)()
	child.L.Push(fn)
	if err := child.L.PCall(0, 1, nil); err != nil {
		return "", errors.New(errorMessage(err))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s, ok := child.L.Get(-1).(lua.LString); ok {
		return strings.TrimSpace(string(s)), nil
	}
	return "", nil
}
