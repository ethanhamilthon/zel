package luart

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"zel/internal/shell"

	lua "github.com/yuin/gopher-lua"
)

const shOutputLimit = 8 << 20

// sh(cmd, o?) runs `bash -c cmd` in its own process group.
func (v *vm) sh(L *lua.LState) int {
	script := textArg(L, "sh", 1)
	o := opts(L, "sh", 2)
	check := boolOpt(L, "sh", o.RawGetString("check"))
	info := v.env.Info()
	dir := info.Cwd
	if d := o.RawGetString("cwd"); d != lua.LNil {
		dir = v.resolve(asString(L, "sh", d))
	}
	env := map[string]string{}
	for _, entry := range v.env.ShellEnv() {
		if k, val, ok := strings.Cut(entry, "="); ok {
			env[k] = val
		}
	}
	if v.loadingEnv != nil {
		for k, val := range v.loadingEnv() {
			env[k] = val
		}
	}
	if extra := o.RawGetString("env"); extra != lua.LNil {
		asTable(L, "sh", extra).ForEach(func(k, val lua.LValue) { env[asString(L, "sh", k)] = asString(L, "sh", val) })
	}
	ctx := v.ctx
	if timeout := duration(L, "sh", o.RawGetString("timeout")); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := shell.Command(ctx, dir, env, script)
	buf := shell.NewOutputBuffer(shOutputLimit)
	cmd.Stdout, cmd.Stderr = buf, buf
	if stdin := o.RawGetString("stdin"); stdin != lua.LNil {
		cmd.Stdin = strings.NewReader(asString(L, "sh", stdin))
	}
	started := time.Now()
	code, timedOut := 0, false
	if err := cmd.Start(); err != nil {
		code = 127
		buf.Write([]byte(err.Error() + "\n"))
	} else {
		err := shell.Reap(cmd)
		var exit *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			code, timedOut = 124, true
		case errors.As(err, &exit):
			code = exit.ExitCode()
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = 128 + int(status.Signal())
			}
		case err != nil:
			code = 1
		}
	}
	out := buf.Text()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code, timedOut = 124, true
	}
	if check && code != 0 {
		raise(L, "sh", EFAIL, "exit status %d: %s", code, firstLine(out))
	}
	t := L.NewTable()
	t.RawSetString("ok", lua.LBool(code == 0))
	t.RawSetString("code", lua.LNumber(code))
	t.RawSetString("out", lua.LString(out))
	t.RawSetString("timed_out", lua.LBool(timedOut))
	t.RawSetString("ms", lua.LNumber(time.Since(started).Milliseconds()))
	L.Push(t)
	return 1
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, more := strings.Cut(s, "\n")
	if len(line) > 200 {
		line, more = line[:200], true
	}
	if more {
		line += "..."
	}
	return line
}
