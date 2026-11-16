package shell

import (
	"fmt"
	"strings"
)

// OutputBuffer bounds retained output to equal-sized head and tail sections.
// The truncation marker is additional to the configured byte budget.
type OutputBuffer struct {
	limit      int
	head, tail []byte
	size       int
}

func NewOutputBuffer(limit int) *OutputBuffer { return &OutputBuffer{limit: limit} }

func (b *OutputBuffer) Write(p []byte) (int, error) {
	total := len(p)
	b.size += total
	if b.limit <= 0 {
		b.head = append(b.head, p...)
		return total, nil
	}
	half := b.limit / 2
	if room := half - len(b.head); room > 0 {
		n := min(len(p), room)
		b.head, p = append(b.head, p[:n]...), p[n:]
	}
	keep := b.limit - half
	if len(p) >= keep {
		b.tail = append(b.tail[:0], p[len(p)-keep:]...)
	} else {
		if drop := len(b.tail) + len(p) - keep; drop > 0 {
			copy(b.tail, b.tail[drop:])
			b.tail = b.tail[:len(b.tail)-drop]
		}
		b.tail = append(b.tail, p...)
	}
	return total, nil
}

func (b *OutputBuffer) Text() string {
	if b.limit <= 0 || b.size <= b.limit {
		return strings.ToValidUTF8(string(b.head)+string(b.tail), "")
	}
	head, tail := strings.ToValidUTF8(string(b.head), ""), strings.ToValidUTF8(string(b.tail), "")
	return head + fmt.Sprintf("\n[output truncated: %d of %d bytes omitted (shell_output=%d, first %d + last %d kept); "+
		"filter with rg, grep, head or tail]\n",
		b.size-len(b.head)-len(b.tail), b.size, b.limit, len(b.head), len(b.tail)) + tail
}
