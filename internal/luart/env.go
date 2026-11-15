// Package luart embeds the Lua runtime: the session's main VM for tool calls,
// task VMs, the host API, and a long-lived config VM that runs init.lua tools and hooks.
package luart

import (
	"context"
	"errors"
	"io"
	"time"
)

// SessionInfo describes the session a call runs in.
type SessionInfo struct {
	ID, Cwd, Root, Parent string
	Mode                  string // "main", "agent" or "headless"
	Model, Effort         string
	Home                  string   // ~/.zel
	TmpDir                string   // session scratch directory
	LuaPath               []string // extra package.path directories
}

// ModelInfo is one model of a provider, as models() lists it.
type ModelInfo struct {
	Provider, Model string
	ContextWindow   int64 // tokens; zero when the provider does not say
}

// TaskOpts are the options of tasks.spawn.
type TaskOpts struct {
	Name    string
	Timeout time.Duration // zero: no limit
}

// TaskFunc is the body of a task. It runs on its own goroutine in its own VM
// until it returns or ctx ends; out receives what it prints. The value is the
// function's first return value.
type TaskFunc func(ctx context.Context, out io.Writer) (value any, err error)

// AgentOpts are the options of new_agent{} and send_agent().
type AgentOpts struct {
	Prompt, Model, Effort, Provider, Cwd string
	Resume                               string // send_agent: the session of the subagent that gets Prompt
	Timeout                              time.Duration
	Waited                               bool // the caller waits for the result: no notification is queued
}

// JobInfo is one row of tasks.list. Name is set for tasks, Model
// and Prompt for agents.
type JobInfo struct {
	ID, Name, Model, State, Prompt string
}

// ErrNoJob is returned for an ID the namespace does not know.
var ErrNoJob = errors.New("no such job")

// Jobs is the lifecycle of one namespace of background work, "tasks" or
// "agents". The namespaces are independent: an ID belongs to exactly one of
// them and stays valid for the whole session. Every method returns ErrNoJob
// for an unknown ID.
type Jobs interface {
	List() []JobInfo
	Get(id string) (JobInfo, bool)
	// Wait blocks until the job ends, the timeout passes (zero: no limit) or ctx
	// ends. ok is false when it gave up; that must not stop the job. The result
	// is delivered here instead of as a queued message, exactly once.
	Wait(ctx context.Context, id string, timeout time.Duration) (res map[string]any, ok bool, err error)
	// Result is the final result without blocking; ok is false while running.
	Result(id string) (res map[string]any, ok bool, err error)
	Tail(id string, n int) (string, error)
	Stop(id string) error
}

// Tasks is the tasks namespace: Lua functions in the background.
type Tasks interface {
	Jobs
	Spawn(run TaskFunc, o TaskOpts) (id string, err error)
}

// Agents is the agents namespace: subagents that run inside the application.
type Agents interface {
	Jobs
	Spawn(o AgentOpts) (id string, err error)
}

// Env is everything the host functions need from the application.
type Env interface {
	Info() SessionInfo
	ShellEnv() map[string]string
	Tasks() Tasks
	Agents() Agents
	View(path string) error
	Notice(msg string)
	Log(level, msg string)
	Rename(name string) error
	// Models lists the models of the configured providers. Providers that fail
	// are reported by name in failures; they do not fail the list.
	Models(ctx context.Context) (models []ModelInfo, failures map[string]string)
	// Choose asks the user to pick one of the options and blocks until he does
	// or ctx ends; ok is false when he cancels.
	Choose(ctx context.Context, title string, options []string) (picked string, ok bool, err error)
	// Begin reports a host operation that starts now, with a label that already
	// hides secrets. The returned end must be called exactly once when it ends,
	// however it ends. Background operations (tasks) are not foreground activity.
	Begin(label string, background bool) (end func())
}
