package config

import (
	"context"
	"crypto/sha256"
	"os"
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

// Watch polls the file until ctx ends and calls fn once after its content
// changed (or it appeared or disappeared) and then stayed unchanged for
// watchSettle. Touching the file without changing its content does not count.
func Watch(ctx context.Context, path string, fn func()) {
	seen := stamp(path, fileStamp{})
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
		if st := stamp(path, seen); st != seen {
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
