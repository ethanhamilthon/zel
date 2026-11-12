package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SubagentRun describes an active child agent run (zel -p).
// Parent holds the root session ID of the whole tree.
type SubagentRun struct {
	ID      string    `json:"id"`
	Parent  string    `json:"parent"`
	Started time.Time `json:"started"`
	Steps   int       `json:"steps"`
	Model   string    `json:"model,omitempty"`
	Effort  string    `json:"effort,omitempty"`
	Command string    `json:"command"`
}

// SubagentHandle is returned by StartSubagent to allow updating progress and finishing.
type SubagentHandle struct {
	run  *SubagentRun
	home string
	mu   sync.Mutex
}

var (
	subagentMu  sync.Mutex
	subagentMap = map[string]*SubagentRun{}
)

// StartSubagent registers an active child agent run under the root session parent.
// If parent is empty, it returns a no-op handle.
func StartSubagent(home, id, parent, command string) *SubagentHandle {
	if parent == "" {
		return &SubagentHandle{}
	}
	run := &SubagentRun{ID: id, Parent: parent, Started: time.Now(), Command: command}
	subagentMu.Lock()
	subagentMap[id] = run
	subagentMu.Unlock()
	h := &SubagentHandle{run: run, home: home}
	h.persist()
	return h
}

// TrackEvent counts committed tool calls and reasoning blocks, not streaming previews.
func (h *SubagentHandle) TrackEvent(e TurnEvent) {
	if e.Final && !e.Provisional && (e.Kind == "code" || e.Kind == "reasoning") {
		h.AddStep()
	}
}

// SetModel records the child's effective model and reasoning effort.
func (h *SubagentHandle) SetModel(model, effort string) {
	h.update(func() { h.run.Model, h.run.Effort = model, effort })
}

// SetSteps updates the step count of the running subagent.
func (h *SubagentHandle) SetSteps(n int) { h.update(func() { h.run.Steps = n }) }

// AddStep increments the step count of the running subagent.
func (h *SubagentHandle) AddStep() { h.update(func() { h.run.Steps++ }) }

func (h *SubagentHandle) update(fn func()) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run != nil {
		subagentMu.Lock()
		fn()
		subagentMu.Unlock()
		h.persist()
	}
}

// Finish unregisters the running subagent and removes its persisted state.
func (h *SubagentHandle) Finish() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run == nil {
		return
	}
	subagentMu.Lock()
	delete(subagentMap, h.run.ID)
	subagentMu.Unlock()
	if h.home != "" {
		_ = os.Remove(filepath.Join(h.home, "subagents", h.run.ID+".json"))
	}
	h.run = nil
}

func (h *SubagentHandle) persist() {
	if h.home == "" || h.run == nil {
		return
	}
	dir := filepath.Join(h.home, "subagents")
	_ = os.MkdirAll(dir, 0700)
	data, _ := json.Marshal(h.run)
	_ = os.WriteFile(filepath.Join(dir, h.run.ID+".json"), data, 0600)
}

// ActiveSubagentsForParent lists all running subagents registered for a root session ID.
func ActiveSubagentsForParent(home, parentID string) (list []SubagentRun) {
	seen := map[string]bool{}
	subagentMu.Lock()
	for _, run := range subagentMap {
		if run.Parent == parentID {
			list = append(list, *run)
			seen[run.ID] = true
		}
	}
	subagentMu.Unlock()
	if home == "" {
		return list
	}
	dir := filepath.Join(home, "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return list
	}
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(name, ".json")
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || seen[id] {
			continue
		}
		info, err := entry.Info()
		file := filepath.Join(dir, name)
		if err != nil || isDeadLock(filepath.Join(home, "locks", id+".lock"), info.ModTime()) {
			if err == nil && time.Since(info.ModTime()) > 5*time.Second {
				_ = os.Remove(file)
			}
			continue
		}
		var run SubagentRun
		if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &run) == nil && run.Parent == parentID {
			list = append(list, run)
			seen[run.ID] = true
		}
	}
	return list
}

func isDeadLock(path string, mtime time.Time) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return os.IsNotExist(err) && time.Since(mtime) > 5*time.Second
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	return false
}
