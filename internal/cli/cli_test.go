package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"zel/internal/core"
	"zel/internal/store"
)

func TestIsHelper(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"version"}, true},
		{[]string{"help"}, true},
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"tasks"}, true},
		{[]string{"view"}, true},
		{[]string{"events"}, true},
		{[]string{"session"}, true},
		{[]string{"write"}, false},
		{[]string{"patch"}, false},
		{[]string{"unknown"}, false},
	}
	for _, tc := range cases {
		if got := IsHelper(tc.args); got != tc.want {
			t.Errorf("IsHelper(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseAll(t *testing.T) {
	var s string
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.StringVar(&s, "s", "", "session")
	positional, err := parseAll(fs, []string{"run", "-s", "123", "echo", "hi"})
	if err != nil {
		t.Fatalf("parseAll error: %v", err)
	}
	if s != "123" {
		t.Errorf("flag s = %q, want 123", s)
	}
	want := []string{"run", "echo", "hi"}
	if !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %v, want %v", positional, want)
	}
}

func TestHeadlessWithoutProviders(t *testing.T) {
	t.Setenv("ZEL_HOME", t.TempDir())
	t.Setenv("ZEL_CONFIG", "")
	var out, diagnostics strings.Builder
	err := Run(context.Background(), []string{"-p", "hello"}, strings.NewReader(""), &out, &diagnostics, Options{})
	if err == nil || !strings.Contains(err.Error(), "No provider") || out.Len() != 0 {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

type headlessOutput struct {
	strings.Builder
	first func(string)
}

func (w *headlessOutput) Write(p []byte) (int, error) {
	n, err := w.Builder.Write(p)
	if w.first != nil && strings.Contains(w.String(), "\n") {
		fn := w.first
		w.first = nil
		fn(strings.TrimSpace(w.String()))
	}
	return n, err
}

func TestHeadlessSessionOutputAndOverrides(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ZEL_HOME", home)
			t.Setenv("ZEL_CONFIG", "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var session string
			requested := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested = true
				if session == "" {
					t.Error("request arrived before session ID")
				}
				var body struct {
					Model        string
					Reasoning    struct{ Effort string }
					Instructions string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "override" || body.Reasoning.Effort != "high" || !strings.Contains(body.Instructions, "headless config") {
					t.Errorf("request overrides: %+v", body)
				}
				if scenario == "failure" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"invalid request"}}`)
					return
				}
				if scenario == "cancel" {
					cancel()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"final answer\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"final answer\"}]}]}}\n\n")
			}))
			defer server.Close()
			settings := core.DefaultSettings()
			settings.Provider, settings.Model = "p", "default"
			settings.Providers["p"] = core.Provider{BaseURL: server.URL}
			if err := core.WriteConfig(home, settings); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "instructions.md")
			if err := os.WriteFile(config, []byte("---\nhooks:\n  app_start: |\n    printf '%s/%s/%s/%s' \"$ZEL_MODE\" \"$ZEL_PROVIDER\" \"$ZEL_MODEL\" \"$ZEL_EFFORT\" > hook-values\n---\nheadless config"), 0600); err != nil {
				t.Fatal(err)
			}
			cwd, _ := os.Getwd()
			out := &headlessOutput{first: func(id string) {
				session = id
				if _, err := os.Stat(filepath.Join(filepath.Dir(config), "hook-values")); !os.IsNotExist(err) {
					t.Errorf("startup hooks ran before session ID: %v", err)
				}
				// The ID must already be readable, without acquiring or changing the session.
				if _, err := core.ReadSession(cwd, id); err != nil {
					t.Errorf("session not persisted before stdout: %v", err)
				}
			}}
			var diagnostics strings.Builder
			err := Run(ctx, []string{"-p", "hello", "--provider", "p", "--model", "override", "--effort", "high", "--config", config}, strings.NewReader(""), out, &diagnostics, Options{})
			hook, hookErr := os.ReadFile(filepath.Join(filepath.Dir(config), "hook-values"))
			if hookErr != nil || string(hook) != "headless/p/override/high" {
				t.Fatalf("startup hook overrides: %q err=%v", hook, hookErr)
			}
			if !requested || session == "" {
				t.Fatalf("request=%v session=%q err=%v", requested, session, err)
			}
			if scenario == "success" {
				if err != nil || out.String() != session+"\nfinal answer\n" {
					t.Fatalf("stdout=%q err=%v", out.String(), err)
				}
				records, err := core.ReadSession(cwd, session)
				if err != nil || len(records) < 2 || records[len(records)-1].Text != "final answer" {
					t.Fatalf("saved records=%v err=%v", records, err)
				}
				var snapshot strings.Builder
				if err := Run(context.Background(), []string{"session", "-s", session}, strings.NewReader(""), &snapshot, &diagnostics, Options{}); err != nil || !strings.Contains(snapshot.String(), "final answer") {
					t.Fatalf("session helper: %q err=%v", snapshot.String(), err)
				}
			} else {
				if err == nil || out.String() != session+"\n" {
					t.Fatalf("failure stdout=%q err=%v", out.String(), err)
				}
				if scenario == "cancel" && ExitCode(err) != 130 {
					t.Fatalf("cancel exit=%d", ExitCode(err))
				}
			}
			saved, err := core.LoadConfig(home, cwd)
			if err != nil || saved.Model != "default" || saved.Effort != "medium" {
				t.Fatalf("defaults changed: %+v err=%v", saved, err)
			}
		})
	}
}

func TestInitCommand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if !IsHelper([]string{"init"}) {
		t.Fatal("init must not start a session")
	}
	var out, diagnostics strings.Builder
	if err := Run(context.Background(), []string{"init"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Created ZEL.md") {
		t.Fatalf("output: %s", out.String())
	}
	if err := Run(context.Background(), []string{"init", "extra"}, strings.NewReader(""), &out, &diagnostics, Options{}); err == nil {
		t.Fatal("expected usage error")
	}
}

func TestSessionRenameCommand(t *testing.T) {
	t.Setenv("ZEL_SESSION_ID", "")
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)

	db, err := store.Open(filepath.Join(home, "zel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSession(store.Session{ID: "s1", Project: cwd, First: "first prompt"}, nil, nil, 0); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	var out, diagnostics strings.Builder

	// Missing args cases
	for _, args := range [][]string{
		{"session", "rename"},
		{"session", "rename", "s1"},
		{"session", "rename", "s1", ""},
		{"session", "rename", "s1", "   "},
		{"session", "rename", "", "new title"},
	} {
		if err := Run(context.Background(), args, strings.NewReader(""), &out, &diagnostics, Options{}); err == nil {
			t.Fatalf("expected error for missing args %v", args)
		}
	}

	// Unknown session ID
	if err := Run(context.Background(), []string{"session", "rename", "unknown-session", "new title"}, strings.NewReader(""), &out, &diagnostics, Options{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error for unknown session, got %v", err)
	}

	// Successful rename
	if err := Run(context.Background(), []string{"session", "rename", "s1", "my custom title"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatalf("rename failed: %v", err)
	}

	db, err = store.Open(filepath.Join(home, "zel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	loaded, err := db.Session("s1", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "my custom title" {
		t.Fatalf("expected title %q, got %q", "my custom title", loaded.Title)
	}

	summaries, err := db.Sessions(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) == 0 || summaries[0].Title != "my custom title" {
		t.Fatalf("expected summary title %q, got %+v", "my custom title", summaries)
	}

	// Rename with multiple unquoted arguments (positional)
	if err := Run(context.Background(), []string{"session", "rename", "s1", "another", "new", "title"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatalf("multi-arg rename failed: %v", err)
	}
	loaded, err = db.Session("s1", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "another new title" {
		t.Fatalf("expected title %q, got %q", "another new title", loaded.Title)
	}

	// Rename with -s flag
	if err := Run(context.Background(), []string{"session", "rename", "-s", "s1", "flag", "title", "here"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatalf("-s flag rename failed: %v", err)
	}
	loaded, err = db.Session("s1", cwd)
	if err != nil || loaded.Title != "flag title here" {
		t.Fatalf("expected title %q, got %q (err: %v)", "flag title here", loaded.Title, err)
	}

	// Rename defaulting to ZEL_SESSION_ID; ensure multi-word name is not mistaken for an ID
	t.Setenv("ZEL_SESSION_ID", "s1")
	if err := Run(context.Background(), []string{"session", "rename", "env", "multi", "word", "title"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatalf("ZEL_SESSION_ID rename failed: %v", err)
	}
	loaded, err = db.Session("s1", cwd)
	if err != nil || loaded.Title != "env multi word title" {
		t.Fatalf("expected title %q, got %q (err: %v)", "env multi word title", loaded.Title, err)
	}

	// Help text includes session rename
	var helpOut strings.Builder
	if err := Run(context.Background(), []string{"help"}, strings.NewReader(""), &helpOut, &diagnostics, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(helpOut.String(), "session rename") {
		t.Fatalf("help text missing session rename: %s", helpOut.String())
	}
}
