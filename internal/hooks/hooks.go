package hooks

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"zel/internal/config"
	"zel/internal/shell"
)

// Hooks run in layer order. Only the turn goroutine consumes their output;
// output never schedules a turn by itself.
type Runner struct {
	mu     sync.Mutex
	pasted []string
}

type Options struct {
	Hooks       []config.Hook
	Dir         string
	Env         map[string]string
	OutputLimit int
	Timeout     time.Duration
	Log         func(string, string)
}

func (r *Runner) Run(ctx context.Context, event string, options Options) {
	closing := event == "session_close" || event == "app_exit"
	for _, h := range options.Hooks {
		if !slices.Contains(h.Events, event) {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, options.Timeout)
		cmd := shell.HookCommand(ctx, options.Dir, h.Run, options.Env, map[string]string{"ZEL_EVENT": event})
		if h.Dir != "" {
			cmd.Dir = h.Dir
		}
		out, stderr := shell.NewOutputBuffer(options.OutputLimit), shell.NewOutputBuffer(options.OutputLimit)
		cmd.Stdout, cmd.Stderr = out, stderr
		err := cmd.Run()
		cancel()
		text := strings.TrimSpace(out.Text())
		if text != "" {
			options.Log("hook", h.Name+"\n"+text)
		}
		if text := strings.TrimSpace(stderr.Text()); text != "" {
			options.Log("error", h.Name+" stderr:\n"+text)
		}
		if err != nil {
			options.Log("error", fmt.Sprintf("Hook %s failed: %v", h.Name, err))
			continue
		}
		if !closing && text != "" {
			r.mu.Lock()
			r.pasted = append(r.pasted, "<hook-output event=\""+event+"\">\n"+strings.TrimRight(out.Text(), "\n")+"\n</hook-output>")
			r.mu.Unlock()
		}
	}
}

func (r *Runner) Take() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pasted := r.pasted
	r.pasted = nil
	return pasted
}
