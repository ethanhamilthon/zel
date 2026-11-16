package edit

import (
	"errors"
	"testing"
)

func TestEdit(t *testing.T) {
	blocks, err := Parse("<<<<<<< old\nfunc a() {\n=======\nfunc a(x int) {\n>>>>>>> new\n\n<<<<<<< old\nnil\n=======\nerr\n>>>>>>> new all\n")
	if err != nil || len(blocks) != 2 || !blocks[1].All {
		t.Fatalf("parse = %+v, %v", blocks, err)
	}
	got, n, err := Apply("func a() {\n\treturn nil, nil\n}", blocks, false)
	if err != nil || n != 3 || got != "func a(x int) {\n\treturn err, err\n}" {
		t.Fatalf("apply = %q, %d, %v", got, n, err)
	}
	var e *Error
	if _, _, err := Apply("x x", []Block{{Old: "x", New: "y"}}, false); !errors.As(err, &e) || e.Code != "EAMBIG" {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, _, err := Apply("x", []Block{{Old: "x", New: "y"}, {Old: "z", New: "w"}}, false); !errors.As(err, &e) || e.Code != "ENOMATCH" {
		t.Fatalf("no match: %v", err)
	}
	if _, err := Parse("<<<<<<< old\nx\n"); err == nil {
		t.Fatal("unclosed block parsed")
	}
	// A separator line inside the new text belongs to it.
	blocks, _ = Parse("<<<<<<< old\na\n=======\nb\n=======\n>>>>>>> new")
	if blocks[0].New != "b\n=======" {
		t.Fatalf("new = %q", blocks[0].New)
	}
}
