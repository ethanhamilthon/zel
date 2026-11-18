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
	"runtime"
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
(touch bg.tmp; mv bg.tmp bg; sleep 5; touch descendant) &
printf '%s %s\n' "$PPID" "$$" > pids.tmp
mv pids.tmp pids
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
	bgPath := filepath.Join(dir, "bg")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				workerPID, _ = strconv.Atoi(fields[0])
				childPID, _ = strconv.Atoi(fields[1])
				if _, bgErr := os.Stat(bgPath); bgErr == nil && workerPID > 0 && childPID > 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for worker/child pids and bg marker: %v", err)
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
	waitProcessGone(t, workerPID, 2*time.Second, "worker stayed alive with undrained caller stdout")
	waitProcessGroupGone(t, childPID, 2*time.Second)
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

type procInfo struct {
	pid  int
	ppid int
	pgid int
	stat string
	comm string
}

func (p procInfo) isZombie() bool {
	return strings.HasPrefix(p.stat, "Z") || p.stat == "X"
}

func waitProcessGone(t *testing.T, pid int, timeout time.Duration, failureMsg string) {
	t.Helper()
	var lastErr error
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		lastErr = syscall.Kill(pid, 0)
		if errors.Is(lastErr, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	procs, listErr := listProcesses(func(p procInfo) bool { return p.pid == pid })
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return
	} else {
		lastErr = err
	}
	if listErr != nil || len(procs) == 0 {
		t.Fatalf("%s: pid %d (kill err=%v, list err=%v)", failureMsg, pid, lastErr, listErr)
	}
	if !procs[0].isZombie() {
		t.Fatalf("%s: pid %d still live (kill err=%v): %+v", failureMsg, pid, lastErr, procs[0])
	}
	t.Logf("pid %d exited to unreaped zombie after %v (kill err=%v): %+v", pid, timeout, lastErr, procs[0])
}

// waitProcessGroupGone waits for all processes in pgid to exit and be reaped.
// After SIGKILL to a process group and reaping only the group leader, descendants
// reparented to init/subreaper can briefly stay in kernel exit teardown or as
// unreaped zombies: Linux Kill(-pgid, 0) returns nil for both, while macOS
// returns nil during exit teardown and EPERM for zombies.
func waitProcessGroupGone(t *testing.T, pgid int, timeout time.Duration) {
	t.Helper()
	var lastErr error
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		lastErr = syscall.Kill(-pgid, 0)
		if errors.Is(lastErr, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	procs, listErr := listProcesses(func(p procInfo) bool { return p.pgid == pgid })
	if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
		return
	} else {
		lastErr = err
	}
	if listErr != nil {
		t.Fatalf("expected child process group %d to be killed after %v (kill err=%v, list err=%v)", pgid, timeout, lastErr, listErr)
	}
	var live, zombies []procInfo
	for _, p := range procs {
		if p.isZombie() {
			zombies = append(zombies, p)
		} else {
			live = append(live, p)
		}
	}
	if len(live) > 0 {
		t.Fatalf("expected child process group %d to be killed, live processes survived after %v (kill err=%v): %+v", pgid, timeout, lastErr, live)
	}
	if len(zombies) == 0 {
		t.Fatalf("expected child process group %d to be killed after %v, got kill err=%v with no listed members", pgid, timeout, lastErr)
	}
	t.Logf("child process group %d has only unreaped zombie(s) after %v (kill err=%v): %+v", pgid, timeout, lastErr, zombies)
}

func listProcesses(match func(procInfo) bool) ([]procInfo, error) {
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, err
		}
		var out []procInfo
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			if p, ok := parseProcStat(string(data)); ok && match(p) {
				out = append(out, p)
			}
		}
		return out, nil
	}
	raw, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,stat=,comm=").Output()
	if err != nil {
		return nil, err
	}
	var out []procInfo
	for _, line := range strings.Split(string(raw), "\n") {
		if p, ok := parsePSLine(line); ok && match(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

func parseProcStat(line string) (procInfo, bool) {
	var p procInfo
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close <= open {
		return p, false
	}
	var err error
	if p.pid, err = strconv.Atoi(strings.TrimSpace(line[:open])); err != nil {
		return p, false
	}
	p.comm = line[open+1 : close]
	fields := strings.Fields(line[close+1:])
	if len(fields) < 3 {
		return p, false
	}
	p.stat = fields[0]
	p.ppid, _ = strconv.Atoi(fields[1])
	if p.pgid, err = strconv.Atoi(fields[2]); err != nil {
		return p, false
	}
	return p, true
}

func parsePSLine(line string) (procInfo, bool) {
	var p procInfo
	s := strings.TrimSpace(line)
	if s == "" {
		return p, false
	}
	take := func() (string, bool) {
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			f := s
			s = ""
			return f, f != ""
		}
		f := s[:i]
		s = strings.TrimLeft(s[i:], " \t")
		return f, f != ""
	}
	f0, ok0 := take()
	f1, ok1 := take()
	f2, ok2 := take()
	f3, ok3 := take()
	if !ok0 || !ok1 || !ok2 || !ok3 {
		return p, false
	}
	var err error
	if p.pid, err = strconv.Atoi(f0); err != nil {
		return p, false
	}
	p.ppid, _ = strconv.Atoi(f1)
	if p.pgid, err = strconv.Atoi(f2); err != nil {
		return p, false
	}
	p.stat = f3
	p.comm = s
	return p, true
}

func TestProcessStateClassification(t *testing.T) {
	if p, ok := parseProcStat("123 (my (sleep)) Z 1 120 120 0 -1"); !ok || p.pid != 123 || p.ppid != 1 || p.pgid != 120 || p.comm != "my (sleep)" || !p.isZombie() {
		t.Fatalf("parseProcStat zombie: ok=%v p=%+v", ok, p)
	}
	if p, ok := parseProcStat("124 (sleep) S 1 120 120 0 -1"); !ok || p.isZombie() {
		t.Fatalf("parseProcStat live: ok=%v p=%+v", ok, p)
	}
	if p, ok := parsePSLine("  43353 43352 43352 Z+   <defunct>"); !ok || p.pid != 43353 || p.ppid != 43352 || p.pgid != 43352 || !p.isZombie() || p.comm != "<defunct>" {
		t.Fatalf("parsePSLine zombie: ok=%v p=%+v", ok, p)
	}
	if p, ok := parsePSLine("  43352 43231 43352 S    /bin/sleep"); !ok || p.isZombie() || p.comm != "/bin/sleep" {
		t.Fatalf("parsePSLine live: ok=%v p=%+v", ok, p)
	}

	cmd := exec.Command("/bin/sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	liveProcs, err := listProcesses(func(p procInfo) bool { return p.pid == pid })
	if err != nil || len(liveProcs) != 1 || liveProcs[0].isZombie() {
		t.Fatalf("expected live sleep process %d, got err=%v procs=%+v", pid, err, liveProcs)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var sawZombie bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		zProcs, err := listProcesses(func(p procInfo) bool { return p.pid == pid })
		if err != nil {
			t.Fatal(err)
		}
		if len(zProcs) == 1 && zProcs[0].isZombie() {
			sawZombie = true
			break
		}
	}
	if !sawZombie {
		t.Fatalf("expected un-waited killed child %d to appear as zombie", pid)
	}
	_ = cmd.Wait()
	afterReap, err := listProcesses(func(p procInfo) bool { return p.pid == pid })
	if err != nil || len(afterReap) != 0 {
		t.Fatalf("expected reaped child %d to disappear, got err=%v procs=%+v", pid, err, afterReap)
	}
}
