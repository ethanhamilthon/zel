package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionFile(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := "/tmp/proj"
	items := []Item{{Data: json.RawMessage(`{"role":"user","content":"hi"}`)}, {Data: json.RawMessage(`{"a":1}`), Meta: json.RawMessage(`{"Steps":2}`)}}
	v := Session{ID: "s1", Project: project, First: "hi", Provider: "p", Model: "m", Effort: "low", Mode: "main"}
	if err := s.Save(v, items, 0); err != nil {
		t.Fatal(err)
	}
	v.Title, v.Model = "named", "m2"
	items = append(items[:1], Item{Data: json.RawMessage(`{"b":2}`)})
	if err := s.Save(v, items, 2); err != nil { // the last item was replaced: truncate + new
		t.Fatal(err)
	}
	items = items[:1]
	if err := s.Save(v, items, 2); err != nil {
		t.Fatal(err)
	}
	// A torn line at the end is ignored.
	f, _ := os.OpenFile(filepath.Join(s.Dir(project, "s1"), "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"t":"item","seq":1,"data":{"x"`)
	f.Close()

	fresh, _ := Open(filepath.Dir(s.Root))
	got, loaded, err := fresh.Load(project, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "named" || got.Model != "m2" || got.Mode != "main" || len(loaded) != 1 || string(loaded[0].Data) != `{"role":"user","content":"hi"}` {
		t.Fatalf("loaded %+v %d items", got, len(loaded))
	}
	if _, _, err := fresh.Load(project, "nope"); err != ErrNotFound {
		t.Fatalf("missing session: %v", err)
	}
	child := Session{ID: "s2", Project: project, Parent: "s1"}
	if err := s.Save(child, nil, 0); err != nil {
		t.Fatal(err)
	}
	list, err := s.Sessions(project)
	if err != nil || len(list) != 1 || list[0].ID != "s1" || list[0].Title != "named" {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
}

func TestUnchangedSavePreservesMetadataAndSuffixAppend(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := Session{ID: "s", Project: "/project", First: "hello"}
	items := []Item{{Data: json.RawMessage(`{"role":"user","content":"hello"}`)}}
	if err := s.Save(v, items, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(v.Project, v.ID), "meta.json")
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(v, nil, 1, 1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatal("no-change save rewrote meta", err)
	}
	next := Item{Data: json.RawMessage(`{"role":"assistant","content":"answer"}`)}
	if err := s.Append(v, []Item{next}, 1, 2); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := s.Load(v.Project, v.ID)
	if err != nil || len(loaded) != 2 || string(loaded[1].Data) != string(next.Data) {
		t.Fatalf("suffix save: %v %+v", err, loaded)
	}
}

func TestMetadataFailureRemainsRetryable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := Session{ID: "retry", Project: "/project", Title: "old"}
	if err := s.Save(v, nil, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(v.Project, v.ID), "meta.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	v.Title = "new"
	if err := s.Save(v, nil, 0); err == nil {
		t.Fatal("metadata failure ignored")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(v, nil, 0); err != nil {
		t.Fatal(err)
	}
	m, err := readMeta(filepath.Dir(path))
	if err != nil || m.Title != "new" {
		t.Fatalf("retry: %+v %v", m, err)
	}
}

func TestMetadataFailureRollsBackDurableLogAndRemainsRetryable(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	v := Session{ID: "rollback", Project: "/project", Title: "old", Provider: "p1", Model: "m1", Effort: "medium"}
	items := []Item{{Data: json.RawMessage(`{"type":"instructions","text":"OLD"}`)}}
	if err := s.Save(v, items, 0); err != nil {
		t.Fatal(err)
	}
	dir := s.Dir(v.Project, v.ID)
	logPath := filepath.Join(dir, "session.jsonl")
	metaPath := filepath.Join(dir, "meta.json")
	origLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metaPath, 0700); err != nil {
		t.Fatal(err)
	}
	v2 := Session{ID: "rollback", Project: "/project", Title: "new", Provider: "p2", Model: "m2", Effort: "high"}
	items2 := append(items, Item{Data: json.RawMessage(`{"type":"instructions","text":"NEW"}`)})
	if err := s.Save(v2, items2, 1); err == nil {
		t.Fatal("metadata replacement failure ignored")
	}
	gotLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotLog, origLog) {
		t.Fatalf("session.jsonl changed on failed metadata replacement:\ngot:  %s\nwant: %s", gotLog, origLog)
	}

	// Removing the metadata fault allows clean resume of previous state and retry.
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, loadedItems, err := fresh.Load(v.Project, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "old" || loaded.Provider != "p1" || loaded.Model != "m1" || loaded.Effort != "medium" ||
		len(loadedItems) != 1 || string(loadedItems[0].Data) != string(items[0].Data) {
		t.Fatalf("loaded after rollback = %+v %+v", loaded, loadedItems)
	}

	if err := s.Save(v2, items2, 1); err != nil {
		t.Fatalf("retry after fault removal: %v", err)
	}
	fresh2, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	retried, retriedItems, err := fresh2.Load(v.Project, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Title != "new" || retried.Provider != "p2" || retried.Model != "m2" || retried.Effort != "high" ||
		len(retriedItems) != 2 || string(retriedItems[1].Data) != string(items2[1].Data) {
		t.Fatalf("loaded after retry = %+v %+v", retried, retriedItems)
	}
	if bytes.Count(func() []byte { b, _ := os.ReadFile(logPath); return b }(), []byte(`"m2"`)) != 1 {
		t.Fatal("duplicate model line after retry")
	}

	// Brand-new session metadata failure must not leave an orphaned session.jsonl.
	freshID := "brandnew"
	freshDir := s.Dir("/project", freshID)
	if err := os.MkdirAll(filepath.Join(freshDir, "meta.json"), 0700); err != nil {
		t.Fatal(err)
	}
	vNew := Session{ID: freshID, Project: "/project", Provider: "p1", Model: "m1"}
	if err := s.Save(vNew, items, 0); err == nil {
		t.Fatal("brand-new session metadata failure ignored")
	}
	if s.Exists("/project", freshID) {
		t.Fatal("failed brand-new save left session.jsonl")
	}
	if err := os.Remove(filepath.Join(freshDir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(vNew, items, 0); err != nil {
		t.Fatalf("brand-new retry: %v", err)
	}
}
