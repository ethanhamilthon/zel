package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"zel/internal/store"
)

func (s *session) openProject(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	s.dir, s.project = absolute, absolute
	return s.createSession()
}

func validSessionID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func (a *Runtime) openSession(id string) error { return a.session().openSession(id) }

func (s *session) openSession(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	v, err := s.db.Session(id, s.project)
	sel := selection{v.Provider, v.Model, v.Effort}
	s.histMu.Lock()
	s.title = v.Title
	s.histMu.Unlock()
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	s.id = id
	s.histMu.Lock()
	s.stored = true
	s.histMu.Unlock()
	s.fixSelection(sel)
	if err := s.acquire(); err != nil {
		return err
	}
	if err := s.readSession(); err != nil {
		s.release()
		return err
	}
	if _, err := s.instructions(context.Background()); err != nil {
		s.release()
		return err
	}
	return nil
}

func (s *session) createSession() error {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	s.id = time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:])
	s.histMu.Lock()
	s.itemMeta = nil
	s.histMu.Unlock()
	s.fixSelection(selection{})
	return s.acquire()
}

func (a *Runtime) readSession() error { return a.session().readSession() }

func (s *session) readSession() error {
	items, err := s.db.Items(s.id)
	if err != nil {
		return err
	}
	s.histMu.Lock()
	s.History, s.itemMeta, s.imageWarnings, s.compacted = nil, nil, nil, compaction{}
	for _, item := range items {
		s.History = append(s.History, item.Data)
		s.imageWarnings = append(s.imageWarnings, missingOriginalImages(item.Data)...)
		if prompt, ok := instructionSnapshot(item.Data); ok {
			s.prompt, s.promptFrozen = prompt, true
		}
		if c, ok := compactionSnapshot(item.Data); ok {
			s.compacted = c
		}
		if item.Meta != nil {
			if s.itemMeta == nil {
				s.itemMeta = map[int]*Metadata{}
			}
			m := *item.Meta
			s.itemMeta[len(s.History)-1] = &m
		}
	}
	s.saved = len(s.History)
	s.histMu.Unlock()
	return nil
}

func (s *session) HasSavedSession() bool {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	return len(s.History) > 0 || s.stored
}

func (a *Runtime) Save() error { return a.session().Save() }

func (s *session) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	// Read before the transaction: update holds cfg.mu while it writes the database.
	// A saved session pins the model it runs with, so a later default does not change it.
	s.cfg.mu.Lock()
	if s.cfg.sel.Model == "" && s.cfg.s.Model != "" {
		s.cfg.sel = selection{s.cfg.s.Provider, s.cfg.s.Model, s.cfg.s.Effort}
	}
	sel := s.cfg.sel
	s.cfg.mu.Unlock()
	s.histMu.RLock()
	history := slices.Clone(s.History)
	meta := maps.Clone(s.itemMeta)
	saved := s.saved
	title := s.title
	first := firstUserLine(history)
	s.histMu.RUnlock()
	if err := s.db.SaveSession(store.Session{ID: s.id, Project: s.project, Title: title, First: first, Provider: sel.Provider, Model: sel.Model, Effort: sel.Effort}, history, meta, saved); err != nil {
		return err
	}
	s.histMu.Lock()
	if len(history) > s.saved {
		s.saved = len(history)
	}
	s.stored = true
	s.histMu.Unlock()
	return nil
}

func (a *Runtime) Sessions() ([]SessionSummary, error) { return a.session().Sessions() }

func (s *session) Sessions() ([]SessionSummary, error) { return s.db.Sessions(s.project) }

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

func (a *Runtime) CustomTitle() string { return a.session().CustomTitle() }

func (s *session) CustomTitle() string {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	return s.title
}

func (a *Runtime) Rename(name string) error { return a.session().Rename(name) }

func (s *session) Rename(name string) error {
	if s.ended.Load() {
		return errors.New("session has ended")
	}
	clean := strings.Join(strings.Fields(name), " ")
	s.histMu.Lock()
	s.title = clean
	s.histMu.Unlock()
	if !s.HasSavedSession() {
		return nil
	}
	return s.Save()
}

func FirstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func (a *Runtime) DefaultTitle() string { return a.session().DefaultTitle() }

func (s *session) DefaultTitle() string {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	return firstUserLine(s.History)
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
