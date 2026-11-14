package shell

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv("ZEL_TEST_OWNER"); dir != "" {
		cmd := Command(context.Background(), dir, nil, "touch ready; sleep 1; touch survived")
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFreshStateEnvironmentAndBASHENV(t *testing.T) {
	t.Setenv("BASH_ENV", filepath.Join(t.TempDir(), "rc"))
	if err := os.WriteFile(os.Getenv("BASH_ENV"), []byte("echo rc-ran\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Dir: t.TempDir(), Env: func() map[string]string { return map[string]string{"ZEL_SESSION_ID": "s1"} }}
	first := execute(r, context.Background(), "x=1; cd /; echo $ZEL_SESSION_ID ${PAGER}", 5*time.Second, 0)
	second := execute(r, context.Background(), "echo \"[$x]\" $PWD", 5*time.Second, 0)
	if first.Output != "s1 cat\n" || strings.Contains(first.Output, "rc-ran") {
		t.Fatalf("first: %q", first.Output)
	}
	if strings.Contains(second.Output, "[1]") || !strings.HasPrefix(second.Output, "[] ") {
		t.Fatalf("state leaked: %q", second.Output)
	}
}

func TestExitCodeAndTimeoutKillsGroup(t *testing.T) {
	if got := run(t, "echo hi; exit 3", 5*time.Second, 0); got.ExitCode != 3 || got.Output != "hi\n" || !got.Error {
		t.Fatalf("exit: %+v", got)
	}
	start := time.Now()
	got := run(t, "sleep 30 & sleep 30", time.Second, 0)
	if got.ExitCode != 124 || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %+v after %s", got, time.Since(start))
	}
}

func TestCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := &Runner{Dir: t.TempDir()}
	result := execute(r, ctx, "sleep 30", 10*time.Second, 0)
	if result.ExitCode != 130 {
		t.Fatalf("cancel: %+v", result)
	}
}

func TestBackgroundDescendantsDoNotSurviveCall(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	result := execute(r, context.Background(), "(sleep 0.5; touch marker) >/dev/null 2>&1 &", 5*time.Second, 0)
	if result.ExitCode != 0 {
		t.Fatalf("call: %+v", result)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background descendant survived the call")
	}
}

func TestPipeHoldingDescendantIsKilled(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	start := time.Now()
	result := execute(r, context.Background(), "(sleep 1; touch marker) &", 5*time.Second, 0)
	if result.ExitCode != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("call: %+v after %s", result, time.Since(start))
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pipe-holding descendant survived the call")
	}
}

func TestBlockOnlyVariablesAreNotInherited(t *testing.T) {
	t.Setenv("ZEL_MODE", "tui")
	t.Setenv("ZEL_SOURCE", "startup")
	const script = `echo "mode=$ZEL_MODE source=$ZEL_SOURCE"`
	dir := t.TempDir()
	got, err := Command(context.Background(), dir, nil, script).Output()
	if err != nil || string(got) != "mode= source=\n" {
		t.Fatalf("inherited: %q %v", got, err)
	}
	got, err = Command(context.Background(), dir, map[string]string{"ZEL_MODE": "print", "ZEL_SOURCE": "new"}, script).Output()
	if err != nil || string(got) != "mode=print source=new\n" {
		t.Fatalf("block env: %q %v", got, err)
	}
}

func TestCommandDiesWithOwner(t *testing.T) {
	dir := t.TempDir()
	owner := exec.Command(os.Args[0], "-test.run=^$")
	owner.Env = append(os.Environ(), "ZEL_TEST_OWNER="+dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			owner.Process.Kill()
			t.Fatal("command did not start")
		}
	}
	owner.Process.Kill()
	owner.Wait()
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("command outlived its killed owner")
	}
}

func TestOutputBufferKeepsHeadAndTail(t *testing.T) {
	b := newOutputBuffer(10)
	for _, chunk := range []string{"0123", "456789", "abcdefghij", "klmnopqrst"} {
		b.Write([]byte(chunk))
	}
	text := b.Text()
	if !strings.HasPrefix(text, "01234") || !strings.HasSuffix(text, "pqrst") ||
		!strings.Contains(text, "20 of 30 bytes omitted (shell_output=10, first 5 + last 5 kept)") {
		t.Fatalf("text = %q", text)
	}
}

func TestOutputBufferWithinLimitAndUnlimitedAreExact(t *testing.T) {
	short := newOutputBuffer(64)
	short.Write([]byte("short output"))
	unlimited := newOutputBuffer(0)
	long := strings.Repeat("x", 5000)
	unlimited.Write([]byte(long))
	if short.Text() != "short output" || unlimited.Text() != long {
		t.Fatal("output within the limit or unlimited output was changed")
	}
}

func TestOutputBufferKeepsMultibyteStraddlingHeadTailSplit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		text  string
	}{
		{4, "a€"},
		{5, "ab€"},
		{6, "ab€"},
		{8192, strings.Repeat("x", 4095) + "€"},
	} {
		whole := newOutputBuffer(tc.limit)
		whole.Write([]byte(tc.text))
		chunked := newOutputBuffer(tc.limit)
		for _, c := range []byte(tc.text) {
			chunked.Write([]byte{c})
		}
		if whole.Text() != tc.text || chunked.Text() != tc.text {
			t.Fatalf("limit %d: whole = %q, chunked = %q, want %q", tc.limit, whole.Text(), chunked.Text(), tc.text)
		}
	}
}

func writeImage(t *testing.T, src image.Image, format string) (string, []byte) {
	t.Helper()
	var out bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 100})
	} else {
		err = png.Encode(&out, src)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source."+format)
	if err = os.WriteFile(path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, out.Bytes()
}

func decodePreview(t *testing.T, img Image) (image.Image, string, []byte) {
	t.Helper()
	prefix, encoded, ok := strings.Cut(img.URL, ";base64,")
	if !ok {
		t.Fatal("missing image data URL")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	preview, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "data:image/"+format || len(data) > maxImageBytes || max(preview.Bounds().Dx(), preview.Bounds().Dy()) > maxImageEdge {
		t.Fatalf("unbounded/mislabeled preview: %s, %d bytes, %v", prefix, len(data), preview.Bounds())
	}
	return preview, format, data
}

func run(t *testing.T, command string, timeout time.Duration, limit int) Result {
	t.Helper()
	r := &Runner{Dir: t.TempDir(), Env: func() map[string]string { return map[string]string{"ZEL_SESSION_ID": "s1"} }}
	return execute(r, context.Background(), command, timeout, limit)
}

func execute(r *Runner, ctx context.Context, command string, timeout time.Duration, limit int) Result {
	result, _ := r.Run(ctx, command, timeout, limit, nil)
	return result
}

func TestOutputBudgetFourKiBAndLargeWrites(t *testing.T) {
	for _, limit := range []int{1, 5, 4096} {
		b := NewOutputBuffer(limit)
		input := strings.Repeat("A", limit/2) + strings.Repeat("X", 100_000) + strings.Repeat("Z", limit-limit/2)
		b.Write([]byte(input))
		if !strings.HasPrefix(b.Text(), strings.Repeat("A", limit/2)) || !strings.HasSuffix(b.Text(), strings.Repeat("Z", limit-limit/2)) || strings.Contains(b.Text(), "XXXX") {
			t.Fatalf("wrong head/tail at limit %d", limit)
		}
		if cap(b.tail) > limit*2+8 {
			t.Fatalf("retained oversized backing buffer: %d", cap(b.tail))
		}
	}
}
