package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain puts a real zel binary on PATH: the defaults of the agent VM call
// `zel exec`, `zel status` and the other subcommands.
func TestMain(m *testing.M) {
	bin, err := os.MkdirTemp("", "zel-core-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "zel"), "../..")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build zel: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(bin)
	os.Exit(code)
}

func TestExecBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zel", "exec", "--timeout", ".5", "--cwd", t.TempDir(),
		"--env", "GREETING=hello", "--", "/bin/bash", "-c", `cat; printf '%s\n' "$GREETING" >&2; sleep 10`)
	cmd.Stdin = strings.NewReader("from stdin\n")
	out, err := cmd.CombinedOutput()
	var result struct {
		OK      bool
		Code    int
		Out     string
		Timeout bool `json:"timed_out"`
	}
	if err != nil || json.Unmarshal(out, &result) != nil || result.OK || result.Code != 124 ||
		result.Out != "from stdin\nhello\n" || !result.Timeout {
		t.Fatalf("standalone exec timeout: %q, %v", out, err)
	}

	cmd = exec.CommandContext(ctx, "zel", "exec", "--bogus", "--", "/bin/true")
	if out, err := cmd.CombinedOutput(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "usage:") {
		t.Fatalf("standalone exec usage: %q, %v", out, err)
	}
}
