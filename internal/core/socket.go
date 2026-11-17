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

// serveSocket answers the zel subcommands run by this session's bash, tasks and hooks.
func (s *session) serveSocket(ctx context.Context, req ipc.Request) ipc.Response {
	if s.ended.Load() {
		return bad("EFAIL", "the session has ended")
	}
	var a struct {
		ID, Name, Cmd, Cwd, Dir, Title, Session, Provider, Model, Effort, Prompt string
		IDs, Options, Paths                                                      []string
		Timeout                                                                  float64
		N                                                                        int
		PID                                                                      int
		Context, Input, Output, Cached                                           int64
	}
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return bad("EINVAL", "bad arguments: %v", err)
		}
	}
	t := s.tasks
	switch req.Op {
	case "task.run":
		if strings.TrimSpace(a.Cmd) == "" {
			return bad("EINVAL", "task run needs a command")
		}
		j, err := t.spawn(a.Cmd, startOpts{name: a.Name, cwd: a.Cwd, timeout: time.Duration(a.Timeout * float64(time.Second))})
		if err != nil {
			return errResponse(err)
		}
		return ok(j.id + "\n")
	case "task.list":
		var lines []string
		for _, j := range t.list() {
			lines = append(lines, j.line())
		}
		if len(lines) == 0 {
			return ok("")
		}
		return ok(strings.Join(lines, "\n") + "\n")
	case "task.get", "task.tail", "task.stop":
		j, err := t.find(a.ID)
		if err != nil {
			return errResponse(err)
		}
		switch req.Op {
		case "task.get":
			return ok(j.result(taskResultLines) + "\n")
		case "task.tail":
			return ok(tailLines(j.log, a.N) + "\n")
		}
		j.Stop()
		return ok("")
	case "task.wait":
		var jobs []*job
		for _, id := range a.IDs {
			j, err := t.find(id)
			if err != nil {
				return errResponse(err)
			}
			jobs = append(jobs, j)
		}
		if len(jobs) == 0 {
			return bad("EINVAL", "task wait needs task ids")
		}
		deadline := time.Now().Add(time.Duration(a.Timeout * float64(time.Second)))
		var out []string
		pending := 0
		for _, j := range jobs {
			left := time.Duration(0)
			if a.Timeout > 0 {
				if left = time.Until(deadline); left <= 0 {
					left = time.Nanosecond
				}
			}
			if !j.Wait(ctx, left) {
				pending++
			}
			out = append(out, j.result(taskResultLines))
		}
		if ctx.Err() != nil {
			return bad("ECANCELED", "the wait was cancelled")
		}
		r := ok(strings.Join(out, "\n\n") + "\n")
		if pending > 0 {
			r.OK, r.Code, r.Error = false, 124, fmt.Sprintf("ETIMEOUT: %d of %d tasks still run", pending, len(jobs))
		}
		return r
	case "choice.ask":
		if s.mode() != "main" {
			return bad("EDENIED", "zel choice needs the user: not available in zel -p")
		}
		if strings.TrimSpace(a.Title) == "" || len(a.Options) < 1 {
			return bad("EINVAL", `usage: zel choice "Question" "Option" ...`)
		}
		id, err := s.ask.ask(s, a.Title, a.Options)
		if err != nil {
			return errResponse(err)
		}
		return ok(fmt.Sprintf("c%d\n", id))
	case "view.add":
		for _, p := range a.Paths {
			if !filepath.IsAbs(p) {
				p = filepath.Join(cmp.Or(a.Cwd, req.Cwd, s.bashCwd()), p)
			}
			if err := s.view(p); err != nil {
				return bad("EFAIL", "%s: %v", p, err)
			}
		}
		return ok("")
	case "cd":
		path, err := s.changeDir(a.Dir, cmp.Or(a.Cwd, req.Cwd))
		if err != nil {
			return errResponse(err)
		}
		return ok(path + "\n")
	case "session.get":
		return s.sessionInfo()
	case "session.rename":
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

// sessionInfo is zel session: text in Out, the same as JSON in Data.
func (s *session) sessionInfo() ipc.Response {
	st := s.Settings()
	parent := ""
	if s.parent != nil {
		parent = s.parent.session
	}
	s.histMu.RLock()
	title := s.title
	if title == "" {
		title = firstUserLine(s.History)
	}
	s.histMu.RUnlock()
	info := map[string]any{"id": s.id, "cwd": s.dir, "parent": parent, "depth": s.depth, "mode": strings.Join(s.modes(), "+"),
		"provider": st.Provider, "model": st.Model, "effort": st.Effort, "title": title, "dir": s.sessionDir(), "config": s.path}
	if bash := s.bashCwd(); bash != s.dir {
		info["bash_cwd"] = bash
	}
	data, _ := json.Marshal(info)
	var b strings.Builder
	for _, k := range []string{"id", "title", "cwd", "bash_cwd", "mode", "parent", "depth", "provider", "model", "effort", "dir"} {
		if v, has := info[k]; has {
			fmt.Fprintf(&b, "%s: %v\n", k, v)
		}
	}
	fmt.Fprintf(&b, "config: %s\n", s.path)
	r := ok(b.String())
	r.Data = data
	return r
}

// view attaches an image to the next model request.
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
