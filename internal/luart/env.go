// Package luart embeds the Lua runtime: one fresh VM per tool call with the host
// API, and a long-lived config VM that runs init.lua plugins and hooks.
package luart

import (
	"context"
	"errors"
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
	Waited    bool // the caller waits for the result: no <task-result> message is queued
}

// AgentOpts are the options of agents.spawn.
type AgentOpts struct {
	Prompt, Model, Effort, Provider, Cwd string
	Timeout                              time.Duration
	Waited                               bool // the caller waits for the result: no <agent-result> message is queued
}

// JobInfo is one row of tasks.list / agents.list. Cmd is set for tasks, Model
// and Prompt for agents.
type JobInfo struct {
	ID, Name, Cmd, Model, State, Prompt string
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

// Tasks is the tasks namespace: shell commands in the background.
type Tasks interface {
	Jobs
	Spawn(cmd string, o TaskOpts) (id string, err error)
}

// Agents is the agents namespace: subagents that run inside the application.
type Agents interface {
	Jobs
	Spawn(o AgentOpts) (id string, err error)
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
	Tasks() Tasks
	Agents() Agents
	View(path string) error
	Notice(msg string)
	Log(level, msg string)
	Rename(name string) error
}
