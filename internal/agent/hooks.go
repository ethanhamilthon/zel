package agent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zel/internal/shell"
)

var HookEvents = []string{"app_start", "app_exit", "session_start", "session_close", "first_message", "user_message", "turn_start", "turn_end", "bash_start", "bash_end", "model_request_start", "model_request_end", "error"}

// Hooks run in layer order. Only the turn goroutine consumes their output;
// output never schedules a turn by itself.
type hookRunner struct {
	mu     sync.Mutex
	pasted []string
}

func (a *Agent) fire(event string, vars map[string]string) {
	if a.hooks == nil {
		return
	}
	settings := a.Settings()
	closing := event == "session_close" || event == "app_exit"
	for _, h := range settings.Hooks {
		if !slices.Contains(h.Events, event) {
			continue
		}
		env := map[string]string{
			"ZEL_EVENT": event, "ZEL_PROVIDER": settings.Provider, "ZEL_MODEL": settings.Model,
			"ZEL_EFFORT": settings.Effort, "ZEL_MODE": a.opts.Mode,
			"ZEL_CONTEXT_TOKENS": strconv.FormatInt(a.ContextTokens(), 10),
		}
		for k, v := range vars {
			env[k] = v
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(HookTimeout)*time.Second)
		cmd := a.shCommand(ctx, h.Run, env)
		if h.Dir != "" {
			cmd.Dir = h.Dir
		}
		// Kill descendants too; inherited pipes must not hold up shutdown.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		out, stderr := shell.NewOutputBuffer(settings.ShellOutput), shell.NewOutputBuffer(settings.ShellOutput)
		cmd.Stdout, cmd.Stderr = out, stderr
		err := cmd.Run()
		cancel()
		text := strings.TrimSpace(out.Text())
		if text != "" {
			a.LogEvent("hook", Redact(settings, h.Name+"\n"+text))
		}
		if text := strings.TrimSpace(stderr.Text()); text != "" {
			a.LogEvent("error", Redact(settings, h.Name+" stderr:\n"+text))
		}
		if err != nil {
			a.LogEvent("error", Redact(settings, fmt.Sprintf("Hook %s failed: %v", h.Name, err)))
			continue
		}
		if !closing && text != "" {
			a.hooks.mu.Lock()
			a.hooks.pasted = append(a.hooks.pasted, "<hook-output event=\""+event+"\">\n"+strings.TrimRight(out.Text(), "\n")+"\n</hook-output>")
			a.hooks.mu.Unlock()
		}
	}
}

func (a *Agent) takePasted() []string {
	if a.hooks == nil {
		return nil
	}
	a.hooks.mu.Lock()
	defer a.hooks.mu.Unlock()
	pasted := a.hooks.pasted
	a.hooks.pasted = nil
	return pasted
}
func millis(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
