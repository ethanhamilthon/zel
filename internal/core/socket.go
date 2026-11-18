package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// serveSocket answers CLI requests and tracks zel -p children.
func (s *session) serveSocket(_ context.Context, req ipc.Request) ipc.Response {
	if s.ended.Load() {
		return bad("EFAIL", "the session has ended")
	}
	var a struct {
		Session, Provider, Model, Effort, Prompt string
		SessionArgs
		N, PID                         int
		Context, Input, Output, Cached int64
	}
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return bad("EINVAL", "bad arguments: %v", err)
		}
	}
	switch req.Op {
	case "steer":
		return s.socketSteer(req.Cwd, a.Text, a.Files)
	case "status":
		if !s.indicators.setCall(a.Text) {
			return bad("EFAIL", "no Lua call is running")
		}
		return ok("")
	case "status.new":
		return ok(s.indicators.add(a.Text))
	case "status.update":
		if !s.indicators.update(a.ID, a.Text, a.Done) {
			return bad("ENOENT", "unknown indicator %q", a.ID)
		}
		return ok("")
	case "choice":
		if s.mode() == "headless" {
			return bad("EDENIED", "no UI to ask the user in")
		}
		if strings.TrimSpace(a.Text) == "" || len(a.Options) == 0 {
			return bad("EINVAL", "choice requires a question and options")
		}
		for _, option := range a.Options {
			if strings.TrimSpace(option) == "" {
				return bad("EINVAL", "choice options must not be empty")
			}
		}
		id, err := s.ask.ask(s, a.Text, a.Options)
		if err != nil {
			return errResponse(err)
		}
		return ok(fmt.Sprintf("c%d", id))
	case "print":
		if a.Text == "" {
			return ok("")
		}
		if err := s.print(a.Text); err != nil {
			return errResponse(err)
		}
		return ok("")
	case "session":
		return ipc.Response{OK: true, Data: s.socketSession()}
	case "rename":
		if err := s.Rename(a.Name); err != nil {
			return errResponse(err)
		}
		return ok("")
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

func (s *session) socketSession() json.RawMessage {
	st := s.Settings()
	return raw(struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Cwd      string   `json:"cwd"`
		Root     string   `json:"root"`
		Mode     string   `json:"mode"`
		Modes    []string `json:"modes"`
		Depth    int      `json:"depth"`
		Model    string   `json:"model"`
		Effort   string   `json:"effort"`
		Provider string   `json:"provider"`
	}{s.id, cmp.Or(s.CustomTitle(), s.DefaultTitle()), s.dir, s.dir,
		s.mode(), s.modes(), s.depth, st.Model, st.Effort, st.Provider})
}

func (s *session) socketSteer(cwd, text string, files []string) ipc.Response {
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return bad("EINVAL", "steer requires text or images")
	}
	if len(files) > MaxPendingImages {
		return bad("ELIMIT", "at most %d new images per model request", MaxPendingImages)
	}
	if strings.TrimSpace(text) == "" {
		text = "Image sent via zel steer"
	}
	input := notification(text)
	for _, file := range files {
		path, err := ResolvePath(file)
		if err != nil {
			return bad("EINVAL", "%v", err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cmp.Or(cwd, s.dir), path)
		}
		image, err := shell.CaptureImage(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return bad("ENOENT", "%v", err)
			}
			return bad("EINVAL", "%v", err)
		}
		input.Images = append(input.Images, image)
	}
	n := len(input.Images)
	if n > 0 && !s.slots.reserve(n) {
		return bad("ELIMIT", "at most %d new images per model request", MaxPendingImages)
	}
	if !s.queue.Push(input) {
		if n > 0 {
			s.slots.release(n)
		}
		return bad("ELIMIT", "input queue is full")
	}
	return ok("")
}
