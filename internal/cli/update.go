package cli

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Release locations; tests point them at a local server.
var (
	releaseLatest   = "https://api.github.com/repos/ethanhamilthon/zel/releases/latest"
	releaseDownload = "https://github.com/ethanhamilthon/zel/releases/download"
	executablePath  = os.Executable
)

const maxReleaseAsset = 256 << 20

// updateCommand replaces the running binary with the latest GitHub release.
func updateCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: zel update")
	}
	var latest struct {
		Tag string `json:"tag_name"`
	}
	if err := fetchJSON(ctx, releaseLatest, &latest); err != nil {
		return fmt.Errorf("check the latest release: %w", err)
	}
	if latest.Tag == "" {
		return errors.New("check the latest release: no tag name")
	}
	current := strings.TrimPrefix(Version, "v")
	if newerVersion(current, strings.TrimPrefix(latest.Tag, "v")) <= 0 {
		fmt.Fprintf(out, "zel %s is up to date\n", current)
		return nil
	}
	target, err := executablePath()
	if err == nil {
		target, err = filepath.EvalSymlinks(target)
	}
	if err != nil {
		return fmt.Errorf("locate the zel binary: %w", err)
	}
	asset := fmt.Sprintf("zel_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	base := releaseDownload + "/" + latest.Tag + "/"
	fmt.Fprintf(out, "downloading zel %s (%s/%s)\n", strings.TrimPrefix(latest.Tag, "v"), runtime.GOOS, runtime.GOARCH)
	archive, err := fetch(ctx, base+asset)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	sums, err := fetch(ctx, base+"checksums.txt")
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	if err := verifyChecksum(asset, archive, sums); err != nil {
		return err
	}
	binary, err := extractBinary(archive)
	if err != nil {
		return fmt.Errorf("unpack %s: %w", asset, err)
	}
	if err := replaceFile(target, binary); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}
	fmt.Fprintf(out, "updated %s: %s → %s\n", target, current, strings.TrimPrefix(latest.Tag, "v"))
	return nil
}

// newerVersion compares dotted numeric versions: >0 when latest is newer.
// A development build is always older than a release.
func newerVersion(current, latest string) int {
	cur, lat := strings.Split(current, "."), strings.Split(latest, ".")
	for i := range max(len(cur), len(lat)) {
		part := func(parts []string) int {
			if i >= len(parts) {
				return 0
			}
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return -1
			}
			return n
		}
		if d := part(lat) - part(cur); d != 0 {
			return d
		}
	}
	return 0
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseAsset+1))
	if err == nil && len(data) > maxReleaseAsset {
		err = errors.New("file too large")
	}
	return data, err
}

func fetchJSON(ctx context.Context, url string, v any) error {
	data, err := fetch(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func verifyChecksum(asset string, data, sums []byte) error {
	scanner := bufio.NewScanner(strings.NewReader(string(sums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			sum := sha256.Sum256(data)
			if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
				return fmt.Errorf("checksum mismatch for %s", asset)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt does not list %s", asset)
}

func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no zel binary in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "zel" {
			return io.ReadAll(io.LimitReader(tr, maxReleaseAsset))
		}
	}
}

// replaceFile writes next to target and renames over it, so a running zel keeps
// its old binary and a failed write never leaves a broken one.
func replaceFile(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".zel-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
