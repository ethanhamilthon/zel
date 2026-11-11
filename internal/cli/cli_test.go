package cli

import (
	"context"
	"flag"
	"reflect"
	"strings"
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

func TestJSONWithoutProvidersReturnsSetupRequired(t *testing.T) {
	t.Setenv("ZEL_HOME", t.TempDir())
	t.Setenv("ZEL_CONFIG", "")
	var out, diagnostics strings.Builder
	err := Run(context.Background(), []string{"json"}, strings.NewReader("{\"id\":\"1\",\"type\":\"input\",\"text\":\"hello\"}\n"), &out, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"setup_required"`) || strings.Contains(out.String(), "zel setup") {
		t.Fatalf("output: %s", out.String())
	}
}
