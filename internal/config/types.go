package config

import "time"

// Instruction is one [instruction.NAME]: text for the system prompt, already
// expanded, and the session modes it applies in.
type Instruction struct {
	Name, Text string
	Modes      []string
}

// Completion is one [completion.NAME]: the raw text (${{ }} is expanded when it
// is used) and the session modes that offer it.
type Completion struct {
	Text  string
	Modes []string
}

// The two base session modes: main (the TUI) and headless (zel -p). A session
// may add one custom mode with --mode; it then runs in both.
const (
	ModeMain     = "main"
	ModeHeadless = "headless"
)

// InModes reports whether an entry with the modes want applies in a session
// running in the modes active. An empty want means every mode.
func InModes(want, active []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, m := range active {
		for _, w := range want {
			if m == w {
				return true
			}
		}
	}
	return false
}

// Hook is one [hook.NAME] for one event. Run is never expanded: it is bash already.
type Hook struct {
	Name, Event string
	Run         string
	Match       string        // regexp; empty matches everything
	Timeout     time.Duration // zero: HookTimeout
	Async       bool          // start and do not wait; output and exit code are ignored
	Modes       []string      // session modes it runs in (default main only)
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

// MatchField is the field of the event data a hook's match regexp is tested
// against; hooks of other events cannot have a match.
var MatchField = map[string]string{
	"message": "text", "notification": "text", "turn_start": "text",
	"bash_before": "command", "bash_after": "command",
}

// Config is the snapshot of the config file of a session.
type Config struct {
	Path         string
	Settings     Settings
	Instructions []Instruction
	Hooks        map[string][]Hook     // by event, in file order
	Completions  map[string]Completion // by name
	Warnings     []string              // non-fatal problems, such as a failed ${{ }}
	Errors       []string              // why the file could not be read ("file:line: msg")
}
