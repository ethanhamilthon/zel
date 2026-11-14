package core

import "sync"

// imageSlots counts zel view images admitted since the last model request.
type imageSlots struct {
	mu sync.Mutex
	n  int
}

const MaxPendingImages = 4

func (s *imageSlots) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n+n > MaxPendingImages {
		return false
	}
	s.n += n
	return true
}

func (s *imageSlots) release(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = max(0, s.n-n)
}

func (s *imageSlots) sent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = 0
}
