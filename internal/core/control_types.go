package core

import (
	"fmt"
	"time"
)

// Event is one ordered notification from the controller. Consumers receive
// events in emission order and must not call the controller from the callback.
type Event struct {
	Kind    string // started, agent, notice, error, done or session
	Agent   *TurnEvent
	Text    string   // started: the input text; notice and error: the text; done (ok): final assistant text
	Status  string   // done: ok, error or cancelled
	Error   string   // done: failure text
	Unsent  []Input  // done: queued inputs the model did not accept
	Title   string   // started, done and rename notices: the session title
	Context int64    // done: estimated context tokens
	Session *Session // session: the new session snapshot
}

// Session is a snapshot of the current session.
type Session struct {
	ID      string
	Title   string
	Context int64
	Records []Record
}

// Error codes: busy and setup_required are expected states a client shows as
// notices; invalid and failed are errors.
const (
	CodeBusy     = "busy"
	CodeSetup    = "setup_required"
	CodeInvalid  = "invalid"
	CodeFailed   = "failed"
	CodeNotFound = "not_found"
	CodeDenied   = "denied"
)

// Error is a rejection. Code tells the interface how to show it; Message is user-facing and never contains credentials.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Info describes the current session for the TUI information page.
type Info struct {
	Session, Title, Project string
	Provider, Model, Effort string
	Config                  string
	Messages, Requests      int   // user messages, model requests with usage
	Context                 int64 // estimated context tokens
	Usage                   Usage
	Subagents               []SubagentRun // the zel -p children of the session (running and finished), oldest first
}

type Status struct {
	Busy       bool
	Cancelling bool
	Started    time.Time
	Queued     []Input
	Session    string
	Activity   string    // foreground host operation of the turn, one row per line; "" before the first
	Live       bool      // Activity is still running, not only the last one of the turn
	LiveSince  time.Time // when the running Activity began
}
