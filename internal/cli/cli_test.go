package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"zel/internal/config"
	"zel/internal/core"
	"zel/internal/store"
)

// noSession clears what a surrounding zel session puts in the environment, so
// a test run from an agent's bash is not taken for a child of that session.
func noSession(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ZEL_SOCKET", "ZEL_TOKEN", "ZEL_SESSION_ID", "ZEL_TASK", "ZEL_CONFIG", "ZEL_MODE",
		"ZEL_DEPTH", "ZEL_PARENT", "ZEL_PROVIDER", "ZEL_MODEL", "ZEL_EFFORT", "ZEL_FREEFORM"} {
		t.Setenv(k, "")
	}
}

func zel(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := Run(context.Background(), args, strings.NewReader(stdin), &out, Options{Stderr: io.Discard})
	return out.String(), err
}

func TestParseFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	n := fs.Int("n", 20, "")
	all := fs.Bool("all", false, "")
	rest, err := parseFlags(fs, []string{"a", "-n", "3", "b", "--all", "--", "-x", "c"})
	if err != nil || !reflect.DeepEqual(rest, []string{"a", "b", "-x", "c"}) || *n != 3 || !*all {
		t.Fatalf("rest=%q n=%d all=%v err=%v", rest, *n, *all, err)
	}
	if rest, err := parseFlags(fs, nil); err != nil || len(rest) != 0 {
		t.Fatalf("no args: %q %v", rest, err)
	}
	if _, err := parseFlags(fs, []string{"a", "--bogus"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestConfigShowsTheFile(t *testing.T) {
	noSession(t)
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	t.Chdir(t.TempDir())
	extra := filepath.Join(t.TempDir(), "extra.lua")
	if err := os.WriteFile(filepath.Join(home, "connect.json"), []byte(`{"model":"m1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("return {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := zel(t, "", "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "init.lua") || !strings.Contains(out, `"m1"`) {
		t.Fatalf("unexpected config output:\n%s", out)
	}
	out, err = zel(t, "", "config", "--config", extra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, extra) || strings.Contains(out, "Local:") {
		t.Fatalf("config --config should use extra and omit Local:\n%s", out)
	}
	// Inside a session zel config sees the session's file.
	t.Setenv("ZEL_CONFIG", extra)
	out, err = zel(t, "", "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, extra) {
		t.Fatalf("config in a session:\n%s", out)
	}
}

// -p takes its prompt as the value, from the words after the flags, or from
// stdin, and the other flags may come before or after it.
func TestHeadlessArguments(t *testing.T) {
	noSession(t)
	cwd, _ := os.Getwd()
	dir := t.TempDir()
	type call struct {
		dir  string
		opts core.Options
	}
	for _, tc := range []struct {
		args  []string
		stdin string
		want  call
		err   string
	}{
		{args: []string{"-p", "--resume", "ID", "text"}, want: call{cwd, core.Options{Prompt: "text", Resume: "ID"}}},
		{args: []string{"-p", "hello", "--model", "m", "--effort", "high"}, want: call{cwd, core.Options{Prompt: "hello", Model: "m", Effort: "high"}}},
		{args: []string{"--provider", "p", "-p", "two words"}, want: call{cwd, core.Options{Prompt: "two words", Provider: "p"}}},
		{args: []string{"-p", "--config", "a.toml", "--cwd", dir, "x"}, want: call{dir, core.Options{Prompt: "x", Config: filepath.Join(cwd, "a.toml")}}},
		{args: []string{"-p"}, stdin: "from stdin", want: call{cwd, core.Options{Prompt: "from stdin"}}},
		{args: []string{"-p", "-", "--timeout", "5"}, stdin: "dash", want: call{cwd, core.Options{Prompt: "dash"}}},
		{args: []string{"-p", "--json"}, stdin: " \n", err: "nonempty prompt"},
		{args: []string{"-p", "a", "b"}, err: "unknown command: b"},
		{args: []string{"--model", "m"}, err: "require -p"},
		{args: []string{"-p", "x", "--bogus"}, err: "flag provided but not defined"},
	} {
		var got *call
		open := func(dir string, opts core.Options) (*core.Runtime, error) {
			got = &call{dir, opts}
			return nil, errors.New("not opened")
		}
		var out strings.Builder
		err := Run(context.Background(), tc.args, strings.NewReader(tc.stdin), &out, Options{Open: open, Stderr: io.Discard})
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) || got != nil {
				t.Errorf("%q: err=%v opened=%v", tc.args, err, got != nil)
			}
			continue
		}
		if got == nil || err == nil || err.Error() != "not opened" || ExitCode(err) != 1 {
			t.Errorf("%q: not opened: %v", tc.args, err)
			continue
		}
		if got.opts.Mode != "headless" || got.opts.SessionReady == nil {
			t.Errorf("%q: mode %q", tc.args, got.opts.Mode)
		}
		got.opts.Mode, got.opts.SessionReady = "", nil
		if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("%q: opened %+v, want %+v", tc.args, *got, tc.want)
		}
	}
}

// With --json a failure is one JSON line on stdout and the error code of core
// becomes the exit code.
func TestHeadlessJSONError(t *testing.T) {
	noSession(t)
	open := func(string, core.Options) (*core.Runtime, error) {
		return nil, &core.Error{Code: core.CodeNotFound, Message: "ENOENT: not a child"}
	}
	var out strings.Builder
	err := Run(context.Background(), []string{"-p", "--json", "--resume", "x", "hi"}, strings.NewReader(""), &out, Options{Open: open, Stderr: io.Discard})
	if ExitCode(err) != 3 || !IsReported(err) || out.String() != `{"answer":"","error":"ENOENT: not a child","ok":false,"session":""}`+"\n" {
		t.Fatalf("exit=%d reported=%v out=%q", ExitCode(err), IsReported(err), out.String())
	}
}

func TestHeadlessWithoutProviders(t *testing.T) {
	noSession(t)
	t.Setenv("ZEL_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	out, err := zel(t, "", "-p", "hello")
	if err == nil || !strings.Contains(err.Error(), "No provider") || out != "" {
		t.Fatalf("output=%q err=%v", out, err)
	}
}

// writeConfig writes the provider and selection of a test into connect.json.
func writeConfig(home string, s core.Settings) error {
	path := config.DefaultPaths(home, "", "").Connect
	data, err := json.MarshalIndent(config.Connect{
		Provider:  s.Provider,
		Model:     s.Model,
		Effort:    s.Effort,
		Providers: s.Providers,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// firstLine calls fn with the first line written, once.
type firstLine struct {
	mu sync.Mutex
	strings.Builder
	fn func(string)
}

func (w *firstLine) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.Builder.Write(p)
	if w.fn != nil && strings.Contains(w.String(), "\n") {
		fn := w.fn
		w.fn = nil
		fn(strings.TrimSpace(w.String()))
	}
	return n, err
}

func (w *firstLine) Text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.String()
}

func responseText(w http.ResponseWriter, text string) {
	item, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
}

func TestHeadlessSessionOutputAndOverrides(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			noSession(t)
			home := t.TempDir()
			t.Setenv("ZEL_HOME", home)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			var session string
			requested := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requested = true
				if session == "" {
					t.Error("request arrived before the session ID")
				}
				mu.Unlock()
				var body struct {
					Model        string
					Reasoning    struct{ Effort string }
					Instructions string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "override" || body.Reasoning.Effort != "high" {
					t.Errorf("request overrides: %+v", body)
				}
				switch scenario {
				case "failure":
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"invalid request"}}`)
				case "cancel":
					cancel()
					<-r.Context().Done()
				default:
					responseText(w, "final answer")
				}
			}))
			defer server.Close()
			settings := core.DefaultSettings()
			settings.Provider, settings.Model = "p", "default"
			settings.Providers["p"] = core.Provider{BaseURL: server.URL}
			if err := writeConfig(home, settings); err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			t.Chdir(project)
			project, _ = os.Getwd()
			stderr := &firstLine{fn: func(line string) {
				id, ok := strings.CutPrefix(line, "zel: session ")
				mu.Lock()
				session = id
				mu.Unlock()
				// The session is on disk before its ID is shown.
				state, _ := config.StateHome(project)
				if db, err := store.Open(state); !ok || err != nil || !db.Exists(project, id) {
					t.Errorf("session %q not saved before its ID was shown: %v", line, err)
				}
			}}
			var out strings.Builder
			err := Run(ctx, []string{"-p", "hello", "--provider", "p", "--model", "override", "--effort", "high"}, strings.NewReader(""), &out, Options{Stderr: stderr})
			mu.Lock()
			defer mu.Unlock()
			if !requested || session == "" || stderr.Text() != "zel: session "+session+"\n" {
				t.Fatalf("request=%v stderr=%q err=%v", requested, stderr.Text(), err)
			}
			switch scenario {
			case "success":
				if err != nil || out.String() != "final answer\n" {
					t.Fatalf("stdout=%q err=%v", out.String(), err)
				}
			case "failure":
				if err == nil || out.String() != "" || ExitCode(err) != 1 {
					t.Fatalf("stdout=%q err=%v", out.String(), err)
				}
			case "cancel":
				if out.String() != "" || ExitCode(err) != 130 {
					t.Fatalf("stdout=%q exit=%d", out.String(), ExitCode(err))
				}
			}
			if saved, err := config.LoadConnect(config.DefaultPaths(home, "", "").Connect); err != nil || saved.Model != "default" || saved.Effort != "medium" {
				t.Fatalf("the flags changed the defaults: %+v err=%v", saved, err)
			}
		})
	}
}

func TestHeadlessJSON(t *testing.T) {
	noSession(t)
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { responseText(w, "the answer") }))
	defer server.Close()
	settings := core.DefaultSettings()
	settings.Provider, settings.Model = "p", "m"
	settings.Providers["p"] = core.Provider{BaseURL: server.URL}
	if err := writeConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	var out, stderr strings.Builder
	if err := Run(context.Background(), []string{"-p", "--json", "q"}, strings.NewReader(""), &out, Options{Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK              bool
		Answer, Session string
		Error           string
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil || !got.OK || got.Answer != "the answer" || got.Session == "" || got.Error != "" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q err=%v", out.String(), stderr.String(), err)
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

// A parent `zel -p` whose lua call runs a child `zel -p` and an explicitly invoked script:
// the whole path through the lua tool, the socket and the child's report.
func TestHeadlessLuaEndToEndWithSubagent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the zel binary")
	}
	noSession(t)
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "zel"), "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	var mu sync.Mutex
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string
			Input        []json.RawMessage
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
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
			responseText(w, "child answer")
		case lastOutput != "":
			responseText(w, "parent final: "+lastOutput)
		default:
			args, _ := json.Marshal(map[string]string{"code": `return sh("zel -p 'child task'\n./hello world").out`})
			item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c1", "name": "lua", "arguments": string(args)})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
		}
	}))
	defer server.Close()
	settings := core.DefaultSettings()
	settings.Provider, settings.Model = "p", "m"
	settings.Providers["p"] = core.Provider{BaseURL: server.URL}
	if err := writeConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	project, _ = os.Getwd()
	if err := os.WriteFile(filepath.Join(project, "hello"), []byte("#!/bin/sh\necho \"hello $1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	if err := Run(context.Background(), []string{"-p", "parent"}, strings.NewReader(""), &out, Options{Stderr: &stderr}); err != nil {
		t.Fatalf("run: %v", err)
	}
	text := out.String()
	if !strings.HasPrefix(text, "parent final:") || !strings.Contains(text, "child answer") || !strings.Contains(text, "hello world") {
		t.Fatalf("output: %s", text)
	}
	// The child says its own session on its stderr, and it is a child of the parent.
	parent := strings.TrimSpace(strings.TrimPrefix(stderr.String(), "zel: session "))
	_, rest, ok := strings.Cut(text, "zel: session ")
	child, _, _ := strings.Cut(rest, "\n")
	state, _ := config.StateHome(project)
	db, err := store.Open(state)
	if err != nil || !ok {
		t.Fatalf("no child session in %q: %v", text, err)
	}
	if v, _, err := db.Load(project, child); err != nil || v.Parent != parent || v.Depth != 1 || v.Mode != "headless" {
		t.Fatalf("child %q of %q: %+v %v", child, parent, v, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 3 || prompts[1] != "child task" {
		t.Fatalf("requests: %q", prompts)
	}
}
