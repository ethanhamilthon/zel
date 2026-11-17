package config

import "time"

// Instruction is one entry of instructions: text for the system prompt, already
// expanded. Main and Headless say in which session modes it applies.
type Instruction struct {
	Text           string
	Main, Headless bool
}

// Hook is one command run on an event. Run is never expanded: it is bash already.
type Hook struct {
	Event          string
	Run            string
	Match          string        // regexp; empty matches everything
	Timeout        time.Duration // zero: HookTimeout
	Async          bool          // start and do not wait; output and exit code are ignored
	Main, Headless bool          // session modes it runs in (default main only)
}

// HookTimeout is the default limit of one hook.
const HookTimeout = 60 * time.Second

// ExprTimeout is the limit of one ${{ }} expression.
const ExprTimeout = 10 * time.Second

// Events are the hook events, in documentation order.
var Events = []string{
	"session_created", "session_resumed", "session_closed", "config_changed",
	"message", "notification", "turn_start", "turn_end", "idle",
	"bash_before", "bash_after",
	"compaction", "model_changed", "error",
}

// Config is the snapshot of the config file of a session.
type Config struct {
	Path         string
	Settings     Settings
	Instructions []Instruction
	Hooks        map[string][]Hook // by event, in file order
	Completions  map[string]string // name -> raw text; ${{ }} is expanded when it is used
	Warnings     []string          // non-fatal problems, such as a failed ${{ }}
	Errors       []string          // why the file could not be read ("file:line: msg")
}
