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

	"zel/internal/core"
	"zel/internal/ipc"
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

func TestIsHelper(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"help"}, {"--help"}, {"-h"}, {"task", "list"}, {"choice"}, {"view"},
		{"session"}, {"edit", "f"}, {"docs"}, {"models"}, {"config"}} {
		if !IsHelper(args) {
			t.Errorf("IsHelper(%v) = false", args)
		}
	}
	for _, args := range [][]string{nil, {}, {"-p", "x"}, {"update"}, {"tasks"}, {"init"}, {"unknown"}} {
		if IsHelper(args) {
			t.Errorf("IsHelper(%v) = true", args)
		}
	}
}

func TestRemovedCommands(t *testing.T) {
	for _, args := range [][]string{{"init"}, {"events"}, {"agent", "x"}, {"init=x"}} {
		if _, err := zel(t, "", args...); err == nil || !strings.Contains(err.Error(), "removed in zel 0.7") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if _, err := zel(t, "", "frobnicate"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
	if _, err := zel(t, "", "version", "x"); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("version x: %v", err)
	}
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

func TestShellJoin(t *testing.T) {
	if got := shellJoin([]string{"echo $HOME | wc -c"}); got != "echo $HOME | wc -c" {
		t.Fatalf("one argument is a script: %q", got)
	}
	argv := []string{"printf", "<%s>", "a b", "it's", "", "$HOME", "x;y", "./a-b_c=1:2,3+@%"}
	joined := shellJoin(argv)
	if !strings.HasPrefix(joined, "printf '<%s>' 'a b' 'it'\\''s' '' '$HOME'") || !strings.HasSuffix(joined, " ./a-b_c=1:2,3+@%") {
		t.Fatalf("joined = %q", joined)
	}
	out, err := exec.Command("bash", "-c", joined).Output()
	if want := "<a b><it's><><$HOME><x;y><./a-b_c=1:2,3+@%>"; err != nil || string(out) != want {
		t.Fatalf("bash got %q (%v), want %q", out, err, want)
	}
}

func TestEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\na\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	block := func(old, new, end string) string {
		return "<<<<<<< old\n" + old + "\n=======\n" + new + "\n>>>>>>> new" + end + "\n"
	}
	content := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	out, err := zel(t, block("b", "B", "")+block("a", "A", " all"), "edit", path)
	if err != nil || out != "3\n" || content() != "A\nB\nA\n" {
		t.Fatalf("edit: out=%q err=%v file=%q", out, err, content())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}
	for _, tc := range []struct {
		name, stdin string
		args        []string
		code        int
		msg         string
	}{
		// A later block that fails leaves the file as it was.
		{"no match", block("B", "b", "") + block("zzz", "y", ""), []string{path}, 4, "ENOMATCH"},
		{"ambiguous", block("A", "a", ""), []string{path}, 5, "EAMBIG"},
		{"missing file", block("A", "a", ""), []string{path + ".none"}, 3, "ENOENT"},
		{"bad blocks", "just text\n", []string{path}, 2, "EINVAL"},
		{"no file", block("A", "a", ""), nil, 2, "usage"},
	} {
		out, err := zel(t, tc.stdin, append([]string{"edit"}, tc.args...)...)
		if ExitCode(err) != tc.code || !strings.Contains(fmt.Sprint(err), "zel edit: ") || !strings.Contains(fmt.Sprint(err), tc.msg) || out != "" {
			t.Errorf("%s: exit=%d err=%v out=%q", tc.name, ExitCode(err), err, out)
		}
		if content() != "A\nB\nA\n" {
			t.Fatalf("%s changed the file: %q", tc.name, content())
		}
	}
	if out, err := zel(t, block("A", "a", ""), "edit", "--all", path); err != nil || out != "2\n" || content() != "a\nB\na\n" {
		t.Fatalf("--all: out=%q err=%v file=%q", out, err, content())
	}
}

func TestSessionCommandsNeedASession(t *testing.T) {
	noSession(t)
	for _, args := range [][]string{{"task", "list"}, {"task", "run", "--", "echo", "hi"}, {"task", "wait", "t1"},
		{"choice", "Q", "A"}, {"view", "x.png"}, {"session"}, {"session", "--json"}, {"session", "rename", "new name"}} {
		out, err := zel(t, "", args...)
		if ExitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "not inside a zel session") || out != "" {
			t.Errorf("%v: exit=%d err=%v out=%q", args, ExitCode(err), err, out)
		}
	}
	// Bad usage is reported before the session is looked for.
	for _, args := range [][]string{{"task"}, {"task", "frob"}, {"task", "get"}, {"task", "run"}, {"choice", "Q"}, {"view"}, {"session", "x"}} {
		if _, err := zel(t, "", args...); ExitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "usage: zel ") {
			t.Errorf("%v: exit=%d err=%v", args, ExitCode(err), err)
		}
	}
}

// The subcommands talk to the session over its socket: the request carries the
// token, ZEL_TASK and the working directory, Out goes to stdout and the error
// code of the answer becomes the exit code.
func TestSessionCommandsOverSocket(t *testing.T) {
	noSession(t)
	path := ipc.Path(t.TempDir(), "session")
	var mu sync.Mutex
	var reqs []ipc.Request
	server, err := ipc.Listen(path, "tok", func(ctx context.Context, req ipc.Request) ipc.Response {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		switch req.Op {
		case "task.list":
			return ipc.Response{OK: true, Out: "t1 running build 1s\n"}
		case "task.run":
			return ipc.Response{OK: true, Out: "t2\n"}
		case "task.wait":
			return ipc.Response{Out: "t1 running\n", Code: 124, Error: "ETIMEOUT: 1 of 1 tasks still run"}
		case "task.get":
			return ipc.Response{Code: 3, Error: "ENOENT: no task t9"}
		case "session.get":
			return ipc.Response{OK: true, Out: "id: s1\n", Data: json.RawMessage(`{"id":"s1"}`)}
		}
		return ipc.Response{Error: "EFAIL: no code"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	t.Setenv("ZEL_SOCKET", path)
	t.Setenv("ZEL_TOKEN", "tok")
	t.Setenv("ZEL_TASK", "t7")
	cwd, _ := os.Getwd()
	last := func() (ipc.Request, map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		r := reqs[len(reqs)-1]
		var args map[string]any
		if len(r.Args) > 0 && string(r.Args) != "null" {
			if err := json.Unmarshal(r.Args, &args); err != nil {
				t.Fatal(err)
			}
		}
		return r, args
	}

	out, err := zel(t, "", "task", "list")
	if r, _ := last(); err != nil || out != "t1 running build 1s\n" || r.Op != "task.list" || r.Token != "tok" || r.Task != "t7" || r.Cwd != cwd {
		t.Fatalf("task list: out=%q err=%v req=%+v", out, err, r)
	}
	out, err = zel(t, "", "task", "run", "--name", "build", "--", "go", "test", "it's")
	if r, a := last(); err != nil || out != "t2\n" || r.Op != "task.run" || a["name"] != "build" || a["cmd"] != `go test 'it'\''s'` {
		t.Fatalf("task run: out=%q err=%v args=%v", out, err, a)
	}
	if _, err = zel(t, "make test\n", "task", "run"); err != nil {
		t.Fatal(err)
	}
	if _, a := last(); a["cmd"] != "make test\n" {
		t.Fatalf("task run from stdin: %v", a)
	}
	out, err = zel(t, "", "task", "wait", "t1", "--timeout", "1.5")
	if _, a := last(); ExitCode(err) != 124 || !strings.Contains(err.Error(), "zel task: ETIMEOUT") || out != "t1 running\n" ||
		!reflect.DeepEqual(a["ids"], []any{"t1"}) || a["timeout"] != 1.5 {
		t.Fatalf("task wait: exit=%d err=%v out=%q args=%v", ExitCode(err), err, out, a)
	}
	if _, err = zel(t, "", "task", "get", "t9"); ExitCode(err) != 3 || !strings.Contains(err.Error(), "ENOENT") {
		t.Fatalf("task get: exit=%d err=%v", ExitCode(err), err)
	}
	if _, err = zel(t, "", "choice", "Q", "A"); ExitCode(err) != 1 || !strings.Contains(err.Error(), "zel choice: EFAIL") {
		t.Fatalf("an error without a code: exit=%d err=%v", ExitCode(err), err)
	}
	if out, err = zel(t, "", "session", "--json"); err != nil || out != "{\"id\":\"s1\"}\n" {
		t.Fatalf("session --json: %q %v", out, err)
	}
	t.Setenv("ZEL_TOKEN", "wrong")
	if _, err = zel(t, "", "task", "list"); ExitCode(err) != 6 || !strings.Contains(err.Error(), "EDENIED") {
		t.Fatalf("wrong token: exit=%d err=%v", ExitCode(err), err)
	}
	t.Setenv("ZEL_SOCKET", path+".gone")
	if _, err = zel(t, "", "task", "list"); ExitCode(err) != 1 || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("no server: exit=%d err=%v", ExitCode(err), err)
	}
}

func TestConfigShowsLayers(t *testing.T) {
	noSession(t)
	home := t.TempDir()
	t.Setenv("ZEL_HOME", home)
	t.Chdir(t.TempDir())
	extra := filepath.Join(t.TempDir(), "extra.toml")
	for file, text := range map[string]string{filepath.Join(home, "zel.toml"): "model = \"m1\"\n", extra: "model = \"m2\"\n"} {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// model is the line of the model setting: its value and the file it comes from.
	model := func(args ...string) string {
		out, err := zel(t, "", append([]string{"config"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 4 && f[0] == "model" {
				return f[2] + " " + f[3]
			}
		}
		t.Fatalf("no model line:\n%s", out)
		return ""
	}
	if got := model(); got != `"m1" `+filepath.Join(home, "zel.toml") {
		t.Fatalf("config: %s", got)
	}
	if got := model("--config", extra); got != `"m2" `+extra {
		t.Fatalf("config --config: %s", got)
	}
	// Inside a session zel config sees the session's layers.
	t.Setenv("ZEL_CONFIG", filepath.Join(home, "zel.toml")+string(os.PathListSeparator)+extra)
	if got := model(); got != `"m2" `+extra {
		t.Fatalf("config in a session: %s", got)
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
		{args: []string{"-p", "--config", "a.toml", "--config", "b.toml", "--cwd", dir, "x"}, want: call{dir, core.Options{Prompt: "x", Configs: []string{"a.toml", "b.toml"}}}},
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

// writeLocalConfig makes a project directory with ./zel.toml and enters it.
func writeLocalConfig(t *testing.T, toml string) string {
	t.Helper()
	project := t.TempDir()
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(project, "zel.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return project
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
				if body.Model != "override" || body.Reasoning.Effort != "high" || !strings.Contains(body.Instructions, "headless config") || strings.Contains(body.Instructions, "main only") {
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
			if err := core.WriteConfig(home, settings); err != nil {
				t.Fatal(err)
			}
			project := writeLocalConfig(t, `instructions = ["main only", {text = "headless config", headless = true, main = false}]`)
			stderr := &firstLine{fn: func(line string) {
				id, ok := strings.CutPrefix(line, "zel: session ")
				mu.Lock()
				session = id
				mu.Unlock()
				// The session is on disk before its ID is shown.
				if db, err := store.Open(home); !ok || err != nil || !db.Exists(project, id) {
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
			if saved := core.LoadConfig(home, project); saved.Model != "default" || saved.Effort != "medium" {
				t.Fatalf("the flags changed the defaults: %+v", saved)
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
	if err := core.WriteConfig(home, settings); err != nil {
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

// A parent `zel -p` whose bash runs a child `zel -p` and a script of ./.zel/bin:
// the whole path through the bash tool, the socket and the child's report.
func TestHeadlessBashEndToEndWithSubagent(t *testing.T) {
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
			if !strings.Contains(body.Instructions, "Project rule: be brief") {
				t.Errorf("the project layer is missing from the prompt:\n%s", body.Instructions)
			}
			args, _ := json.Marshal(map[string]string{"command": "zel -p 'child task'\nhello world"})
			item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": string(args)})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
		}
	}))
	defer server.Close()
	settings := core.DefaultSettings()
	settings.Provider, settings.Model = "p", "m"
	settings.Providers["p"] = core.Provider{BaseURL: server.URL}
	if err := core.WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	project := writeLocalConfig(t, `instructions = [{text = "Project rule: be brief", headless = true}]`)
	if err := os.MkdirAll(filepath.Join(project, ".zel", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zel", "bin", "hello"), []byte("#!/bin/sh\necho \"hello $1\"\n"), 0o755); err != nil {
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
	db, err := store.Open(home)
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
