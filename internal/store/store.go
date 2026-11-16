// Package store keeps sessions as files: one folder per session under
// ~/.zel/sessions/<project key>/<id>/ with an append-only session.jsonl, a
// meta.json cache for the session list, tasks.jsonl and the task logs.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("session not found")

// Store is the sessions folder of one zel home. It is safe for concurrent use;
// one session is written by one process at a time (the caller holds its lock).
type Store struct {
	Root string // ~/.zel/sessions

	mu   sync.Mutex
	last map[string]Session // what the session file says now, by session dir
}

func Open(home string) (*Store, error) {
	root := filepath.Join(home, "sessions")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Store{Root: root, last: map[string]Session{}}, nil
}

func (s *Store) Close() error { return nil }

// ProjectKey names the folder of a project: its absolute path with "/" as "-".
func ProjectKey(project string) string {
	return strings.ReplaceAll(filepath.Clean(project), string(filepath.Separator), "-")
}

// Dir is the folder of one session.
func (s *Store) Dir(project, id string) string {
	return filepath.Join(s.Root, ProjectKey(project), id)
}

// Session is the header and the latest title and model of a session.
type Session struct {
	ID, Project, Title, First string
	Provider, Model, Effort   string
	Parent, Mode              string
	Depth                     int
	Config                    []string
	BashCwd                   string // zel cd; empty = the session folder
	Created, Updated          time.Time
}

// Summary is one row of the session list.
type Summary struct {
	ID, Title, Project, Parent string
	Updated                    time.Time
}

// line is one line of session.jsonl.
type line struct {
	T        string          `json:"t"`
	V        int             `json:"v,omitempty"`
	ID       string          `json:"id,omitempty"`
	Cwd      string          `json:"cwd,omitempty"`
	Parent   string          `json:"parent,omitempty"`
	Depth    int             `json:"depth,omitempty"`
	Mode     string          `json:"mode,omitempty"`
	Created  *time.Time      `json:"created,omitempty"`
	Config   []string        `json:"config,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	Effort   string          `json:"effort,omitempty"`
	Title    *string         `json:"title,omitempty"`
	Dir      *string         `json:"dir,omitempty"` // cd lines: the bash folder
	Seq      *int            `json:"seq,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

type meta struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	First    string    `json:"first"`
	Parent   string    `json:"parent,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Effort   string    `json:"effort,omitempty"`
	Updated  time.Time `json:"updated"`
}

// Item is one history item with its metadata, as JSON.
type Item struct {
	Data json.RawMessage
	Meta json.RawMessage
}

// Load reads a session: its header, latest title and model, and its items.
// A torn last line (a crash in the middle of a write) is ignored.
func (s *Store) Load(project, id string) (Session, []Item, error) {
	dir := s.Dir(project, id)
	file, err := os.Open(filepath.Join(dir, "session.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, nil, ErrNotFound
	}
	if err != nil {
		return Session{}, nil, err
	}
	defer file.Close()
	var v Session
	var items []Item
	r := bufio.NewReaderSize(file, 1<<20)
	for {
		raw, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 && (err == nil || json.Valid(raw)) {
			var l line
			if json.Unmarshal(raw, &l) == nil {
				switch l.T {
				case "session":
					v = Session{ID: l.ID, Project: l.Cwd, Parent: l.Parent, Depth: l.Depth, Mode: l.Mode, Config: l.Config,
						Provider: l.Provider, Model: l.Model, Effort: l.Effort}
					if l.Created != nil {
						v.Created = *l.Created
					}
				case "title":
					if l.Title != nil {
						v.Title = *l.Title
					}
				case "cd":
					if l.Dir != nil {
						v.BashCwd = *l.Dir
					}
				case "model":
					v.Provider, v.Model, v.Effort = l.Provider, l.Model, l.Effort
				case "item":
					if l.Seq != nil && *l.Seq <= len(items) {
						items = append(items[:*l.Seq], Item{Data: l.Data, Meta: l.Meta})
					}
				case "truncate":
					if l.Seq != nil && *l.Seq < len(items) {
						items = items[:*l.Seq]
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if v.ID == "" {
		return Session{}, nil, ErrNotFound
	}
	if m, err := readMeta(dir); err == nil {
		v.First, v.Updated = m.First, m.Updated
	}
	s.mu.Lock()
	v.Project = project
	s.last[dir] = v
	s.mu.Unlock()
	return v, items, nil
}

// Exists reports whether the session has a file in the project.
func (s *Store) Exists(project, id string) bool {
	_, err := os.Stat(filepath.Join(s.Dir(project, id), "session.jsonl"))
	return err == nil
}

// Save appends what changed since the last save: the header of a new session,
// the title and model when they changed, and the items from saved on. When the
// history is shorter than what was saved, a truncate line drops the rest.
func (s *Store) Save(v Session, items []Item, saved int) error {
	return s.Append(v, items[min(saved, len(items)):], saved, len(items))
}

// Append accepts only the changed suffix, avoiding full-history preparation.
func (s *Store) Append(v Session, items []Item, saved, total int) error {
	dir := s.Dir(v.Project, v.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, known := s.last[dir]
	if !known {
		if old, err := readMeta(dir); err == nil {
			prev, known = Session{ID: v.ID, Title: old.Title, Provider: old.Provider, Model: old.Model, Effort: old.Effort}, true
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if !known {
		created := v.Created
		if created.IsZero() {
			created = time.Now().UTC()
		}
		_ = enc.Encode(line{T: "session", V: 1, ID: v.ID, Cwd: v.Project, Parent: v.Parent, Depth: v.Depth, Mode: v.Mode,
			Created: &created, Config: v.Config, Provider: v.Provider, Model: v.Model, Effort: v.Effort})
		prev = Session{Provider: v.Provider, Model: v.Model, Effort: v.Effort}
	}
	if v.Title != prev.Title {
		title := v.Title
		_ = enc.Encode(line{T: "title", Title: &title})
	}
	if v.BashCwd != prev.BashCwd {
		dir := v.BashCwd
		_ = enc.Encode(line{T: "cd", Dir: &dir})
	}
	if v.Model != "" && (v.Provider != prev.Provider || v.Model != prev.Model || v.Effort != prev.Effort) {
		_ = enc.Encode(line{T: "model", Provider: v.Provider, Model: v.Model, Effort: v.Effort})
	}
	if total < saved {
		n := total
		_ = enc.Encode(line{T: "truncate", Seq: &n})
	}
	for i := range items {
		seq := min(saved, total) + i
		_ = enc.Encode(line{T: "item", Seq: &seq, Data: items[i].Data, Meta: items[i].Meta})
	}
	if buf.Len() > 0 {
		if err := appendFile(filepath.Join(dir, "session.jsonl"), buf.Bytes(), true); err != nil {
			return err
		}
	}
	if buf.Len() == 0 && known && v.First == prev.First && v.Parent == prev.Parent {
		if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
			return nil
		}
	}
	keep := v
	keep.Created = time.Time{}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta{ID: v.ID, Title: v.Title, First: v.First, Parent: v.Parent,
		Provider: v.Provider, Model: v.Model, Effort: v.Effort, Updated: time.Now().UTC()}); err != nil {
		return err
	}
	s.last[dir] = keep
	return nil
}

// Sessions lists the sessions of a project, newest first. Sessions started by
// another session (zel -p children) are left out.
func (s *Store) Sessions(project string) ([]Summary, error) {
	base := filepath.Join(s.Root, ProjectKey(project))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Summary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readMeta(filepath.Join(base, e.Name()))
		if err != nil || m.Parent != "" {
			continue
		}
		title := m.Title
		if title == "" {
			title = m.First
		}
		list = append(list, Summary{ID: e.Name(), Title: title, Project: project, Updated: m.Updated})
	}
	sortSummaries(list)
	return list, nil
}

func sortSummaries(list []Summary) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && (list[j].Updated.After(list[j-1].Updated) ||
			(list[j].Updated.Equal(list[j-1].Updated) && list[j].ID > list[j-1].ID)); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func readMeta(dir string) (meta, error) {
	var m meta
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	return m, err
}

// appendFile appends data to path, created private; sync flushes it to disk.
func appendFile(path string, data []byte, sync bool) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil && sync {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// writeJSON replaces path with v atomically.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
