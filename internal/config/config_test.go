package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitProject(t *testing.T) {
	for _, instructions := range []string{"", "# Rules\n\nUse English.\n---\nKeep this verbatim.\n"} {
		t.Run(instructions, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "project: #quoted")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if instructions != "" {
				if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(instructions), 0600); err != nil {
					t.Fatal(err)
				}
			}
			created, err := InitProject(dir)
			if err != nil || !created {
				t.Fatalf("created=%t err=%v", created, err)
			}
			path := filepath.Join(dir, "ZEL.md")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != instructions {
				t.Fatalf("instructions not preserved: %q", before)
			}
			if created, err := InitProject(dir); err != nil || created {
				t.Fatalf("second init: created=%t err=%v", created, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("existing file changed: %v", err)
			}
		})
	}
}

func TestInitProjectCleansUpOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "AGENTS.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitProject(dir); err == nil {
		t.Fatal("expected instructions read error")
	}
	if _, err := os.Stat(filepath.Join(dir, "ZEL.md")); !os.IsNotExist(err) {
		t.Fatalf("partial file retained: %v", err)
	}
}

func TestPortableHomePaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "actual-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	for _, test := range []struct {
		input, want string
	}{
		{"~", home},
		{"~/projects/zel", filepath.Join(home, "projects", "zel")},
		{"~someone/project", "~someone/project"},
		{"relative/path", "relative/path"},
	} {
		got, err := ResolvePath(test.input)
		if err != nil || got != test.want {
			t.Errorf("ResolvePath(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}

	for _, test := range []struct {
		input, want string
	}{
		{home, "~"},
		{filepath.Join(home, "projects", "zel"), "~/projects/zel"},
		{filepath.Join(home+"-other", "project"), filepath.Join(home+"-other", "project")},
		{"relative/path", "relative/path"},
	} {
		if got := DisplayPath(test.input); got != test.want {
			t.Errorf("DisplayPath(%q) = %q; want %q", test.input, got, test.want)
		}
	}

	cwd := filepath.Join(t.TempDir(), "project")
	t.Setenv("ZEL_HOME", "~/.zel-state")
	got, err := ZelHome(cwd)
	if err != nil || got != filepath.Join(home, ".zel-state") {
		t.Fatalf("ZelHome with a home-relative override = %q, %v", got, err)
	}
	t.Setenv("ZEL_CONFIG", "~/.config/zel.md")
	got = FlagConfig(cwd)
	if got != filepath.Join(home, ".config", "zel.md") {
		t.Fatalf("FlagConfig with a home-relative override = %q", got)
	}
}
