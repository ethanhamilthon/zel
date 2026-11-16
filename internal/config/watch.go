package config

import (
	"context"
	"crypto/sha256"
	"os"
	"slices"
	"time"
)

// Polling parameters of Watch; tests make them small.
var (
	watchInterval = 500 * time.Millisecond
	watchSettle   = 300 * time.Millisecond
)

type fileStamp struct {
	exists bool
	size   int64
	mod    int64 // mtime, ns
	sum    [sha256.Size]byte
}

// stamp describes path; the content is hashed again only when size or mtime
// differ from prev.
func stamp(path string, prev fileStamp) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	st := fileStamp{exists: true, size: fi.Size(), mod: fi.ModTime().UnixNano()}
	if prev.exists && prev.size == st.size && prev.mod == st.mod {
		st.sum = prev.sum
		return st
	}
	if data, err := os.ReadFile(path); err == nil {
		st.sum = sha256.Sum256(data)
	}
	return st
}

// Watch polls files until ctx ends and calls fn once after their content
// changed (or a file appeared or disappeared) and then stayed unchanged for
// watchSettle. Touching a file without changing its content does not count.
func Watch(ctx context.Context, files []string, fn func()) {
	seen := make([]fileStamp, len(files))
	for i, f := range files {
		seen[i] = stamp(f, fileStamp{})
	}
	fired := slices.Clone(seen) // the state fn was last called for
	var changed time.Time
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for i, f := range files {
			if st := stamp(f, seen[i]); st != seen[i] {
				seen[i], changed = st, time.Now()
			}
		}
		if time.Since(changed) < watchSettle {
			continue
		}
		differ := false
		for i := range seen {
			differ = differ || seen[i].exists != fired[i].exists || seen[i].sum != fired[i].sum
		}
		if differ {
			copy(fired, seen)
			fn()
		}
	}
}
