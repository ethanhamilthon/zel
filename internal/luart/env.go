// Package luart owns Zel's config VM, the agent (session) VM and the VMs of
// async hooks and commands. Config, hook and command VMs have the hardened Lua
// standard library; agent tool chunks run in a separate allowlisted REPL
// environment with pure Lua base/text/table/math/coroutine plus config.functions,
// while copied config functions retain their private library environment.
package luart

type SessionInfo struct {
	ID, Name, Cwd, Root, Mode       string // Cwd is the session start folder
	Modes                           []string
	Depth                           int
	Model, Effort, Provider, TmpDir string
}

// Env is the session-owned host. The runtime may call it concurrently from
// the agent and from hooks.
type Env interface {
	Info() SessionInfo
	ShellEnv() []string // KEY=VALUE for the children of io.popen and os.execute
	Log(kind, msg string)
}
