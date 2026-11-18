package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zel/internal/core"
)

func TestExecCLI(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		output   string
		timedOut bool
	}{
		{"exit and merged streams", []string{"--cwd", dir, "--env", "ZEL_EXEC_TEST=one", "--env", "ZEL_EXEC_TEST=two", "--", "/bin/bash", "-c", `cat; printf ':%s:' "$ZEL_EXEC_TEST"; printf '%s' "${PWD##*/}" >&2; exit 7`}, 7, "input:two:" + filepath.Base(dir), false},
		{"timeout", []string{"--timeout", ".1", "--", "/bin/bash", "-c", `echo started; sleep 10 & wait`}, 124, "started\n", true},
		{"background", []string{"--", "/bin/bash", "-c", `sleep 3 & echo done`}, 0, "done\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			out, err := zel(t, "input", append([]string{"exec"}, tc.args...)...)
			var result execResult
			if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.Code != tc.code || result.Out != tc.output || result.TimedOut != tc.timedOut || result.OK != (tc.code == 0) {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("inherited pipe blocked exec")
			}
		})
	}
	for _, args := range [][]string{{"/bin/echo"}, {"--timeout", "NaN", "--", "/bin/echo"}, {"--env", "oops", "--", "/bin/echo"}} {
		if _, err := zel(t, "", append([]string{"exec"}, args...)...); ExitCode(err) != 2 {
			t.Fatalf("%q: %v", args, err)
		}
	}
}

func TestCLIExplicitSessionAndStdin(t *testing.T) {
	noSession(t)
	t.Setenv("ZEL_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if _, err := zel(t, "", "session", "--json"); ExitCode(err) != 1 || !strings.Contains(err.Error(), "no session") {
		t.Fatal(err)
	}
	project := t.TempDir()
	a, err := core.New(project, core.Options{Mode: "headless", Custom: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := a.SessionID()
	t.Setenv("ZEL_SESSION_ID", "bogus")
	t.Setenv("ZEL_SOCKET", "/bogus")
	t.Setenv("ZEL_TOKEN", "bogus")
	for _, args := range [][]string{{"rename", "-s", id, "my session"}, {"print", "-s", id, "-"}} {
		if _, err := zel(t, "from stdin\n", args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := zel(t, "", "session", "--json", "-s", id)
	var session struct {
		ID, Name, Cwd, Mode string
		Modes               []string
	}
	if err != nil || json.Unmarshal([]byte(out), &session) != nil || session.ID != id || session.Name != "my session" || session.Cwd != project || session.Mode != "headless" || len(session.Modes) != 2 {
		t.Fatalf("session=%q err=%v", out, err)
	}
	t.Setenv("ZEL_SESSION_ID", id) // inherited socket variables still point elsewhere
	if reply, err := zel(t, "", "session", "--json"); err != nil || reply != out {
		t.Fatalf("inherited ID: %q %v", reply, err)
	}
	if records := a.Records(); len(records) != 1 || records[0].Kind != "print" || records[0].Text != "from stdin\n" {
		t.Fatal(records)
	}
	if _, err := zel(t, "", "choice", "-s", id, "question?", "yes"); ExitCode(err) != 6 {
		t.Fatal(err)
	}
	a.Close()
	if _, err := zel(t, "", "session", "--json", "-s", id); ExitCode(err) != 1 {
		t.Fatal(err)
	}
}

func TestResetDefaults(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "defaults")
	old := filepath.Join(dir, "obsolete.lua")
	init := filepath.Join(home, "init.lua")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old, init} {
		if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetDefaults(home, nil); err == nil {
		t.Fatal("empty bundle passed")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal(err)
	}
	if err := resetDefaults(home, map[string]string{"sub/new.lua": "return true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file: %v", err)
	}
	for path, want := range map[string]string{init: "unchanged", filepath.Join(dir, "sub/new.lua"): "return true"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", path, data, err)
		}
	}
}
