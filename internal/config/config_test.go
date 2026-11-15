package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
	if got := LuaLayers(home, sub); len(got) != 0 {
		t.Fatalf("no files, no layers: %v", got)
	}
	os.WriteFile(filepath.Join(home, "init.lua"), nil, 0600)
	os.WriteFile(filepath.Join(project, ".zel", "init.lua"), nil, 0600)
	got := LuaLayers(home, sub)
	want := []Layer{{"global", filepath.Join(home, "init.lua")}, {"project", filepath.Join(project, ".zel", "init.lua")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	// Running from the home directory finds the global file once.
	if got := LuaLayers(home, filepath.Dir(home)); len(got) != 1 || got[0].Name != "global" {
		t.Fatalf("home layers: %v", got)
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
}
