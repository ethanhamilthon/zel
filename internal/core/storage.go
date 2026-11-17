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
	"strings"
	"time"
	"zel/internal/store"
)

// openProject binds the session to the project of dir and creates a new session
// there, or opens the stored session resume.
func (s *session) openProject(dir, resume string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	s.dir, s.project = absolute, absolute
	if resume != "" {
		return s.openSession(resume)
	}
	return s.createSession()
}

func validSessionID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func (s *session) openSession(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	v, items, err := s.db.Load(s.project, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	// A child may only continue its own children.
	if s.parent != nil && v.Parent != s.parent.session {
		return fail(CodeNotFound, "ENOENT: %s is not a zel -p session of this session", id)
	}
	s.id, s.created = id, v.Created
	if v.BashCwd != "" {
		if info, err := os.Stat(v.BashCwd); err == nil && info.IsDir() {
			s.bashMu.Lock()
			s.bashDir = v.BashCwd
			s.bashMu.Unlock()
		} else {
			defer s.notify("Folder " + v.BashCwd + " (zel cd) is gone: bash starts in " + s.dir)
		}
	}
	s.histMu.Lock()
	s.title = v.Title
	s.stored = true
	s.histMu.Unlock()
	if err := s.loadConfig(); err != nil {
		return err
	}
	s.fixSelection(selection{v.Provider, v.Model, v.Effort})
	if err := s.acquire(); err != nil {
		return err
	}
	s.readItems(items)
	choices, _ := s.db.Choices(s.project, id)
	s.histMu.Lock()
	s.choices = choices
	s.histMu.Unlock()
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
	s.created = time.Now().UTC()
	s.id = s.created.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:])
	s.histMu.Lock()
	s.itemMeta = nil
	s.histMu.Unlock()
	if err := s.loadConfig(); err != nil {
		return err
	}
	s.fixSelection(selection{})
	return s.acquire()
}

func (s *session) readItems(items []store.Item) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.History, s.itemMeta, s.compacted = nil, nil, compaction{}
	s.cacheMu.Lock()
	s.projections, s.firstLine = nil, ""
	s.cacheMu.Unlock()
	for _, item := range items {
		s.History = append(s.History, item.Data)
		if prompt, ok := instructionSnapshot(item.Data); ok {
			s.prompt, s.promptFrozen = prompt, true
		}
		if c, ok := compactionSnapshot(item.Data); ok {
			s.compacted = c
		}
		var m Metadata
		if len(item.Meta) > 0 && json.Unmarshal(item.Meta, &m) == nil {
			if s.itemMeta == nil {
				s.itemMeta = map[int]*Metadata{}
			}
			s.itemMeta[len(s.History)-1] = &m
		}
	}
	s.saved = len(s.History)
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
	// A saved session pins the model it runs with, so a later default does not change it.
	s.cfg.mu.Lock()
	if s.cfg.sel.Model == "" && s.cfg.s.Model != "" {
		s.cfg.sel = selection{s.cfg.s.Provider, s.cfg.s.Model, s.cfg.s.Effort}
	}
	sel := s.cfg.sel
	s.cfg.mu.Unlock()
	s.histMu.RLock()
	saved, total := s.saved, len(s.History)
	metaVersion := s.metaVersion
	start := min(saved, total)
	items := make([]store.Item, total-start)
	for i := start; i < total; i++ {
		items[i-start].Data = withoutImages(s.History[i])
		if m := s.itemMeta[i]; m != nil {
			items[i-start].Meta, _ = json.Marshal(m)
		}
	}
	title := s.title
	first := s.firstUserLine()
	s.histMu.RUnlock()
	v := store.Session{ID: s.id, Project: s.project, Title: title, First: first, Provider: sel.Provider, Model: sel.Model, Effort: sel.Effort,
		Mode: s.mode(), Depth: s.depth, Created: s.created, BashCwd: s.bashCwdSet()}
	if s.parent != nil {
		v.Parent = s.parent.session
	}
	if err := s.db.Append(v, items, saved, total); err != nil {
		return err
	}
	s.histMu.Lock()
	if s.metaVersion == metaVersion {
		s.saved = total
	} else {
		s.saved = min(s.saved, start)
	}
	s.stored = true
	s.histMu.Unlock()
	return nil
}

func (a *Runtime) Sessions() ([]SessionSummary, error) { return a.session().Sessions() }

func (s *session) Sessions() ([]SessionSummary, error) {
	list, err := s.db.Sessions(s.project)
	out := make([]SessionSummary, 0, len(list))
	for _, v := range list {
		out = append(out, SessionSummary{ID: v.ID, Title: v.Title, Project: v.Project, Updated: v.Updated})
	}
	return out, err
}

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
	return s.firstUserLine()
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

// imagePlaceholder replaces an image in the saved history: images live in
// memory only, so a resumed session shows this text instead.
const imagePlaceholder = "[an image was here]"

// withoutImages is the item as it is saved: input images become a text part.
func withoutImages(item json.RawMessage) json.RawMessage {
	if !strings.Contains(string(item), `"input_image"`) {
		return item
	}
	var v map[string]any
	if json.Unmarshal(item, &v) != nil {
		return item
	}
	parts, _ := v["content"].([]any)
	for i, part := range parts {
		if p, ok := part.(map[string]any); ok && p["type"] == "input_image" {
			parts[i] = map[string]any{"type": "input_text", "text": imagePlaceholder}
		}
	}
	return raw(v)
}
