package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"zel/internal/entities"
)

func TestSessionRoundTripAndExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zel.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)}
	metadata := map[int]*entities.Metadata{0: {Steps: 2}}
	session := Session{ID: "s1", Project: "/project", Model: "m", Provider: "p", Effort: "medium"}
	if err := db.SaveSession(session, history, metadata, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPreferences([]byte(`{"Model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := db.Items("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || string(items[0].Data) != string(history[0]) || items[0].Meta == nil || items[0].Meta.Steps != 2 {
		t.Fatalf("items=%+v", items)
	}
	loaded, err := db.Session("s1", "/project")
	if err != nil || loaded.Model != "m" {
		t.Fatalf("session=%+v err=%v", loaded, err)
	}
	if _, err := db.Session("s1", "/other"); err != ErrNotFound {
		t.Fatalf("project isolation: %v", err)
	}
	if data, err := db.Preferences(); err != nil || string(data) != `{"Model":"m"}` {
		t.Fatalf("preferences=%s err=%v", data, err)
	}
	history = append(history, json.RawMessage(`{"role":"assistant","content":"done"}`))
	if err := db.SaveSession(session, history, metadata, 1); err != nil {
		t.Fatal(err)
	}
	if items, err := db.Items("s1"); err != nil || len(items) != 2 {
		t.Fatalf("appended items=%v err=%v", items, err)
	}
	if err := db.RenameSession("s1", "renamed"); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.Session("s1", "/project")
	if err != nil || loaded.Title != "renamed" {
		t.Fatalf("renamed session=%+v err=%v", loaded, err)
	}
	if err := db.RenameSession("unknown", "renamed"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for unknown session, got %v", err)
	}
}

func TestKV(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, err := db.KVGet("s", "a"); ok || err != nil {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	for _, kv := range [][3]string{{"s", "b", "2"}, {"s", "a1", `"x"`}, {"s", "a2", "3"}, {"p", "a1", "9"}} {
		if err := db.KVSet(kv[0], kv[1], kv[2]); err != nil {
			t.Fatal(err)
		}
	}
	db.KVSet("s", "b", "22")
	if v, ok, _ := db.KVGet("s", "b"); !ok || v != "22" {
		t.Fatalf("overwrite: %q", v)
	}
	if keys, _ := db.KVList("s", "a"); !reflect.DeepEqual(keys, []string{"a1", "a2"}) {
		t.Fatalf("list: %v", keys)
	}
	if keys, _ := db.KVList("s", ""); len(keys) != 3 {
		t.Fatalf("list all: %v", keys)
	}
	if n, _ := db.KVSize("s"); n != len("b22")+len(`a1"x"`)+len("a23") {
		t.Fatalf("size: %d", n)
	}
	db.KVDelete("s", "b")
	db.KVDeleteScope("p")
	if n, _ := db.KVSize("p"); n != 0 {
		t.Fatal("scope not cleared")
	}
}
