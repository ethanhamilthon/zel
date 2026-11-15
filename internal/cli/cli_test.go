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
	"sync/atomic"
	"testing"
	"time"
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
		{[]string{"events"}, false},
		{[]string{"session"}, false},
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

func TestRemovedCommands(t *testing.T) {
	for _, args := range [][]string{{"init"}, {"session", "-s", "x"}, {"models"}, {"events"}, {"--config", "x.lua"}, {"--config=x.lua"}} {
		var out strings.Builder
		err := Run(context.Background(), args, strings.NewReader(""), &out, Options{})
		if err == nil || !strings.Contains(err.Error(), "removed in zel 0.6") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	var out strings.Builder
	if err := Run(context.Background(), []string{"frobnicate"}, strings.NewReader(""), &out, Options{}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
}

// writeProjectInit makes a project directory with .zel/init.lua and enters it.
func writeProjectInit(t *testing.T, initLua string) string {
	t.Helper()
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".zel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zel", "init.lua"), []byte(initLua), 0o600); err != nil {
		t.Fatal(err)
	}
	return project
}

func TestHeadlessWithoutProviders(t *testing.T) {
	t.Setenv("ZEL_HOME", t.TempDir())
	var out strings.Builder
	err := Run(context.Background(), []string{"-p", "hello"}, strings.NewReader(""), &out, Options{})
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
			writeProjectInit(t, `return {instructions = "headless config"}`)
			out := &headlessOutput{first: func(id string) {
				session = id
				// The ID must already be readable, without acquiring or changing the session.
				if err := sessionSaved(home, id); err != nil {
					t.Errorf("session not persisted before stdout: %v", err)
				}
			}}
			err := Run(ctx, []string{"-p", "hello", "--provider", "p", "--model", "override", "--effort", "high"}, strings.NewReader(""), out, Options{})
			if !requested || session == "" {
				t.Fatalf("request=%v session=%q err=%v", requested, session, err)
			}
			if scenario == "success" {
				if err != nil || out.String() != session+"\nfinal answer\n" {
					t.Fatalf("stdout=%q err=%v", out.String(), err)
				}
			} else {
				if err == nil || out.String() != session+"\n" {
					t.Fatalf("failure stdout=%q err=%v", out.String(), err)
				}
				if scenario == "cancel" && ExitCode(err) != 130 {
					t.Fatalf("cancel exit=%d", ExitCode(err))
				}
			}
			saved, err := core.LoadConfig(home)
			if err != nil || saved.Model != "default" || saved.Effort != "medium" {
				t.Fatalf("defaults changed: %+v err=%v", saved, err)
			}
		})
	}
}

// sessionSaved checks that the session is in the database, without opening it.
func sessionSaved(home, id string) error {
	db, err := store.Open(filepath.Join(home, "zel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.SessionProject(id)
	return err
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
	t.Setenv("ZEL_MODE", "")
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
			if !strings.Contains(body.Instructions, "proj.hello{...}  -- greet") || !strings.Contains(body.Instructions, "Project rule: be brief") {
				t.Errorf("the project layer is missing from the prompt:\n%s", body.Instructions)
			}
			code := "local r = new_agent{prompt = 'child task'}\nprint(r.ok, r.answer)\nprint(proj.hello{who = 'world'})"
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
	writeProjectInit(t, `return {
  instructions = "Project rule: be brief",
  tools = {["proj.hello"] = {desc = "greet", fn = function(a) return "hello " .. a.who end}},
}`)
	var out strings.Builder
	if err := Run(context.Background(), []string{"-p", "parent"}, strings.NewReader(""), &out, Options{}); err != nil {
		t.Fatalf("run: %v", err)
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

// A headless prompt that is cancelled while its completions expand is never sent:
// no request reaches the provider, the run exits as cancelled and the session
// keeps no turn.
func TestHeadlessCancelDuringPromptExpansionSendsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"error":{"message":"unexpected"}}`)
	}))
	defer server.Close()
	settings := core.DefaultSettings()
	settings.Provider, settings.Model = "p", "default"
	settings.Providers["p"] = core.Provider{BaseURL: server.URL}
	if err := core.WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	writeProjectInit(t, `return {completions = {slow = function() sh("sleep 30") return "never" end}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out strings.Builder
	start := time.Now()
	err := Run(ctx, []string{"-p", "do #slow"}, strings.NewReader(""), &out, Options{})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("cancelled expansion took %s", took)
	}
	if ExitCode(err) != 130 {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d provider requests after a cancelled expansion", n)
	}
	if strings.Contains(out.String(), "never") {
		t.Fatalf("stdout carries text from a cancelled expansion: %q", out.String())
	}
}
