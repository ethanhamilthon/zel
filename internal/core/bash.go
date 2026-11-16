package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"zel/internal/shell"
)

// bashOutputLimit is the safety cap of one call's output: head and tail stay.
const bashOutputLimit = 1 << 20

// runBash runs one bash call of the agent: the bash_before hooks may block or
// rewrite it, the bash_after hooks may add to its output.
func (s *session) runBash(ctx context.Context, command string) string {
	if s.hasHooks("bash_before") {
		res := s.runHooks(ctx, "bash_before", map[string]any{"command": command})
		if res.blocked {
			return "[blocked by a hook: " + strings.TrimSpace(res.reason) + "]"
		}
		command, _ = res.input["command"].(string)
	}
	timeout := time.Duration(s.Settings().BashTimeout) * time.Second
	end := s.beginActivity(bashLabel(Redact(s.Settings(), command)))
	started := time.Now()
	out, code, timedOut, capture := s.execBash(ctx, command, timeout)
	end()
	if s.hasHooks("bash_after") {
		res := s.runHooks(ctx, "bash_after", map[string]any{"command": command, "out": out, "code": code,
			"ms": time.Since(started).Milliseconds(), "timed_out": timedOut})
		for _, extra := range res.outs {
			out = strings.TrimRight(out, "\n") + "\n" + extra
		}
	}
	return s.cutOutput(out, capture)
}

// execBash runs the script with `bash -c` in the session folder; the whole
// process group dies at the timeout or when ctx ends.
func (s *session) execBash(ctx context.Context, command string, timeout time.Duration) (out string, code int, timedOut bool, capture *bashCapture) {
	if ctx.Err() != nil {
		return "[cancelled]", 130, false, nil
	}
	cmd := shell.Command(context.Background(), s.bashCwd(), s.shellEnv(), command)
	capture = &bashCapture{buffer: shell.NewOutputBuffer(bashOutputLimit), limit: s.Settings().BashOutput, open: s.openOutputFile}
	buffer := capture.buffer
	w := &lockedWriter{w: capture}
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		return err.Error() + "\n[exit 127]", 127, false, nil
	}
	done := make(chan error, 1)
	go func() { done <- shell.Reap(cmd) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	suffix := ""
	select {
	case err = <-done:
		code = exitCode(err)
		if code != 0 {
			suffix = fmt.Sprintf("[exit %d]", code)
		}
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		code, timedOut, suffix = 124, true, fmt.Sprintf("[timeout after %s]", timeout)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		code, suffix = 130, "[cancelled]"
	}
	w.mu.Lock()
	out = buffer.Text()
	capture.lost = max(0, capture.total-len(out))
	w.mu.Unlock()
	if suffix != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += suffix
	}
	return out, code, timedOut, capture
}

// exitCode is the exit status of a finished command; a signal counts as 128+n.
func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// Bounds of the working indicator label: the indicator cuts lines to its width
// and cycles through them, so longer ones are not needed.
const (
	labelLines    = 100
	labelLineRune = 200
)

// bashLabel is what the working indicator shows for a call: the command as is,
// its lines without empty ones and # comments, one per row.
func bashLabel(command string) string {
	var lines []string
	for _, l := range strings.Split(command, "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if r := []rune(l); len(r) > labelLineRune {
			l = string(r[:labelLineRune-1]) + "…"
		}
		if lines = append(lines, l); len(lines) == labelLines {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// bashFileCap is the most bytes of one call's output written to its file.
const bashFileCap = 64 << 20

// bashCapture receives the output of one call: the bounded in-memory buffer
// and, when the output may be cut, a copy as it streams. The copy stays in
// memory until it passes the limit, then goes to a file, so a call whose
// output fits writes none.
type bashCapture struct {
	buffer *shell.OutputBuffer
	limit  int
	open   func() (*os.File, error)
	total  int // bytes streamed
	lost   int // bytes streamed that the buffer no longer holds
	pre    []byte
	file   *os.File
	err    error
	capped bool
	size   int
}

func (c *bashCapture) Write(p []byte) (int, error) {
	_, _ = c.buffer.Write(p)
	c.total += len(p)
	if c.limit <= 0 || c.err != nil {
		return len(p), nil
	}
	if c.file == nil {
		if c.pre = append(c.pre, p...); len(c.pre) > c.limit {
			c.spill()
		}
		return len(p), nil
	}
	c.put(p)
	return len(p), nil
}

// spill opens the file and moves the held output into it.
func (c *bashCapture) spill() {
	if c.file, c.err = c.open(); c.err != nil {
		return
	}
	pre := c.pre
	c.pre = nil
	c.put(pre)
}

func (c *bashCapture) put(p []byte) {
	if c.capped {
		return
	}
	if room := bashFileCap - c.size; len(p) > room {
		p, c.capped = p[:room], true
	}
	c.size += len(p)
	_, _ = c.file.Write(p)
	if c.capped {
		_, _ = c.file.WriteString("\n[output file stops here: 64 MiB limit]\n")
	}
}

// finish closes the file and returns its path; with need set it writes the
// file even if the output never passed the limit (the endings pushed it over).
func (c *bashCapture) finish(need bool) string {
	if c.file == nil && need && c.err == nil {
		c.spill()
	}
	if c.file == nil {
		return ""
	}
	path := c.file.Name()
	_ = c.file.Close()
	return path
}

// openOutputFile creates the next outputs/b<N>.txt of the session folder.
func (s *session) openOutputFile() (*os.File, error) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	dir := filepath.Join(s.sessionDir(), "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	last := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "b"), ".txt")); err == nil && strings.HasPrefix(name, "b") {
			last = max(last, n)
		}
	}
	return os.OpenFile(filepath.Join(dir, "b"+strconv.Itoa(last+1)+".txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

// cutOutput applies bash_output to the whole text the agent gets.
func (s *session) cutOutput(text string, c *bashCapture) string {
	if c == nil || c.limit <= 0 {
		return text
	}
	total := len(text) + c.lost
	if total <= c.limit {
		c.finish(false)
		return text
	}
	path := c.finish(true)
	if path == "" {
		path = "(not saved: " + errString(c.err) + ")"
	}
	return cutText(text, total, c.limit, path)
}

func errString(err error) string {
	if err == nil {
		return "no file"
	}
	return err.Error()
}

// cutText keeps the first and last half of limit bytes of text, with the
// marker between them. total is the size of the full output. A cut never
// splits a UTF-8 character and moves to a line break within ~10% of the half.
func cutText(text string, total, limit int, path string) string {
	half := limit / 2
	slack := half / 10
	headEnd := min(half, len(text))
	for headEnd > 0 && headEnd < len(text) && !utf8.RuneStart(text[headEnd]) {
		headEnd--
	}
	if i := strings.LastIndexByte(text[max(0, headEnd-slack):headEnd], '\n'); i >= 0 {
		headEnd = max(0, headEnd-slack) + i + 1
	}
	tailStart := max(headEnd, len(text)-(limit-half))
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	if i := strings.IndexByte(text[tailStart:min(len(text), tailStart+slack)], '\n'); i >= 0 {
		tailStart += i + 1
	}
	head, tail := text[:headEnd], text[tailStart:]
	gap := "\n\n"
	if strings.HasSuffix(head, "\n") {
		gap = "\n"
	}
	omitted := total - len(head) - len(tail)
	return head + gap +
		fmt.Sprintf("======== [output cut: %d of %d bytes omitted]\n", omitted, total) +
		"======== full output: " + path + "\n" +
		"======== read it with sed -n '100,200p', rg, head or tail\n\n" + tail
}

// bashCwd is the folder bash calls and new tasks start in: the one set by
// zel cd, else the folder the session started in.
func (s *session) bashCwd() string {
	s.bashMu.Lock()
	defer s.bashMu.Unlock()
	return cmp.Or(s.bashDir, s.dir)
}

// bashCwdSet is the folder set by zel cd, empty when none.
func (s *session) bashCwdSet() string {
	s.bashMu.Lock()
	defer s.bashMu.Unlock()
	return s.bashDir
}

// changeDir is zel cd: dir (relative to base, ~ allowed) becomes the folder of
// the next bash calls; empty goes back to the start folder. It returns the new
// absolute path. Nothing changes when dir is missing or not a folder.
func (s *session) changeDir(dir, base string) (string, error) {
	path := s.dir
	if dir != "" {
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fail(CodeInvalid, "EINVAL: %v", err)
			}
			dir = filepath.Join(home, dir[1:])
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cmp.Or(base, s.bashCwd()), dir)
		}
		path = filepath.Clean(dir)
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fail(CodeNotFound, "ENOENT: %s: no such folder", path)
	case err != nil:
		return "", fail(CodeInvalid, "EINVAL: %v", err)
	case !info.IsDir():
		return "", fail(CodeInvalid, "EINVAL: %s: not a folder", path)
	}
	s.bashMu.Lock()
	if path == s.dir {
		s.bashDir = ""
	} else {
		s.bashDir = path
	}
	s.bashMu.Unlock()
	if s.HasSavedSession() {
		if err := s.Save(); err != nil {
			return path, nil
		}
	}
	return path, nil
}
