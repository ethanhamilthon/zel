package core

import (
	"bytes"
	"encoding/json"
)

// Raw history items are immutable after append. Cache their projections by
// slice identity; replaced/truncated histories invalidate only affected slots.
type historyProjection struct {
	raw           json.RawMessage
	sent, user    bool
	tokens        int64
	records       []Record
	compactRaw    json.RawMessage
	compactTokens int64
	compactZone   int
	compactKept   bool
}

func (s *session) firstUserLine() string {
	s.cacheMu.Lock()
	first := s.firstLine
	s.cacheMu.Unlock()
	if first != "" {
		return first
	}
	for i := range s.History {
		p := s.projection(i)
		for _, r := range p.records {
			if r.Kind == "user" {
				first = FirstLine(r.Text)
				if first != "" {
					s.cacheMu.Lock()
					s.firstLine = first
					s.cacheMu.Unlock()
					return first
				}
			}
		}
	}
	return ""
}

func sameItem(a, b json.RawMessage) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// The caller holds histMu. cacheMu allows projections under a read lock too.
func (s *session) projection(i int) historyProjection {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.projections) > len(s.History) {
		clear(s.projections[len(s.History):])
		s.projections = s.projections[:len(s.History)]
	}
	for len(s.projections) < len(s.History) {
		s.projections = append(s.projections, historyProjection{})
	}
	p := &s.projections[i]
	if p.raw == nil || !sameItem(p.raw, s.History[i]) {
		*p = historyProjection{raw: s.History[i]}
		p.sent, p.user = historyItem(p.raw)
		p.tokens = estimateTokens([]json.RawMessage{p.raw})
		p.records = RecordsForItem(p.raw)
	}
	return *p
}

func (s *session) tokensFrom(start int) int64 {
	var total int64
	for i := max(0, start); i < len(s.History); i++ {
		total += s.projection(i).tokens
	}
	return total
}

// compactedProjection caches the two transformed zones independently of
// growing history. Prefix boundaries are monotonic during ordinary turns.
func (s *session) compactedProjection(i int) (json.RawMessage, bool, int64) {
	p := s.projection(i)
	zone := 0
	if i < s.compacted.Drop {
		zone = 2
	} else if i < s.compacted.Prune {
		zone = 1
	}
	if zone == 0 {
		return p.raw, true, p.tokens
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached := &s.projections[i]
	if cached.compactZone != zone {
		cached.compactRaw, cached.compactKept = s.compactedInput(i, p.raw)
		cached.compactTokens = 0
		if cached.compactKept {
			cached.compactTokens = estimateTokens([]json.RawMessage{cached.compactRaw})
		}
		cached.compactZone = zone
	}
	return cached.compactRaw, cached.compactKept, cached.compactTokens
}

// Count bytes without allocating replacements for large encoded image bodies.
func textBytes(item []byte) int {
	size := len(item)
	for {
		start := bytes.Index(item, []byte("data:image/"))
		if start < 0 {
			break
		}
		end := bytes.IndexByte(item[start:], '"')
		if end < 0 {
			end = len(item) - start
		}
		size -= end - len("data:image")
		item = item[start+end:]
	}
	return size
}
