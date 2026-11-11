package shell

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// shCommand runs script with sh -c in the session directory with ZEL_* variables.
func HookCommand(ctx context.Context, dir, script string, env, extra map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	// Do not leak another hook's event payload into this event.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ZEL_") && key != "ZEL_CONFIG" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd
}
