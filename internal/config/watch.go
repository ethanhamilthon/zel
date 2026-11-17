package config

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Polling parameters of Watch; tests make them small.
var (
	watchInterval = 500 * time.Millisecond
	watchSettle   = 300 * time.Millisecond
)

type fileStamp struct {
	exists bool
	sum    [sha256.Size]byte
}

// treeStamp includes the supported snippets, including additions and removals.
func treeStamp(path string) fileStamp {
	h := sha256.New()
	paths := []string{path}
	for _, dir := range []string{"hooks", "prompts", "systems"} {
		want := ".md"
		if dir == "hooks" {
			want = ".yaml"
		}
		entries, _ := os.ReadDir(filepath.Join(filepath.Dir(path), dir))
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && filepath.Ext(e.Name()) == want {
				paths = append(paths, filepath.Join(filepath.Dir(path), dir, e.Name()))
			}
		}
	}
	st := fileStamp{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			st.exists = true
			h.Write([]byte(p))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
	}
	copy(st.sum[:], h.Sum(nil))
	return st
}

// Watch polls the file until ctx ends and calls fn once after its content
// changed (or it appeared or disappeared) and then stayed unchanged for
// watchSettle. Touching the file without changing its content does not count.
func Watch(ctx context.Context, path string, fn func()) {
	seen := treeStamp(path)
	fired := seen // the state fn was last called for
	var changed time.Time
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st := treeStamp(path); st != seen {
			seen, changed = st, time.Now()
		}
		if time.Since(changed) < watchSettle {
			continue
		}
		if seen.exists != fired.exists || seen.sum != fired.sum {
			fired = seen
			fn()
		}
	}
}
