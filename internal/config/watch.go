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

func filesStamp(paths []string) [sha256.Size]byte {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	h := sha256.New()
	for _, p := range sorted {
		if p == "" {
			continue
		}
		h.Write([]byte(p))
		data, err := os.ReadFile(p)
		if err != nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		h.Write(data)
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// Watch polls the files returned by files() until ctx ends and calls onChange
// once after their content changed (or a file appeared or disappeared) and then
// stayed unchanged for watchSettle.
func Watch(ctx context.Context, files func() []string, onChange func()) {
	seen := filesStamp(files())
	fired := seen
	var changed time.Time
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st := filesStamp(files()); st != seen {
			seen, changed = st, time.Now()
		}
		if time.Since(changed) < watchSettle {
			continue
		}
		if seen != fired {
			fired = seen
			onChange()
		}
	}
}
