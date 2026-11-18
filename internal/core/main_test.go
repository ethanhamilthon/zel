package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
