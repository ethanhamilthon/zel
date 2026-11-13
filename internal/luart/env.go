// Package luart embeds the Lua runtime: one fresh VM per tool call with the host
// API, and a long-lived config VM that runs init.lua plugins and hooks.
package luart

import (
	"context"
	"time"
)

// SessionInfo describes the session a call runs in.
type SessionInfo struct {
	ID, Cwd, Root, Parent string
	Depth                 int
	Model, Effort         string
	Home                  string   // ~/.zel
	TmpDir                string   // session scratch directory
	LuaPath               []string // extra package.path directories
}

// TaskOpts are the options of tasks.spawn.
type TaskOpts struct {
	Name, Cwd string
	Env       map[string]string
	Timeout   time.Duration
}

// AgentOpts are the options of agents.spawn.
type AgentOpts struct {
	Prompt, Model, Effort, Provider, Cwd string
	Timeout                              time.Duration
}

// JobInfo is one row of tasks.list / agents.list.
type JobInfo struct {
	ID, Kind, Name, Cmd, Model, State, Prompt string
}

// Job is a background task or subagent.
type Job interface {
	ID() string
	Kind() string // "task" | "agent"
	Status() string
	// Wait blocks until the job ends, the timeout passes (zero: no limit) or ctx
	// ends. A wait that gives up must not kill the job.
	Wait(ctx context.Context, timeout time.Duration) (map[string]any, bool)
	// Result is the final result without blocking.
	Result() (map[string]any, bool)
	Tail(n int) string
	Stop()
}

// StateStore keeps the values behind state.get/set/list.
type StateStore interface {
	Get(scope, key string) (value string, ok bool, err error)
	Set(scope, key, value string) error
	Delete(scope, key string) error
	List(scope, prefix string) ([]string, error)
	Size(scope string) (int, error)
}

// Env is everything the host functions need from the application.
type Env interface {
	Info() SessionInfo
	ShellEnv() map[string]string
	State() StateStore
	SpawnTask(cmd string, o TaskOpts) (Job, error)
	SpawnAgent(o AgentOpts) (Job, error)
	GetJob(kind, id string) (Job, bool)
	ListJobs(kind string) []JobInfo
	View(path string) error
	Notice(msg string)
	Log(level, msg string)
	Rename(name string) error
}
