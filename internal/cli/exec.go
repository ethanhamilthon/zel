package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"zel/internal/config"
)

func execCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	const usage = "exec [--timeout S] [--cwd DIR] [--env K=V]... -- CMD ARGS..."
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	seconds := fs.Float64("timeout", 0, "")
	dir := fs.String("cwd", "", "")
	env := map[string]string{}
	fs.Func("env", "", func(s string) error {
		key, value, ok := strings.Cut(s, "=")
		if !ok || key == "" || strings.ContainsRune(s, 0) {
			return errors.New("expected K=V")
		}
		env[key] = value
		return nil
	})
	sep := slices.Index(args, "--")
	if sep < 0 || sep+1 == len(args) || fs.Parse(args[:sep]) != nil || fs.NArg() != 0 {
		return usageError("exec", usage)
	}
	nanos := *seconds * float64(time.Second)
	if math.IsNaN(nanos) || nanos < 0 || nanos >= float64(math.MaxInt64) {
		return usageError("exec", usage)
	}
	if *dir != "" {
		path, err := config.ResolvePath(*dir)
		if err != nil {
			return err
		}
		*dir = path
	}
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, max(1, time.Duration(nanos)))
		defer cancel()
	}
	res, err := runExec(ctx, args[sep+1:], *dir, env, in)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(res)
}

type execResult struct {
	OK       bool   `json:"ok"`
	Code     int    `json:"code"`
	Out      string `json:"out"`
	TimedOut bool   `json:"timed_out"`
	Ms       int64  `json:"ms"`
}

func runExec(ctx context.Context, args []string, dir string, env map[string]string, in io.Reader) (execResult, error) {
	started := time.Now()
	exe := args[0]
	if path, overridden := env["PATH"]; overridden && !strings.ContainsRune(exe, filepath.Separator) {
		// os/exec normally looks up the executable in the parent's PATH.
		// Interpret the command's PATH relative to its working directory.
		p, err := findExec(exe, dir, path)
		if err != nil {
			return execResult{Code: 127, Out: err.Error(), Ms: time.Since(started).Milliseconds()}, nil
		}
		exe = p
	}
	cmd := exec.CommandContext(ctx, exe, args[1:]...)
	cmd.Args[0] = args[0]
	cmd.Dir = dir
	cmd.Env = cmd.Environ()
	for key, val := range env {
		cmd.Env = append(cmd.Env, key+"="+val)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var killed atomic.Bool
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		killed.Store(err == nil)
		return err
	}

	// Hand the child real descriptors: stdout and stderr share one pipe.
	// We own the read side, so a background process holding a descriptor
	// cannot keep Wait blocked or turn a successful exit into an error.
	r, w, err := os.Pipe()
	if err != nil {
		return execResult{}, err
	}
	defer r.Close()
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	var input *os.File
	if f, ok := in.(*os.File); ok {
		cmd.Stdin = f
	} else if in != nil {
		child, parent, err := os.Pipe()
		if err != nil {
			return execResult{}, err
		}
		defer child.Close()
		defer parent.Close()
		cmd.Stdin, input = child, parent
	}

	if err := cmd.Start(); err != nil {
		res := execResult{Code: 126, Out: err.Error(), Ms: time.Since(started).Milliseconds()}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			res.Code, res.TimedOut = 124, true
		case errors.Is(err, context.Canceled):
			res.Code = 130
		case errors.Is(err, exec.ErrNotFound), os.IsNotExist(err) && cmd.Dir == "":
			res.Code = 127
		}
		return res, nil
	}
	_ = w.Close()
	if input != nil {
		_ = cmd.Stdin.(*os.File).Close()
		go func() { defer input.Close(); _, _ = io.Copy(input, in) }()
	}
	drained := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		drained <- buf.String()
	}()
	waitErr := cmd.Wait()
	timer := time.AfterFunc(100*time.Millisecond, func() { _ = r.Close() })
	defer timer.Stop()
	res := execResult{Out: <-drained, Ms: time.Since(started).Milliseconds(), OK: waitErr == nil}
	if waitErr != nil {
		res.Code = 1
		if state := cmd.ProcessState; state != nil {
			res.Code = state.ExitCode()
			if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				res.Code = 128 + int(status.Signal())
				if status.Signal() == syscall.SIGKILL && killed.Load() {
					res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
					res.Code = 130
					if res.TimedOut {
						res.Code = 124
					}
				}
			}
		}
	}
	return res, nil
}

func findExec(name, dir, path string) (string, error) {
	for _, base := range strings.Split(path, string(os.PathListSeparator)) {
		if !filepath.IsAbs(base) {
			base = filepath.Join(dir, base)
		}
		p, err := filepath.Abs(filepath.Join(base, name))
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: executable file not found in PATH", name)
}
