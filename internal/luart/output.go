package luart

import (
	"strings"
	"sync"
)

const cutMarker = "\n[output truncated]\n"

type output struct {
	mu         sync.Mutex
	head, tail []byte
	size, next int
	last       byte
}

func newOutput() *output { return &output{} }

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.add(string(p))
	return len(p), nil
}

func (o *output) WriteString(s string) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.add(s)
	return len(s), nil
}

func (o *output) add(s string) {
	if s == "" {
		return
	}
	o.last = s[len(s)-1]
	o.size += len(s)
	const half = maxOutput / 2
	if n := min(half-len(o.head), len(s)); n > 0 {
		o.head, s = append(o.head, s[:n]...), s[n:]
	}
	if len(s) >= half {
		o.tail = append(o.tail[:0], s[len(s)-half:]...)
		o.next = 0
		return
	}
	if n := min(half-len(o.tail), len(s)); n > 0 {
		o.tail, s = append(o.tail, s[:n]...), s[n:]
	}
	if s == "" {
		return
	}
	n := copy(o.tail[o.next:], s)
	copy(o.tail, s[n:])
	o.next = (o.next + len(s)) % half
}

func (o *output) finish(suffix string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if suffix != "" {
		if o.size > 0 && o.last != '\n' {
			o.add("\n")
		}
		o.add(suffix)
	}
	if o.size <= maxOutput {
		return string(o.head) + string(o.tail)
	}
	tail := string(o.tail[o.next:]) + string(o.tail[:o.next])
	return strings.ToValidUTF8(string(o.head), "") + cutMarker + strings.ToValidUTF8(tail[len(cutMarker):], "")
}
