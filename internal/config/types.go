package config

import (
	"regexp"
	"time"
)

// The two base session modes: main (the TUI) and headless (zel -p). A session
// may add one custom mode with --mode; it then runs in both.
const (
	ModeMain     = "main"
	ModeHeadless = "headless"
)

var (
	entryName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// ModeName is what a mode may be called with --mode.
	ModeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// HookTimeout is the default limit of one hook.
const HookTimeout = 60 * time.Second

// Events are the hook events, in documentation order.
var Events = []string{
	"session_created", "session_resumed", "session_closed", "config_changed",
	"message", "notification", "turn_start", "turn_end", "idle",
	"lua_before", "lua_after",
	"compaction", "model_changed", "error",
}

// System is one named system prompt entry from systems in Lua config.
// Source is the config file that defined it (absolute path).
type System struct{ Name, Text, Source string }

// Hook is one hook entry from hooks in Lua config. Source is
// "<config file>:<line>" of its function.
type Hook struct {
	Events []string
	Async  bool
	Ref    int // index in luart's hook table; opaque for config
	Source string
}

// Function is one function of the agent VM from functions in Lua config.
type Function struct{ Name, Description, Source string }

// Command is one entry of commands in Lua config: a menu item, optionally with
// a key. Ref is opaque for config (index in luart's table).
type Command struct {
	Name, Description, Key, Source string
	Ref                            int
}

// Paths are the configuration file locations of a session.
type Paths struct {
	Files    []string // in load order: the defaults (if kept), then the --config files
	Required []string // the --config files: a missing one is an error, a missing default is skipped
	Connect  string
}

// Config is the snapshot of the config files of a session.
type Config struct {
	Paths            Paths
	Settings         Settings
	SettingSources   map[string]string // dotted keys relative to settings
	Systems          []System
	Prompts          map[string]string
	PromptSources    map[string]string
	Hooks            []Hook
	Functions        []Function
	Commands         []Command
	Files            []string // every file read, loads included
	Warnings, Errors []string
}
