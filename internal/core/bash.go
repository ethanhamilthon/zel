package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"zel/internal/shell"
)

// bashOutputLimit is the safety cap of one call's output: head and tail stay.
const bashOutputLimit = 1 << 20

// runBash runs one bash call of the agent: the bash_before hooks may block or
// rewrite it, the bash_after hooks may add to its output.
func (s *session) runBash(ctx context.Context, command string) string {
	if s.hasHooks("bash_before") {
		res := s.runHooks(ctx, "bash_before", map[string]any{"command": command})
		if res.blocked {
			return "[blocked by a hook: " + strings.TrimSpace(res.reason) + "]"
		}
		command, _ = res.input["command"].(string)
	}
	timeout := time.Duration(s.Settings().BashTimeout) * time.Second
	end := s.beginActivity(bashLabel(Redact(s.Settings(), command)))
	started := time.Now()
	out, code, timedOut := s.execBash(ctx, command, timeout)
	end()
	if s.hasHooks("bash_after") {
		res := s.runHooks(ctx, "bash_after", map[string]any{"command": command, "out": out, "code": code,
			"ms": time.Since(started).Milliseconds(), "timed_out": timedOut})
		for _, extra := range res.outs {
			out = strings.TrimRight(out, "\n") + "\n" + extra
		}
	}
	return out
}

// execBash runs the script with `bash -c` in the session folder; the whole
// process group dies at the timeout or when ctx ends.
func (s *session) execBash(ctx context.Context, command string, timeout time.Duration) (out string, code int, timedOut bool) {
	if ctx.Err() != nil {
		return "[cancelled]", 130, false
	}
	cmd := shell.Command(context.Background(), s.dir, s.shellEnv(), command)
	buffer := shell.NewOutputBuffer(bashOutputLimit)
	w := &lockedWriter{w: buffer}
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		return err.Error() + "\n[exit 127]", 127, false
	}
	done := make(chan error, 1)
	go func() { done <- shell.Reap(cmd) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	suffix := ""
	select {
	case err = <-done:
		code = exitCode(err)
		if code != 0 {
			suffix = fmt.Sprintf("[exit %d]", code)
		}
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		code, timedOut, suffix = 124, true, fmt.Sprintf("[timeout after %s]", timeout)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		code, suffix = 130, "[cancelled]"
	}
	w.mu.Lock()
	out = buffer.Text()
	w.mu.Unlock()
	if suffix != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += suffix
	}
	return out, code, timedOut
}

// exitCode is the exit status of a finished command; a signal counts as 128+n.
func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// Bounds of the working indicator label: the indicator cuts lines to its width
// and cycles through them, so longer ones are not needed.
const (
	labelLines    = 100
	labelLineRune = 200
)

// bashLabel is what the working indicator shows for a call: the command as is,
// its lines without empty ones and # comments, one per row.
func bashLabel(command string) string {
	var lines []string
	for _, l := range strings.Split(command, "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if r := []rune(l); len(r) > labelLineRune {
			l = string(r[:labelLineRune-1]) + "…"
		}
		if lines = append(lines, l); len(lines) == labelLines {
			break
		}
	}
	return strings.Join(lines, "\n")
}
