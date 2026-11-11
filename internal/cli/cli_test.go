package cli

import (
	"flag"
	"reflect"
	"testing"
)

func TestIsHelper(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"version"}, true},
		{[]string{"help"}, true},
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"tasks"}, true},
		{[]string{"view"}, true},
		{[]string{"events"}, true},
		{[]string{"write"}, false},
		{[]string{"patch"}, false},
		{[]string{"json"}, false},
		{[]string{"unknown"}, false},
	}
	for _, tc := range cases {
		if got := IsHelper(tc.args); got != tc.want {
			t.Errorf("IsHelper(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseAll(t *testing.T) {
	var s string
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.StringVar(&s, "s", "", "session")
	positional, err := parseAll(fs, []string{"run", "-s", "123", "echo", "hi"})
	if err != nil {
		t.Fatalf("parseAll error: %v", err)
	}
	if s != "123" {
		t.Errorf("flag s = %q, want 123", s)
	}
	want := []string{"run", "echo", "hi"}
	if !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %v, want %v", positional, want)
	}
}
