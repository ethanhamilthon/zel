package core

import (
	"context"
	"strconv"
	"time"
	"zel/internal/config"
	"zel/internal/hooks"
)

var HookEvents = config.HookEvents

func (a *Runtime) fire(event string, vars map[string]string) {
	a.fireContext(context.Background(), event, vars)
}
func (a *Runtime) fireContext(ctx context.Context, event string, vars map[string]string) {
	if a.hooks == nil {
		return
	}
	settings := a.Settings()
	env := a.shellEnv()
	env["ZEL_HOME"], env["ZEL_CWD"] = a.home, a.Shell.Dir
	env["ZEL_PROVIDER"], env["ZEL_MODEL"], env["ZEL_EFFORT"], env["ZEL_MODE"] = settings.Provider, settings.Model, settings.Effort, a.opts.Mode
	env["ZEL_CONTEXT_TOKENS"] = strconv.FormatInt(a.ContextTokens(), 10)
	for k, v := range vars {
		env[k] = v
	}
	a.hooks.Run(ctx, event, hooks.Options{Hooks: settings.Hooks, Dir: a.Shell.Dir, Env: env, OutputLimit: settings.ShellOutput, Timeout: time.Duration(HookTimeout) * time.Second, Log: func(kind, text string) { a.LogEvent(kind, Redact(settings, text)) }})
}
func (a *Runtime) takePasted() []string { return a.hooks.Take() }
func millis(d time.Duration) string     { return strconv.FormatInt(d.Milliseconds(), 10) }
