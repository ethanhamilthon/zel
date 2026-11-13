package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitProject(t *testing.T) {
	for _, withAgents := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "project: #quoted")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if withAgents {
			if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Rules\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		created, err := InitProject(dir)
		if err != nil || !created {
			t.Fatalf("created=%t err=%v", created, err)
		}
		path := filepath.Join(dir, ".zel", "init.lua")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(before), `zel.instructions{file = "AGENTS.md"}`); got != withAgents {
			t.Fatalf("AGENTS.md include = %t, want %t:\n%s", got, withAgents, before)
		}
		if _, err := os.Stat(filepath.Join(dir, "ZEL.md")); err == nil {
			t.Fatal("ZEL.md must not be created")
		}
		if err := os.WriteFile(path, []byte("-- mine\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if created, err := InitProject(dir); err != nil || created {
			t.Fatalf("second init: created=%t err=%v", created, err)
		}
		if after, _ := os.ReadFile(path); string(after) != "-- mine\n" {
			t.Fatalf("existing file changed: %q", after)
		}
	}
}

func TestLuaLayers(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".zel")
	project := filepath.Join(root, "proj")
	sub := filepath.Join(project, "a", "b")
	for _, dir := range []string{home, filepath.Join(project, ".zel"), sub} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got := luaLayers(home, sub, ""); len(got) != 0 {
		t.Fatalf("no files, no layers: %v", got)
	}
	os.WriteFile(filepath.Join(home, "init.lua"), nil, 0600)
	os.WriteFile(filepath.Join(project, ".zel", "init.lua"), nil, 0600)
	flagged := filepath.Join(root, "extra.lua")
	got := luaLayers(home, sub, flagged)
	want := []Layer{{"global", filepath.Join(home, "init.lua")}, {"project", filepath.Join(project, ".zel", "init.lua")}, {"config", flagged}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	// Running from the home directory finds the global file once.
	if got := luaLayers(home, filepath.Dir(home), ""); len(got) != 1 || got[0].Name != "global" {
		t.Fatalf("home layers: %v", got)
	}
}

func TestLoadRejectsNonLuaConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEL_CONFIG", filepath.Join(dir, "x.md"))
	if _, err := Load(dir, dir, nil); err == nil || !strings.Contains(err.Error(), ".lua") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("ZEL_CONFIG", filepath.Join(dir, "missing.lua"))
	if _, err := Load(dir, dir, nil); err == nil {
		t.Fatal("a missing --config file is an error")
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
	t.Setenv("ZEL_CONFIG", "~/.config/zel.lua")
	got = FlagConfig(cwd)
	if got != filepath.Join(home, ".config", "zel.lua") {
		t.Fatalf("FlagConfig with a home-relative override = %q", got)
	}
}
