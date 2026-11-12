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

// SetSteps updates the step count of the running subagent.
func (h *SubagentHandle) SetSteps(n int) {
	if h != nil && h.run != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.run.Steps = n
		h.persist()
	}
}

// AddStep increments the step count of the running subagent.
func (h *SubagentHandle) AddStep() {
	if h != nil && h.run != nil {
		h.SetSteps(h.run.Steps + 1)
	}
}

// Finish unregisters the running subagent and removes its persisted state.
func (h *SubagentHandle) Finish() {
	if h == nil || h.run == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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
	if data, err := json.Marshal(h.run); err == nil {
		_ = os.WriteFile(filepath.Join(dir, h.run.ID+".json"), data, 0600)
	}
}

// ActiveSubagentsForParent lists all running subagents registered for a root session ID.
func ActiveSubagentsForParent(home, parentID string) []SubagentRun {
	var list []SubagentRun
	seen := map[string]bool{}
	subagentMu.Lock()
	for _, run := range subagentMap {
		if run.Parent == parentID {
			list = append(list, *run)
			seen[run.ID] = true
		}
	}
	subagentMu.Unlock()

	if home != "" {
		dir := filepath.Join(home, "subagents")
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				id := strings.TrimSuffix(entry.Name(), ".json")
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || seen[id] {
					continue
				}
				if isDeadLock(filepath.Join(home, "locks", id+".lock")) {
					_ = os.Remove(filepath.Join(dir, entry.Name()))
					continue
				}
				if data, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
					var run SubagentRun
					if json.Unmarshal(data, &run) == nil && run.Parent == parentID {
						list = append(list, run)
						seen[run.ID] = true
					}
				}
			}
		}
	}
	return list
}

func isDeadLock(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return os.IsNotExist(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	return false
}
