// Package luart owns Zel's agent, task and configuration Lua states.
package luart

import (
	"context"
	"io"
	"time"
)

type SessionInfo struct {
	ID, Name, Cwd, Root, Mode       string
	Modes                           []string
	Depth                           int
	Model, Effort, Provider, TmpDir string
}

type ModelInfo struct {
	Provider, Model string
	ContextWindow   int64
}

// Env is the session-owned host. The runtime may call it concurrently from
// agents, tasks and hooks.
type Env interface {
	Info() SessionInfo
	ShellEnv() []string
	Cd(dir string) error
	Attach(path string) error
	Choice(question string, options []string) (string, error)
	Rename(name string) error
	Models(ctx context.Context) ([]ModelInfo, map[string]string)
	Tasks() Tasks
	Log(kind, msg string)
}

type TaskFunc func(ctx context.Context, out io.Writer) (result string, err error)

type TaskInfo struct {
	ID, Name, State string
	Ms              int64
	Result, Error   string
}

// Tasks owns scheduling, lifetimes, logs and notifications. Spawn calls run
// once on success and applies timeout, if nonzero, to the context it passes.
type Tasks interface {
	Spawn(name string, timeout time.Duration, run TaskFunc) (id string, err error)
	List() []TaskInfo
	Get(id string) (TaskInfo, bool)
	Wait(ctx context.Context, ids []string, timeout time.Duration) []TaskInfo
	Tail(id string, n int) (string, error)
	Stop(id string) error
}
