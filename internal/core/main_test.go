package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	type execOut struct {
		OK      bool
		Code    int
		Out     string
		Timeout bool `json:"timed_out"`
	}

	cmd := exec.CommandContext(ctx, "zel", "exec", "--timeout", ".5", "--cwd", t.TempDir(),
		"--env", "GREETING=hello", "--", "/bin/bash", "-c", `cat; printf '%s\n' "$GREETING" >&2; sleep 10`)
	cmd.Stdin = strings.NewReader("from stdin\n")
	out, err := cmd.CombinedOutput()
	var result execOut
	if err != nil || json.Unmarshal(out, &result) != nil || result.OK || result.Code != 124 ||
		result.Out != "from stdin\nhello\n" || !result.Timeout {
		t.Fatalf("standalone exec timeout: %q, %v", out, err)
	}

	cmd = exec.CommandContext(ctx, "zel", "exec", "--", "/bin/bash", "-c", `printf '%524288s\n' ''; exit 7`)
	out, err = cmd.CombinedOutput()
	if err != nil || json.Unmarshal(out, &result) != nil || result.OK || result.Code != 7 || len(result.Out) != 524289 {
		t.Fatalf("standalone exec large/exit: len=%d code=%d ok=%v err=%v", len(result.Out), result.Code, result.OK, err)
	}

	started := time.Now()
	cmd = exec.CommandContext(ctx, "zel", "exec", "--", "/bin/bash", "-c", `sleep 3 & echo done`)
	out, err = cmd.CombinedOutput()
	if err != nil || json.Unmarshal(out, &result) != nil || !result.OK || result.Code != 0 || result.Out != "done\n" {
		t.Fatalf("standalone exec background: %q, %v", out, err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("background job blocked standalone exec for %v", time.Since(started))
	}

	cmd = exec.CommandContext(ctx, "zel", "exec", "--bogus", "--", "/bin/true")
	if out, err := cmd.CombinedOutput(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "usage:") {
		t.Fatalf("standalone exec usage: %q, %v", out, err)
	}
}

func TestExecLauncherKilledWithUndrainedStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()

	dir := t.TempDir()
	pubOutR, pubOutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pubOutR.Close()
	defer pubOutW.Close()

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer errR.Close()
	defer errW.Close()

	script := `printf '%524288s\n' ''
printf '%s %s\n' "$PPID" "$$" > pids.tmp
mv pids.tmp pids
(sleep 5; touch descendant) &
sleep 10
touch foreground
wait`
	cmd := exec.CommandContext(ctx, "zel", "exec", "--cwd", dir, "--", "/bin/bash", "-c", script)
	cmd.Stdout = pubOutW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = pubOutW.Close()
	_ = errW.Close()

	var workerPID, childPID int
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if childPID > 0 {
			_ = syscall.Kill(-childPID, syscall.SIGKILL)
		}
		if workerPID > 0 {
			_ = syscall.Kill(-workerPID, syscall.SIGKILL)
		}
	})

	pidPath := filepath.Join(dir, "pids")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				workerPID, _ = strconv.Atoi(fields[0])
				childPID, _ = strconv.Atoi(fields[1])
				if workerPID > 0 && childPID > 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for worker/child pids: %v", err)
		}
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill launcher: %v", err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("expected killed launcher to exit with error")
	}

	// Keep pubOutR open and undrained while asserting the worker exits promptly.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		err := syscall.Kill(workerPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker pid %d stayed alive with undrained caller stdout (kill err=%v)", workerPID, err)
		}
	}

	if err := syscall.Kill(-childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("expected child process group %d to be killed, got err=%v", childPID, err)
	}
	for _, marker := range []string{"foreground", "descendant"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
			t.Fatalf("%s marker should not exist: %v", marker, err)
		}
	}

	readPipe := func(name string, r *os.File) []byte {
		t.Helper()
		ch := make(chan struct {
			data []byte
			err  error
		}, 1)
		go func() {
			data, err := io.ReadAll(r)
			ch <- struct {
				data []byte
				err  error
			}{data, err}
		}()
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("read %s: %v", name, res.err)
			}
			return res.data
		case <-time.After(time.Second):
			t.Fatalf("%s pipe did not reach EOF after launcher and worker exited", name)
			return nil
		}
	}
	if out := readPipe("stdout", pubOutR); len(out) != 0 {
		t.Fatalf("expected empty stdout after launcher kill, got %d bytes", len(out))
	}
	if errOut := readPipe("stderr", errR); len(errOut) != 0 {
		t.Fatalf("expected empty stderr after launcher kill, got %q", errOut)
	}
}
