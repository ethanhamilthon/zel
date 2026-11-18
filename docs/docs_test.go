package docs

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The packages keep to the dependency graph of ARCHITECTURE.md.
func TestProductionPackageBoundaries(t *testing.T) {
	allowed := map[string][]string{
		"entities": {}, "config": {}, "store": {}, "edit": {}, "ipc": {},
		"shell": {"entities"}, "provider": {"entities"},
		"defaults": {},
		"luart":    {"config", "shell", "edit"},
		"tui":      {"core"},
		"cli":      {"core", "config"},
	}
	for pkg, deps := range allowed {
		paths, err := filepath.Glob(filepath.Join("..", "internal", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) == 0 {
			t.Fatalf("missing package %s", pkg)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if dependency, internal := strings.CutPrefix(name, "zel/internal/"); internal && !slices.Contains(deps, dependency) {
					t.Errorf("%s imports forbidden package %s", path, dependency)
				}
			}
		}
	}
	if _, err := os.Stat("ARCHITECTURE.md"); err != nil {
		t.Fatal(err)
	}
}

// Every section of lua-api.md has a topic and every topic returns text, so the
// agent can reach the whole reference through zel docs.
func TestExamplesIncludeFiles(t *testing.T) {
	text := exampleText()
	for _, want := range []string{"# --- init.lua", "# --- zel.lua", "# --- connect.json", "# --- lua/web.lua", "loads"} {
		if !strings.Contains(text, want) {
			t.Errorf("examples missing %s", want)
		}
	}
}

func TestEverySectionIsReachable(t *testing.T) {
	src, err := files.ReadFile("lua-api.md")
	if err != nil {
		t.Fatal(err)
	}
	for h := range parse(string(src)) {
		if h == "Examples" {
			continue // examples has its own topic
		}
		if _, ok := sections[h]; !ok {
			t.Errorf("section %q of lua-api.md has no topic", h)
		}
	}
	for _, topic := range Topics() {
		text, ok := Get(topic.Name)
		if !ok || strings.TrimSpace(text) == "" {
			t.Errorf("topic %q is empty", topic.Name)
		}
		if !strings.Contains(Index(), "zel docs "+topic.Name+" ") {
			t.Errorf("index misses %q", topic.Name)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown topic resolved")
	}
	if text, _ := Get("hooks"); !strings.Contains(text, "turn_end") || strings.Contains(text, "#name") {
		t.Errorf("hooks topic is wrong: %q", text)
	}
}
