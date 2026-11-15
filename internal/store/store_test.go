package store

import (
	"encoding/json"
	"path/filepath"
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
}
