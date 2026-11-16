package store

import (
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

func TestTasks(t *testing.T) {
	s, _ := Open(t.TempDir())
	p := "/tmp/proj"
	s.CreateTask(p, "s", "t1", "build", "make", time.Now())
	s.CreateTask(p, "s", "t2", "", "sleep 9", time.Now())
	code := 2
	s.FinishTask(p, "s", "t1", "failed", &code)
	if err := s.LoseTasks(p, "s"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Tasks(p, "s")
	if err != nil || len(list) != 2 || list[0].Status != "failed" || *list[0].ExitCode != 2 || list[1].Status != "lost" || list[0].Name != "build" {
		t.Fatalf("tasks = %+v, %v", list, err)
	}
}
