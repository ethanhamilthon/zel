package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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
		{[]string{"tasks"}, false},
		{[]string{"view"}, false},
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
			config := filepath.Join(t.TempDir(), "init.lua")
			if err := os.WriteFile(config, []byte("require('zel').instructions('headless config')"), 0600); err != nil {
				t.Fatal(err)
			}
			cwd, _ := os.Getwd()
			out := &headlessOutput{first: func(id string) {
				session = id
				// The ID must already be readable, without acquiring or changing the session.
				if _, err := core.ReadSession(cwd, id); err != nil {
					t.Errorf("session not persisted before stdout: %v", err)
				}
			}}
			var diagnostics strings.Builder
			err := Run(ctx, []string{"-p", "hello", "--provider", "p", "--model", "override", "--effort", "high", "--config", config}, strings.NewReader(""), out, &diagnostics, Options{})
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
	if !strings.Contains(out.String(), "Created .zel/init.lua") {
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

func TestUpdateReplacesBinaryWithVerifiedRelease(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	payload := []byte("new zel")
	_ = tw.WriteHeader(&tar.Header{Name: "zel", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	asset := fmt.Sprintf("zel_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprint(w, `{"tag_name":"v9.0.0"}`)
		case "/download/v9.0.0/" + asset:
			w.Write(archive.Bytes())
		case "/download/v9.0.0/checksums.txt":
			fmt.Fprint(w, checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "zel")
	if err := os.WriteFile(target, []byte("old zel"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldLatest, oldDownload, oldExe, oldVersion := releaseLatest, releaseDownload, executablePath, Version
	t.Cleanup(func() {
		releaseLatest, releaseDownload, executablePath, Version = oldLatest, oldDownload, oldExe, oldVersion
	})
	releaseLatest, releaseDownload = server.URL+"/latest", server.URL+"/download"
	executablePath = func() (string, error) { return target, nil }

	Version = "9.0.0"
	var out strings.Builder
	if err := updateCommand(context.Background(), nil, &out); err != nil || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("same version: %q %v", out.String(), err)
	}
	Version = "0.4.0"
	checksums = strings.Repeat("0", 64) + "  " + asset + "\n"
	if err := updateCommand(context.Background(), nil, &out); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	checksums = hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if err := updateCommand(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new zel" {
		t.Fatalf("binary = %q %v", data, err)
	}
	if newerVersion("0.10.0", "0.9.9") >= 0 || newerVersion("0.3.7", "0.4.0") <= 0 {
		t.Fatal("version order")
	}
}

// A parent `zel -p` run with an init.lua plugin function and a subagent: the whole
// path through the lua tool, the host API, agents.run and the child's report.
func TestHeadlessLuaEndToEndWithSubagent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the zel binary")
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "zel"), "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	t.Setenv("ZEL_CONFIG", "")
	t.Setenv("ZEL_DEPTH", "0")
	t.Setenv("ZEL_SESSION_ID", "")
	var mu sync.Mutex
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body struct {
			Instructions string
			Input        []json.RawMessage
		}
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		reply := func(item string) {
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
		}
		text := func(s string) {
			item, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": s}}})
			reply(string(item))
		}
		var first, lastOutput string
		for _, in := range body.Input {
			var m struct {
				Role, Type, Output string
				Content            json.RawMessage
			}
			_ = json.Unmarshal(in, &m)
			if m.Role == "user" && first == "" {
				_ = json.Unmarshal(m.Content, &first)
			}
			if m.Type == "function_call_output" {
				lastOutput = m.Output
			}
		}
		mu.Lock()
		prompts = append(prompts, first)
		mu.Unlock()
		switch {
		case first == "child task":
			text("child answer")
		case lastOutput != "":
			text("parent final: " + lastOutput)
		default:
			if !strings.Contains(body.Instructions, "proj.hello(who: string)") || !strings.Contains(body.Instructions, "Project rule: be brief") {
				t.Errorf("the project layer is missing from the prompt:\n%s", body.Instructions)
			}
			code := "local r = agents.run{prompt = 'child task'}\nprint(r.ok, r.answer)\nprint(proj.hello('world'))\nstate.set('seen', 1, 'project')"
			args, _ := json.Marshal(map[string]string{"code": code})
			item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c1", "name": "lua", "arguments": string(args)})
			reply(string(item))
		}
	}))
	defer server.Close()
	settings := core.DefaultSettings()
	settings.Provider, settings.Model = "p", "m"
	settings.Providers["p"] = core.Provider{BaseURL: server.URL}
	if err := core.WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".zel"), 0o755); err != nil {
		t.Fatal(err)
	}
	initLua := `local zel = require("zel")
zel.instructions("Project rule: be brief")
zel.fn{name = "proj.hello", summary = "greet", params = {{"who", "string"}}, run = function(who) return "hello " .. who end}`
	if err := os.WriteFile(filepath.Join(project, ".zel", "init.lua"), []byte(initLua), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics strings.Builder
	if err := Run(context.Background(), []string{"-p", "parent"}, strings.NewReader(""), &out, &diagnostics, Options{}); err != nil {
		t.Fatalf("run: %v\n%s", err, diagnostics.String())
	}
	if !strings.Contains(out.String(), "parent final:") || !strings.Contains(out.String(), "true\tchild answer") || !strings.Contains(out.String(), "hello world") {
		t.Fatalf("output: %s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(prompts, "child task") {
		t.Fatalf("the child never ran: %q", prompts)
	}
}
