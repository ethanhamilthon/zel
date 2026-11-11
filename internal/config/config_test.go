package config

import (
	"os"
	"path/filepath"
	"strings"
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
			s := DefaultSettings()
			if err := mergeConfig(&s, path); err != nil {
				t.Fatal(err)
			}
			if s.Name != filepath.Base(dir) || AccentColors[s.Color] == "" {
				t.Fatalf("invalid metadata: %+v", s)
			}
			if !strings.HasSuffix(string(before), "---\n\n"+instructions) {
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
