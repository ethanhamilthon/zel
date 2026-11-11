package agent

import (
	"cmp"
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hookRunner runs [hooks.*] commands in the background and keeps the output of
// paste_output hooks until the next turn.
type hookRunner struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	pasted []string
}

// fileVars are passed to hooks as paths of temporary files holding the value.
var fileVars = []string{"DASHY_MESSAGE_FILE", "DASHY_COMMAND_FILE"}

// fire starts the enabled hooks subscribed to event. The environment is captured now,
// on the calling goroutine; the commands run in the background.
func (a *Agent) fire(event string, vars map[string]string) {
	settings := a.Settings()
	var hooks []Hook
	for _, h := range settings.Hooks {
		if h.Enabled && (slices.Contains(h.Events, event) || slices.Contains(h.Events, "*")) {
			hooks = append(hooks, h)
		}
	}
	if len(hooks) == 0 || a.hooks == nil {
		return
	}
	env := map[string]string{"DASHY_EVENT": event, "DASHY_PROVIDER": settings.Provider, "DASHY_MODEL": settings.Model,
		"DASHY_EFFORT": settings.Effort, "DASHY_SESSION_TITLE": cmp.Or(a.title, a.DefaultTitle()), "DASHY_MODE": a.opts.Mode}
	var files []string
	for k, v := range vars {
		if slices.Contains(fileVars, k) {
			f, err := os.CreateTemp("", "dashy-hook-*")
			if err != nil {
				continue
			}
			_, _ = f.WriteString(v)
			f.Close()
			v = f.Name()
			files = append(files, v)
		}
		env[k] = v
	}
	var done sync.WaitGroup
	for _, h := range hooks {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(h.Timeout)*time.Second)
		cmd := a.shCommand(ctx, h.Run, env)
		a.hooks.wg.Add(1)
		done.Add(1)
		go func() {
			defer a.hooks.wg.Done()
			defer done.Done()
			defer cancel()
			out, err := cmd.Output()
			if h.PasteOutput && err == nil && strings.TrimSpace(string(out)) != "" {
				a.hooks.mu.Lock()
				a.hooks.pasted = append(a.hooks.pasted, "<hook-output name=\""+h.Name+"\">\n"+strings.TrimRight(string(out), "\n")+"\n</hook-output>")
				a.hooks.mu.Unlock()
			}
		}()
	}
	if len(files) > 0 {
		go func() {
			done.Wait()
			for _, f := range files {
				os.Remove(f)
			}
		}()
	}
}

// takePasted returns the hook output queued for the next turn.
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
