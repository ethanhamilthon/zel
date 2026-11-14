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
// the whole agent tree.
type SubagentRun struct {
	ID      string
	Parent  string
	Started time.Time
	Steps   int
	Model   string
	Effort  string
	Command string
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

// agentLink ties a child runtime to the tree of the session that started it.
type agentLink struct {
	tree  *agentTree
	root  string
	depth int
}

// ActiveSubagents lists the running agents of this session tree. It only reads
// memory, so it is safe to call from rendering paths.
func (a *Runtime) ActiveSubagents() []SubagentRun { return a.tree.active(a.rootID()) }

// OnSubagentsChanged registers fn to run whenever the set of running agents or
// their step counts change. fn must not block.
func (a *Runtime) OnSubagentsChanged(fn func()) {
	a.tree.mu.Lock()
	a.tree.changed = fn
	a.tree.mu.Unlock()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// spawnAgent starts a child agent inside this process. The child is a full
// runtime with its own session; this runtime owns its context, progress,
// cancellation and result.
func (a *Runtime) spawnAgent(o luart.AgentOpts) (*job, error) {
	if a.tasks == nil {
		return nil, errors.New("no active session")
	}
	if a.depth+1 > MaxAgentDepth {
		return nil, fmt.Errorf("agents are nested too deeply (max depth %d)", MaxAgentDepth)
	}
	cwd := cmp.Or(o.Cwd, a.Shell.Dir)
	model := cmp.Or(o.Model, a.Settings().Model)
	ctx, cancel := context.WithCancel(context.Background())
	var failed bool
	hooks, depth := a.lua, a.depth+1
	j, err := a.tasks.spawnProc("agent: "+oneLine(o.Prompt), startOpts{kind: "agent", prompt: o.Prompt, model: model, waited: o.Waited, timeout: o.Timeout},
		func(log io.Writer, j *job) (*proc, error) {
			// The lifecycle hooks run on goroutines of their own. They wait for the Lua
			// config lock, which a plugin function can hold while it waits for this very
			// subagent, so no hook may sit on the path to the subagent's result.
			started := make(chan struct{})
			j.endHook = func(status string, res map[string]any, closing bool) {
				<-started
				hooks.endSubagent(subagentEndInfo(j, o, status, res), closing)
			}
			done := make(chan error, 1)
			go func() {
				go func() {
					defer close(started)
					hooks.startSubagent(subagentStartInfo(j, o, cwd, depth))
				}()
				err := a.runChild(ctx, j, log, cwd, o)
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
func (a *Runtime) runChild(ctx context.Context, j *job, log io.Writer, cwd string, o luart.AgentOpts) error {
	key := a.id + "/" + j.id
	root := a.rootID()
	tree := a.tree
	run := SubagentRun{ID: key, Parent: root, Started: j.started, Model: j.model, Effort: o.Effort, Command: oneLine(o.Prompt)}
	tree.add(run)
	defer tree.remove(key)

	var outcome agentOutcome
	opts := Options{Mode: "headless", Provider: o.Provider, Model: o.Model, Effort: o.Effort,
		link: &agentLink{tree: tree, root: root, depth: a.depth + 1}}
	opts.SessionReady = func(c *Runtime) error {
		if hint := c.Settings().SetupHint(); hint != "" {
			return errors.New(hint)
		}
		if err := c.Save(); err != nil {
			return err
		}
		outcome.Session = c.SessionID()
		return nil
	}
	child, err := New(cwd, opts)
	if err != nil {
		j.setOutcome(agentOutcome{Error: err.Error()})
		return err
	}
	defer child.Close()
	if ctx.Err() != nil {
		j.setOutcome(agentOutcome{Session: outcome.Session})
		return ctx.Err()
	}
	s := child.Settings()
	tree.mu.Lock()
	if r := tree.runs[key]; r != nil {
		r.Model, r.Effort = s.Model, s.Effort
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

// subagentStartInfo is the payload of subagent_start: the identity of the
// subagent and what it was asked to do.
func subagentStartInfo(j *job, o luart.AgentOpts, cwd string, depth int) map[string]any {
	return map[string]any{"id": j.id, "prompt": o.Prompt, "model": j.model, "effort": o.Effort,
		"provider": o.Provider, "cwd": cwd, "depth": depth}
}

// subagentEndInfo is the payload of subagent_end: the final status and outcome.
// error is present only when the subagent did not succeed.
func subagentEndInfo(j *job, o luart.AgentOpts, status string, res map[string]any) map[string]any {
	info := map[string]any{"id": j.id, "status": status, "ok": res["ok"], "ms": res["ms"],
		"model": j.model, "effort": o.Effort, "answer": res["answer"], "session": res["session"]}
	if msg, ok := res["error"].(string); ok {
		info["error"] = msg
	}
	return info
}
