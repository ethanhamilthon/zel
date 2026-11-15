package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"zel/internal/luart"
)

// MaxAgentDepth is how deep agents nest: subagents cannot start subagents.
const MaxAgentDepth = 1

// SubagentRun describes a running child agent. Parent is the root session of
// the whole agent tree. Activity is the foreground host operation the agent is
// waiting on, "" when none runs.
type SubagentRun struct {
	ID       string
	Parent   string
	Started  time.Time
	Steps    int
	Model    string
	Effort   string
	Command  string
	Activity string
}

// agentTree is the in-memory registry of running agents. A root session and
// every agent below it share one tree; nothing is written to disk.
type agentTree struct {
	mu      sync.Mutex
	runs    map[string]*SubagentRun
	changed func() // optional: called, outside the lock, after the set or a step count changed
}

func newAgentTree() *agentTree { return &agentTree{runs: map[string]*SubagentRun{}} }

func (t *agentTree) notify() {
	t.mu.Lock()
	fn := t.changed
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (t *agentTree) add(run SubagentRun) {
	t.mu.Lock()
	t.runs[run.ID] = &run
	t.mu.Unlock()
	t.notify()
}

func (t *agentTree) remove(id string) {
	t.mu.Lock()
	delete(t.runs, id)
	t.mu.Unlock()
	t.notify()
}

// setActivity updates the activity of one running agent. Only a change notifies.
func (t *agentTree) setActivity(id, label string) {
	t.mu.Lock()
	run := t.runs[id]
	changed := run != nil && run.Activity != label
	if changed {
		run.Activity = label
	}
	t.mu.Unlock()
	if changed {
		t.notify()
	}
}

func (t *agentTree) step(id string) {
	t.mu.Lock()
	if run := t.runs[id]; run != nil {
		run.Steps++
	}
	t.mu.Unlock()
	t.notify()
}

// active lists the running agents of one root session, oldest first.
func (t *agentTree) active(root string) []SubagentRun {
	t.mu.Lock()
	var list []SubagentRun
	for _, run := range t.runs {
		if run.Parent == root {
			list = append(list, *run)
		}
	}
	t.mu.Unlock()
	slices.SortFunc(list, func(a, b SubagentRun) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return list
}

// agentLink ties a child runtime to the tree of the session that started it;
// id is the key of the child's row in the tree.
type agentLink struct {
	tree  *agentTree
	root  string
	depth int
	id    string
	// budget is the shutdown budget of the parent session; the child's session
	// shares it (see lifeParent).
	budget context.Context
}

// ActiveSubagents lists the running agents of this session tree. It only reads
// memory, so it is safe to call from rendering paths.
func (a *Runtime) ActiveSubagents() []SubagentRun {
	s := a.session()
	return s.tree.active(s.rootID())
}

// OnSubagentsChanged registers fn to run whenever the set of running agents or
// background tasks, or a step count, changes. fn must not block.
func (a *Runtime) OnSubagentsChanged(fn func()) {
	a.tree.mu.Lock()
	a.tree.changed = fn
	a.tree.mu.Unlock()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// agentRec is a subagent this session started and can send more messages to.
// idle is closed once no child runtime holds the subagent's session: the child
// that ran its last message has closed. Messages to one subagent run one by one.
type agentRec struct {
	dir  string
	idle chan struct{}
}

// spawnAgent starts a child agent inside this process, or, with o.Resume, sends
// a new message to the subagent whose session that is. The child is a full
// runtime with its own session; this session owns its context, progress,
// cancellation and result.
func (s *session) spawnAgent(o luart.AgentOpts) (*job, error) {
	if s.tasks == nil {
		return nil, errors.New("no active session")
	}
	if s.depth+1 > MaxAgentDepth {
		return nil, fmt.Errorf("agents are nested too deeply (max depth %d)", MaxAgentDepth)
	}
	cwd := cmp.Or(o.Cwd, s.dir)
	next := make(chan struct{})
	var prev chan struct{}
	if o.Resume != "" {
		s.agentsMu.Lock()
		rec := s.agents[o.Resume]
		if rec != nil {
			cwd, prev, rec.idle = rec.dir, rec.idle, next
		}
		s.agentsMu.Unlock()
		if rec == nil {
			return nil, fmt.Errorf("%w: no subagent with session %s in this session", luart.ErrNoJob, o.Resume)
		}
	}
	model := cmp.Or(o.Model, s.Settings().Model)
	ctx, cancel := context.WithCancel(context.Background())
	var failed bool
	budget := s.tasks.life
	j, err := s.tasks.spawnProc("agent: "+oneLine(o.Prompt), startOpts{kind: "agent", prompt: o.Prompt, model: model, waited: o.Waited, timeout: o.Timeout},
		func(log io.Writer, j *job) (*proc, error) {
			// The child's session closes after the result is published, so its own
			// session_closed cannot delay the answer the caller waits for.
			j.afterEnd = j.runDeferredClose
			done := make(chan error, 1)
			go func() {
				err := s.runChild(ctx, j, log, cwd, o, budget, prev, next)
				failed = err != nil
				done <- err
			}()
			return &proc{cancel: cancel, term: cancel, wait: func() error { return <-done },
				exit: func() int {
					if failed {
						return 1
					}
					return 0
				}}, nil
		})
	if err != nil {
		cancel()
		return nil, err
	}
	return j, nil
}

// runChild runs one agent to completion and records its outcome on the job.
func (s *session) runChild(ctx context.Context, j *job, log io.Writer, cwd string, o luart.AgentOpts, budget context.Context, prev, next chan struct{}) error {
	// next is closed when this run no longer holds the subagent's session.
	release := sync.OnceFunc(func() { close(next) })
	holds := false // the child's close, set below, releases it later
	defer func() {
		if !holds {
			release()
		}
	}()
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			j.setOutcome(agentOutcome{Session: o.Resume})
			return ctx.Err()
		}
	}
	key := s.id + "/" + j.id
	root := s.rootID()
	tree := s.tree
	run := SubagentRun{ID: key, Parent: root, Started: j.started, Model: j.model, Effort: o.Effort, Command: oneLine(o.Prompt)}
	tree.add(run)
	defer tree.remove(key)

	var outcome agentOutcome
	opts := Options{Mode: "headless", Provider: o.Provider, Model: o.Model, Effort: o.Effort, Resume: o.Resume,
		link: &agentLink{tree: tree, root: root, depth: s.depth + 1, id: key, budget: budget}}
	opts.SessionReady = func(c *Runtime) error {
		if hint := c.Settings().SetupHint(); hint != "" {
			return errors.New(hint)
		}
		if err := c.Save(); err != nil {
			return err
		}
		outcome.Session = c.SessionID()
		if o.Resume == "" {
			s.agentsMu.Lock()
			if s.agents == nil {
				s.agents = map[string]*agentRec{}
			}
			s.agents[outcome.Session] = &agentRec{dir: cwd, idle: next}
			s.agentsMu.Unlock()
		}
		return nil
	}
	child, err := New(cwd, opts)
	if err != nil {
		j.setOutcome(agentOutcome{Error: err.Error()})
		return err
	}
	// The child's session closes after its result is published (see spawnAgent).
	j.deferClose(func() {
		child.Close()
		release()
	})
	holds = true
	if ctx.Err() != nil {
		j.setOutcome(agentOutcome{Session: outcome.Session})
		return ctx.Err()
	}
	st := child.Settings()
	tree.mu.Lock()
	if r := tree.runs[key]; r != nil {
		r.Model, r.Effort = st.Model, st.Effort
	}
	tree.mu.Unlock()
	tree.notify()

	steps := 0
	err = child.Run(ctx, o.Prompt, func(e TurnEvent) {
		if e.Final && !e.Provisional && (e.Kind == "code" || e.Kind == "reasoning") {
			steps++
			tree.step(key)
			label := "lua"
			if e.Kind == "reasoning" {
				label = "thinking"
			}
			fmt.Fprintf(log, "step %d: %s\n", steps, label)
		}
	})
	if err != nil {
		outcome.Error = err.Error()
		if ctx.Err() != nil {
			outcome.Error = "" // stopped or timed out: the job status says why
		}
		j.setOutcome(outcome)
		return err
	}
	outcome.OK, outcome.Answer = true, child.FinalText()
	fmt.Fprintf(log, "%s\n", outcome.Answer)
	j.setOutcome(outcome)
	return nil
}
