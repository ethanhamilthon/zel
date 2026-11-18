package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zel/internal/entities"
)

// sessionClosedTimeout is the shutdown budget of a session: running its
// session_closed hooks.
const sessionClosedTimeout = 5 * time.Second

// lifecycle is the shutdown budget of a session.
type lifecycle struct {
	// life ends with the session's shutdown budget (see startBudget).
	life     context.Context
	stopLife context.CancelFunc
	budget   sync.Once
}

func newLifecycle() *lifecycle {
	life, stop := context.WithCancel(context.Background())
	return &lifecycle{life: life, stopLife: stop}
}

// startBudget starts the session's shutdown budget, once: after
// sessionClosedTimeout the lifecycle context ends, so session_closed hooks
// still pending are skipped.
func (l *lifecycle) startBudget() {
	l.budget.Do(func() { time.AfterFunc(sessionClosedTimeout, l.stopLife) })
}

// close runs the session_closed hooks (sessionClosed may be nil) within the
// shutdown budget, which starts here. Close is idempotent.
func (l *lifecycle) close(sessionClosed func(ctx context.Context)) {
	l.startBudget()
	if sessionClosed != nil {
		sessionClosed(l.life)
	}
	l.stopLife()
}

var IsNotice = entities.IsNotice

// eventsLog is the file of the session that keeps the notices, errors and hook runs.
func (s *session) eventsLog() string { return filepath.Join(s.sessionDir(), "trace.log") }

func (a *Runtime) LogEvent(kind, text string) { a.session().LogEvent(kind, text) }

// LogEvent appends one notice or error to the session's events log; failures are ignored.
func (s *session) LogEvent(kind, text string) {
	if s.home == "" || s.id == "" || s.store == nil {
		return
	}
	path := s.eventsLog()
	s.logMu.Lock()
	defer s.logMu.Unlock()
	// A hook that ends after the shutdown must not bring back the folder of a
	// session that release removed: only an existing folder gets the line.
	if !s.ended.Load() && os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	text = strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n    ")
	fmt.Fprintf(file, "%s %s %s\n", time.Now().Format(time.RFC3339Nano), kind, text)
}
