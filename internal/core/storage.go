package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"zel/internal/store"
)

func (a *Runtime) openProject(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	a.Shell.Dir, a.project = absolute, absolute
	return a.createSession()
}

func validSessionID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func (a *Runtime) openSession(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	v, err := a.db.Session(id, a.project)
	sel := selection{v.Provider, v.Model, v.Effort}
	a.title = v.Title
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	a.id, a.stored = id, true
	a.fixSelection(sel)
	if err := a.acquire(); err != nil {
		return err
	}
	if err := a.readSession(); err != nil {
		a.release()
		return err
	}
	if _, err := a.instructions(context.Background()); err != nil {
		a.release()
		return err
	}
	return nil
}

func (a *Runtime) createSession() error {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	a.id = time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:])
	a.itemMeta = nil
	a.fixSelection(selection{})
	return a.acquire()
}

func (a *Runtime) readSession() error {
	items, err := a.db.Items(a.id)
	if err != nil {
		return err
	}
	a.History, a.itemMeta, a.imageWarnings, a.compacted = nil, nil, nil, compaction{}
	for _, item := range items {
		a.History = append(a.History, item.Data)
		a.imageWarnings = append(a.imageWarnings, missingOriginalImages(item.Data)...)
		if prompt, ok := instructionSnapshot(item.Data); ok {
			a.prompt, a.promptFrozen = prompt, true
		}
		if c, ok := compactionSnapshot(item.Data); ok {
			a.compacted = c
		}
		if item.Meta != nil {
			a.setItemMeta(len(a.History)-1, *item.Meta)
		}
	}
	a.saved = len(a.History)
	return nil
}

func (a *Runtime) Save() error {
	// Read before the transaction: update holds cfg.mu while it writes the database.
	// A saved session pins the model it runs with, so a later default does not change it.
	a.cfg.mu.Lock()
	if a.cfg.sel.Model == "" && a.cfg.s.Model != "" {
		a.cfg.sel = selection{a.cfg.s.Provider, a.cfg.s.Model, a.cfg.s.Effort}
	}
	sel := a.cfg.sel
	a.cfg.mu.Unlock()
	if err := a.db.SaveSession(store.Session{ID: a.id, Project: a.project, Title: a.title, First: a.DefaultTitle(), Provider: sel.Provider, Model: sel.Model, Effort: sel.Effort}, a.History, a.itemMeta, a.saved); err != nil {
		return err
	}
	a.saved, a.stored = len(a.History), true
	return nil
}

func (a *Runtime) Sessions() ([]SessionSummary, error) { return a.db.Sessions(a.project) }

func firstUserLine(history []json.RawMessage) string {
	for _, item := range history {
		for _, r := range RecordsForItem(item) {
			if line := FirstLine(r.Text); r.Kind == "user" && line != "" {
				return line
			}
		}
	}
	return ""
}

func (a *Runtime) CustomTitle() string { return a.title }

func (a *Runtime) Rename(name string) error {
	a.title = strings.Join(strings.Fields(name), " ")
	if !a.HasSavedSession() {
		return nil
	}
	return a.Save()
}

func FirstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func (a *Runtime) DefaultTitle() string {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	return firstUserLine(a.History)
}

func interruptedOutputs(history []json.RawMessage) []json.RawMessage {
	var calls []string
	outputs := map[string]bool{}
	for _, item := range history {
		v, err := parseCall(item)
		if err != nil || v.CallID == "" {
			continue
		}
		if v.Type == "function_call" {
			calls = append(calls, v.CallID)
		}
		if v.Type == "function_call_output" {
			outputs[v.CallID] = true
		}
	}
	var result []json.RawMessage
	for _, id := range calls {
		if outputs[id] {
			continue
		}
		outputs[id] = true
		result = append(result, callOutput(id, "error: tool execution was interrupted; its effects and result are unknown. It was not rerun."))
	}
	return result
}

// ReadSession reads a persisted snapshot without acquiring session ownership. It works while another process is running the session.
func ReadSession(cwd, id string) ([]Record, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("invalid session ID: %s", id)
	}
	home, err := ZelHome(cwd)
	if err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(home, "zel.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	_, err = db.SessionProject(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("session not found: %s", id)
		}
		return nil, err
	}
	a := &Runtime{db: db, id: id, histMu: &sync.RWMutex{}, acts: &activities{}}
	if err := a.readSession(); err != nil {
		return nil, err
	}
	return a.Records(), nil
}

func missingOriginalImages(item json.RawMessage) []Record {
	var v struct{ Content []struct{ Text string } }
	if json.Unmarshal(item, &v) != nil {
		return nil
	}
	var warnings []Record
	for _, part := range v.Content {
		for _, source := range strings.Split(part.Text, originalImage)[1:] {
			quoted, _, _ := strings.Cut(source, "\n")
			path, err := strconv.Unquote(quoted)
			if err != nil {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				warnings = append(warnings, Record{Kind: "notice", Text: "Original image unavailable for crops: " + path + " (" + err.Error() + "). The saved preview is still available."})
			}
		}
	}
	return warnings
}
