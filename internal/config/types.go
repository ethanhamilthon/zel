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
type System struct {
	Name, Text string
}

// Hook is one hook entry from hooks in Lua config.
type Hook struct {
	Events []string
	Async  bool
	Ref    int    // index in luart's hook table; opaque for config
	Source string // "init.lua:12" for diagnostics
}

// Function is one custom tool function from functions in Lua config.
type Function struct {
	Name, Description string
}

// Paths are the configuration file locations of a session.
type Paths struct {
	Init, Local, Connect string // Local "" = none
}

// Config is the snapshot of the config files of a session.
type Config struct {
	Paths            Paths
	Settings         Settings
	Systems          []System
	Prompts          map[string]string
	Hooks            []Hook
	Functions        []Function
	Files            []string // every file read: watched for hot reload
	Warnings, Errors []string
}
