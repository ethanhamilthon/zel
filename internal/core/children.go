package core

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zel/internal/ipc"
)

// notifier calls one registered function; it is shared by pointer between a
// runtime and its sessions.
type notifier struct {
	mu sync.Mutex
	fn func()
}

func (n *notifier) set(fn func()) {
	n.mu.Lock()
	n.fn = fn
	n.mu.Unlock()
}

// call runs the function; false when none is set.
func (n *notifier) call() bool {
	n.mu.Lock()
	fn := n.fn
	n.mu.Unlock()
	if fn != nil {
		fn()
	}
	return fn != nil
}

func (n *notifier) notify() { n.call() }

// SubagentRun describes a zel -p child of this session.
type SubagentRun struct {
	ID       string
	Parent   string
	Started  time.Time
	Ended    time.Time // zero while running
	Steps    int
	Provider string
	Model    string
	Effort   string
	Command  string
	Activity string
	Context  int64 // estimated context tokens, as the child last reported
	Usage    Usage // the child's spend so far
	Requests int   // its model requests with usage
}

// ActiveSubagents lists the running zel -p children, oldest first.
func (a *Runtime) ActiveSubagents() []SubagentRun { return a.session().ActiveSubagents() }

// Subagents lists all zel -p children of the session (running and finished), oldest first.
func (a *Runtime) Subagents() []SubagentRun { return a.session().Subagents() }

func (s *session) reapChildrenLocked() {
	for id, run := range s.children {
		if !run.Ended.IsZero() {
			continue
		}
		// A child killed with its bash call or task never says child.end.
		if pid := s.childPIDs[id]; pid > 0 && syscall.Kill(pid, 0) == syscall.ESRCH {
			run.Ended = time.Now()
			s.children[id] = run
			delete(s.childPIDs, id)
		}
	}
}

func sortSubagents(list []SubagentRun) {
	slices.SortFunc(list, func(a, b SubagentRun) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func (s *session) ActiveSubagents() []SubagentRun {
	s.childMu.Lock()
	s.reapChildrenLocked()
	list := make([]SubagentRun, 0, len(s.children))
	for _, run := range s.children {
		if run.Ended.IsZero() {
			list = append(list, run)
		}
	}
	s.childMu.Unlock()
	sortSubagents(list)
	return list
}

func (s *session) Subagents() []SubagentRun {
	if s.store != nil && s.project != "" && s.id != "" {
		if stored, err := s.store.Children(s.project, s.id); err == nil {
			for _, sum := range stored {
				s.childMu.Lock()
				_, known := s.children[sum.ID]
				s.childMu.Unlock()
				if known {
					continue
				}
				v, items, err := s.store.Load(s.project, sum.ID)
				if err != nil {
					continue
				}
				tmp := &session{acts: &activities{}}
				tmp.readItems(items)
				usage, reqs := tmp.SessionUsage()
				run := SubagentRun{
					ID: v.ID, Parent: s.id, Started: v.Created, Ended: sum.Updated,
					Provider: v.Provider, Model: v.Model, Effort: v.Effort,
					Command: sum.Title, Context: tmp.ContextTokens(), Usage: usage, Requests: reqs,
				}
				s.childMu.Lock()
				if s.children == nil {
					s.children, s.childPIDs = map[string]SubagentRun{}, map[string]int{}
				}
				if _, known := s.children[sum.ID]; !known {
					s.children[sum.ID] = run
				}
				s.childMu.Unlock()
			}
		}
	}
	s.childMu.Lock()
	s.reapChildrenLocked()
	list := make([]SubagentRun, 0, len(s.children))
	for _, run := range s.children {
		list = append(list, run)
	}
	s.childMu.Unlock()
	sortSubagents(list)
	return list
}

// OnSubagentsChanged registers fn to run whenever the running tasks or
// children change. fn must not block.
func (a *Runtime) OnSubagentsChanged(fn func()) { a.changes.set(fn) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// parentLink is the session that started this zel -p, from the environment
// its bash gave us.
type parentLink struct {
	session, socket, token  string
	depth                   int
	config                  string
	provider, model, effort string
}

func parentFromEnv() (*parentLink, error) {
	socket, token, id := os.Getenv("ZEL_SOCKET"), os.Getenv("ZEL_TOKEN"), os.Getenv("ZEL_SESSION_ID")
	if socket == "" || token == "" || id == "" {
		return nil, nil
	}
	depth, _ := strconv.Atoi(os.Getenv("ZEL_DEPTH"))
	p := &parentLink{session: id, socket: socket, token: token, depth: depth,
		provider: os.Getenv("ZEL_PROVIDER"), model: os.Getenv("ZEL_MODEL"), effort: os.Getenv("ZEL_EFFORT")}
	p.config = os.Getenv("ZEL_CONFIG")
	return p, nil
}

func (p *parentLink) call(op string, args any) {
	data, _ := json.Marshal(args)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = ipc.Call(ctx, p.socket, ipc.Request{Token: p.token, Op: op, Args: data})
}

// announceChild tells the parent session that this child runs, for its chat row.
func (s *session) announceChild() {
	if s.parent == nil {
		return
	}
	st := s.Settings()
	s.parent.call("child.start", map[string]any{"session": s.id, "provider": st.Provider, "model": st.Model, "effort": st.Effort,
		"prompt": oneLine(s.opts.Prompt), "pid": os.Getpid()})
}

// reportUsage tells the parent this child's model, context and spend, for its
// Info page. It runs after every model response and does not wait.
func (s *session) reportUsage() {
	if s.parent == nil || s.id == "" {
		return
	}
	st := s.Settings()
	usage, requests := s.SessionUsage()
	args := map[string]any{"session": s.id, "provider": st.Provider, "model": st.Model, "effort": st.Effort,
		"context": s.ContextTokens(), "input": usage.Input, "output": usage.Output, "cached": usage.Cached, "n": requests}
	go s.parent.call("child.usage", args)
}

// childUsage records what a running child reported; a late report of one
// that ended is ignored.
func (s *session) childUsage(id string, update func(*SubagentRun)) {
	s.childMu.Lock()
	run, ok := s.children[id]
	if ok {
		update(&run)
		s.children[id] = run
	}
	s.childMu.Unlock()
}

// leaveParent tells the parent that this child ended, with its final spend.
func (s *session) leaveParent() {
	if s.parent != nil && s.id != "" {
		st := s.Settings()
		usage, requests := s.SessionUsage()
		s.parent.call("child.end", map[string]any{"session": s.id, "provider": st.Provider, "model": st.Model, "effort": st.Effort,
			"context": s.ContextTokens(), "input": usage.Input, "output": usage.Output, "cached": usage.Cached, "n": requests})
	}
}

func (s *session) childStarted(run SubagentRun, pid int) {
	s.childMu.Lock()
	if s.children == nil {
		s.children, s.childPIDs = map[string]SubagentRun{}, map[string]int{}
	}
	if prev, ok := s.children[run.ID]; ok {
		if !prev.Started.IsZero() {
			run.Started = prev.Started
		}
		if run.Command == "" {
			run.Command = prev.Command
		}
		run.Context, run.Usage, run.Requests = prev.Context, prev.Usage, prev.Requests
	}
	s.children[run.ID], s.childPIDs[run.ID] = run, pid
	s.childMu.Unlock()
	s.idleSent.Store(false)
	s.changes.notify()
}

func (s *session) childEnded(id string) {
	s.childMu.Lock()
	if run, ok := s.children[id]; ok {
		if run.Ended.IsZero() {
			run.Ended = time.Now()
		}
		s.children[id] = run
	}
	delete(s.childPIDs, id)
	s.childMu.Unlock()
	s.changes.notify()
	s.checkIdle()
}
