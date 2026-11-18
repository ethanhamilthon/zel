package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"zel/internal/ipc"
	"zel/internal/shell"
)

// Exit codes of the zel subcommands, by error code.
var exitCodes = map[string]int{"EFAIL": 1, "EINVAL": 2, "ENOENT": 3, "ENOMATCH": 4, "EAMBIG": 5, "EDENIED": 6, "ELIMIT": 7, "ETIMEOUT": 124, "ECANCELED": 130}

func ok(out string) ipc.Response { return ipc.Response{OK: true, Out: out} }

func bad(code, format string, args ...any) ipc.Response {
	return ipc.Response{Code: exitCodes[code], Error: code + ": " + fmt.Sprintf(format, args...)}
}

// errResponse turns an error into a response; "CODE: msg" errors keep their code.
func errResponse(err error) ipc.Response {
	msg := err.Error()
	code, _, _ := strings.Cut(msg, ":")
	if n, known := exitCodes[code]; known {
		return ipc.Response{Code: n, Error: msg}
	}
	return ipc.Response{Code: 1, Error: "EFAIL: " + msg}
}

// serveSocket answers the zel -p children of this session.
func (s *session) serveSocket(ctx context.Context, req ipc.Request) ipc.Response {
	if s.ended.Load() {
		return bad("EFAIL", "the session has ended")
	}
	var a struct {
		Session, Provider, Model, Effort, Prompt string
		N, PID                                   int
		Context, Input, Output, Cached           int64
	}
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return bad("EINVAL", "bad arguments: %v", err)
		}
	}
	switch req.Op {
	case "child.start":
		s.childStarted(SubagentRun{ID: a.Session, Parent: s.id, Started: time.Now(), Provider: a.Provider, Model: a.Model, Effort: a.Effort, Command: a.Prompt}, a.PID)
		return ok("")
	case "child.usage":
		s.childUsage(a.Session, func(r *SubagentRun) {
			r.Provider, r.Model, r.Effort = cmp.Or(a.Provider, r.Provider), cmp.Or(a.Model, r.Model), cmp.Or(a.Effort, r.Effort)
			r.Context, r.Usage, r.Requests = a.Context, Usage{Input: a.Input, Output: a.Output, Cached: a.Cached}, a.N
		})
		return ok("")
	case "child.end":
		if a.Model != "" || a.Context > 0 || a.Input > 0 || a.Output > 0 || a.N > 0 {
			s.childUsage(a.Session, func(r *SubagentRun) {
				r.Provider, r.Model, r.Effort = cmp.Or(a.Provider, r.Provider), cmp.Or(a.Model, r.Model), cmp.Or(a.Effort, r.Effort)
				r.Context, r.Usage, r.Requests = a.Context, Usage{Input: a.Input, Output: a.Output, Cached: a.Cached}, a.N
			})
		}
		s.childEnded(a.Session)
		return ok("")
	}
	return bad("EINVAL", "unknown op %q", req.Op)
}

// sessionInfo is zel session: text in Out, the same as JSON in Data.
func (s *session) view(path string) error {
	if s.ended.Load() {
		return errors.New("session has ended")
	}
	image, err := shell.CaptureImage(path)
	if err != nil {
		return err
	}
	if !s.slots.reserve(1) {
		return fmt.Errorf("at most %d new images per model request", MaxPendingImages)
	}
	if !s.queue.Push(withImages(notification("view: "+filepath.Base(path)+" attached"), image)) {
		s.slots.release(1)
		return fmt.Errorf("input queue is full")
	}
	return nil
}
