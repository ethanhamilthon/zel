package config

import "time"

// Layer is one zel.toml file. Name is "global" (~/.zel/zel.toml), "local"
// (./zel.toml of the working directory) or "config" (a --config file).
type Layer struct{ Name, Path string }

// Instruction is one entry of instructions: text for the system prompt, already
// expanded. Main and Headless say in which session modes it applies.
type Instruction struct {
	Text           string
	Main, Headless bool
	Layer          string // path of the file it came from
}

// Hook is one command run on an event. Run is never expanded: it is bash already.
type Hook struct {
	Event          string
	Run            string
	Match          string        // regexp; empty matches everything
	Timeout        time.Duration // zero: HookTimeout
	Async          bool          // start and do not wait; output and exit code are ignored
	Main, Headless bool          // session modes it runs in (default main only)
	Layer          string        // path of the file it came from
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

// Config is the merged snapshot of the zel.toml layers of a session.
type Config struct {
	Settings     Settings
	Instructions []Instruction     // in layer order
	Hooks        map[string][]Hook // by event, in layer order
	Completions  map[string]string // name -> raw text; ${{ }} is expanded when it is used
	// Sources maps a settings key to the path of the layer that set it last.
	// Keys: "provider", "model", "effort", "settings.<key>", "providers.<name>",
	// "env.<NAME>".
	Sources  map[string]string
	Layers   []Layer  // the layers that exist and were read, in merge order
	Warnings []string // non-fatal problems: a failed ${{ }}, a replaced completion
	Errors   []string // layers that could not be read; they were skipped ("file:line: msg")
}
