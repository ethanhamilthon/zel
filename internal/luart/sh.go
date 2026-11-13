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
	script := L.CheckString(1)
	o := opts(L, 2)
	if strings.TrimSpace(script) == "" {
		raise(L, "sh", EINVAL, "empty command")
	}
	info := v.env.Info()
	dir := info.Cwd
	if d, ok := optString(o, "cwd"); ok {
		dir = v.resolve(d)
	}
	env := v.env.ShellEnv()
	if extra, ok := o.RawGetString("env").(*lua.LTable); ok {
		merged := map[string]string{}
		for k, val := range env {
			merged[k] = val
		}
		extra.ForEach(func(k, val lua.LValue) { merged[k.String()] = val.String() })
		env = merged
	}
	ctx := v.ctx
	timeout := time.Duration(0)
	if n, ok := optNumber(o, "timeout"); ok {
		if n <= 0 {
			raise(L, "sh", EINVAL, "timeout must be > 0")
		}
		timeout = time.Duration(n * float64(time.Second))
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := shell.Command(ctx, dir, env, script)
	buf := shell.NewOutputBuffer(shOutputLimit)
	cmd.Stdout, cmd.Stderr = buf, buf
	if stdin, ok := optString(o, "stdin"); ok {
		cmd.Stdin = strings.NewReader(stdin)
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
		case v.ctx.Err() != nil:
			v.ctxErr("sh")
		case ctx.Err() != nil:
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
	if optBool(o, "check") && code != 0 {
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
