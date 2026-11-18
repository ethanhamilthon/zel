// Package edit applies exact-text edits: blocks of old and
// new text, read from stdin, all applied or none.
package edit

import (
	"fmt"
	"strings"
)

const (
	openMark  = "<<<<<<< old"
	sepMark   = "======="
	closeMark = ">>>>>>> new"
)

// Block replaces Old with New: the one occurrence, or every one when All.
type Block struct {
	Old, New string
	All      bool
}

// Error is a failure with the error code the command reports.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Parse reads the blocks:
//
//	<<<<<<< old
//	old text
//	=======
//	new text
//	>>>>>>> new        (or ">>>>>>> new all")
//
// Markers are whole lines; text outside blocks is not allowed.
func Parse(input string) ([]Block, error) {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var blocks []Block
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		if line == "" {
			continue
		}
		if line != openMark {
			return nil, fail("EINVAL", "line %d: expected %q, got %q", i+1, openMark, line)
		}
		start, sep := i+1, -1
		end, all := -1, false
		for j := start; j < len(lines); j++ {
			l := strings.TrimRight(lines[j], " \t")
			if sep < 0 && l == sepMark {
				sep = j
			} else if sep >= 0 && (l == closeMark || l == closeMark+" all") {
				end, all = j, l != closeMark
				break
			}
		}
		if sep < 0 || end < 0 {
			return nil, fail("EINVAL", "line %d: block is not closed with %q and %q", i+1, sepMark, closeMark)
		}
		b := Block{Old: strings.Join(lines[start:sep], "\n"), New: strings.Join(lines[sep+1:end], "\n"), All: all}
		if b.Old == "" {
			return nil, fail("EINVAL", "line %d: the old text is empty", i+1)
		}
		blocks = append(blocks, b)
		i = end
	}
	if len(blocks) == 0 {
		return nil, fail("EINVAL", "no edit blocks on stdin")
	}
	return blocks, nil
}

// Apply runs the blocks in order on content and returns the new text and the
// number of replacements. A block whose old text is missing (ENOMATCH) or found
// more than once without All (EAMBIG) fails the whole edit.
func Apply(content string, blocks []Block, all bool) (string, int, error) {
	total := 0
	for i, b := range blocks {
		n := strings.Count(content, b.Old)
		switch {
		case n == 0:
			return "", 0, fail("ENOMATCH", "block %d: old text not found", i+1)
		case n > 1 && !b.All && !all:
			return "", 0, fail("EAMBIG", "block %d: old text found %d times; make it unique or end the block with \">>>>>>> new all\"", i+1, n)
		}
		if b.All || all {
			content = strings.ReplaceAll(content, b.Old, b.New)
			total += n
		} else {
			content = strings.Replace(content, b.Old, b.New, 1)
			total++
		}
	}
	return content, total, nil
}
