package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// fakeRun expands every expression to "X:<script>", "fail" fails.
func fakeRun(env map[string]string) Expander {
	return func(_ context.Context, script string) (string, error) {
		if script == "fail" {
			return "", errors.New("boom")
		}
		if v, ok := strings.CutPrefix(script, "env "); ok {
			return env[v] + "\n", nil
		}
		return "X:" + script + "\n", nil
	}
}

func TestLayers(t *testing.T) {
	root := t.TempDir()
	home, cwd := filepath.Join(root, "home"), filepath.Join(root, "proj", "sub")
	os.MkdirAll(home, 0700)
	os.MkdirAll(cwd, 0700)
	writeFile(t, filepath.Join(root, "proj", "zel.toml"), "") // parent dir: not used
	got := Layers(home, cwd, []string{"extra.toml", filepath.Join(cwd, "extra.toml"), "/abs/x.toml"})
	want := []Layer{{"global", filepath.Join(home, "zel.toml")}, {"config", filepath.Join(cwd, "extra.toml")}, {"config", "/abs/x.toml"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	writeFile(t, filepath.Join(cwd, "zel.toml"), "")
	if got := Layers(home, cwd, nil); len(got) != 2 || got[1] != (Layer{"local", filepath.Join(cwd, "zel.toml")}) {
		t.Fatalf("local layer: %v", got)
	}
	writeFile(t, filepath.Join(home, "zel.toml"), "")
	if got := Layers(home, home, nil); len(got) != 1 {
		t.Fatalf("cwd = home must give the global file once: %v", got)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	global, local, bad, syntax := filepath.Join(dir, "g.toml"), filepath.Join(dir, "l.toml"), filepath.Join(dir, "bad.toml"), filepath.Join(dir, "syn.toml")
	writeFile(t, global, `
provider = "openai"
model = "m1"
instructions = ["global ${{ hi }}", { text = "headless only", main = false, headless = true }, ""]
[settings]
compaction = 1000
[colors]
accent = "${{ fail }}#123456"
user = "#111111"
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key = "${{ env KEY }}"
[env]
KEY = "k-${{ one }}"
[hooks]
bash_before = ["echo a", { run = "echo b", match = "^git", timeout = 1.5, async = true, headless = true }]
idle = "echo idle"
[completions]
fix = "Fix ${{ x }}"
`)
	writeFile(t, local, `
model = "m2"
instructions = [{ text = "local" }]
[settings]
bash_timeout = 5
slash_menu = false
[colors]
user = "#222222"
[colors.trace]
bash = "#010203"
[providers.openai]
base_url = "http://localhost:1234"
[env]
OTHER = "${{ env KEY }}"
[[hooks.bash_before]]
run = "echo c"
[completions]
fix = "local fix"
`)
	writeFile(t, bad, "[settings]\ncompaction = 1\nunknown = 2\n")
	writeFile(t, syntax, "model = \"x\"\n\nmodel = = 3\n")
	layers := []Layer{{"global", global}, {"local", local}, {"config", bad}, {"config", syntax}, {"config", filepath.Join(dir, "missing.toml")}}
	cfg := Load(context.Background(), layers, fakeRun)

	if len(cfg.Layers) != 2 || len(cfg.Errors) != 3 {
		t.Fatalf("layers %v, errors %q", cfg.Layers, cfg.Errors)
	}
	if !strings.HasPrefix(cfg.Errors[0], bad+": ") || !strings.Contains(cfg.Errors[0], "unknown") ||
		!strings.HasPrefix(cfg.Errors[1], syntax+":3: ") || !strings.HasPrefix(cfg.Errors[2], filepath.Join(dir, "missing.toml")+": ") {
		t.Fatalf("errors: %q", cfg.Errors)
	}
	s := cfg.Settings
	if s.Provider != "openai" || s.Model != "m2" || s.Effort != "medium" || s.CompactionThreshold != 1000 || s.BashTimeout != 5 ||
		s.UI.SlashMenu || !s.ReasoningSummary {
		t.Fatalf("settings: %+v", s)
	}
	wantColors := DefaultColors()
	wantColors.Set("accent", "#123456")
	wantColors.Set("user", "#222222")
	wantColors.Set("trace.bash", "#010203")
	if s.Colors != wantColors {
		t.Fatalf("colors: %v", s.Colors)
	}
	if p := s.Providers["openai"]; p.BaseURL != "http://localhost:1234" || p.APIKey != "" {
		t.Fatalf("a later layer replaces the whole provider: %+v", p)
	}
	if s.Env["KEY"] != "k-X:one" || s.Env["OTHER"] != "k-X:one" {
		t.Fatalf("env: %v", s.Env)
	}
	wantSrc := map[string]string{"provider": global, "model": local, "settings.compaction": global, "colors.accent": global,
		"colors.user": local, "colors.trace.bash": local, "settings.bash_timeout": local, "settings.slash_menu": local, "providers.openai": local, "env.KEY": global, "env.OTHER": local}
	if !reflect.DeepEqual(cfg.Sources, wantSrc) {
		t.Fatalf("sources = %v", cfg.Sources)
	}
	want := []Instruction{{"global X:hi", true, false, global}, {"headless only", false, true, global}, {"local", true, false, local}}
	if !reflect.DeepEqual(cfg.Instructions, want) {
		t.Fatalf("instructions = %+v", cfg.Instructions)
	}
	hooks := cfg.Hooks["bash_before"]
	if len(hooks) != 3 || hooks[0] != (Hook{Event: "bash_before", Run: "echo a", Main: true, Layer: global}) ||
		hooks[1] != (Hook{Event: "bash_before", Run: "echo b", Match: "^git", Timeout: 1500 * time.Millisecond, Async: true, Main: true, Headless: true, Layer: global}) ||
		hooks[2].Run != "echo c" || hooks[2].Layer != local || len(cfg.Hooks["idle"]) != 1 {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}
	if cfg.Completions["fix"] != "local fix" {
		t.Fatalf("completions = %v", cfg.Completions)
	}
	if len(cfg.Warnings) != 2 || !strings.Contains(cfg.Warnings[0], "completion #fix of "+local) ||
		!strings.HasPrefix(cfg.Warnings[1], global+": colors.accent: ${{ fail }}: boom") {
		t.Fatalf("warnings = %q", cfg.Warnings)
	}
	desc := cfg.Describe()
	for _, part := range []string{"model", "m2", "api_key=empty", "Env: KEY, OTHER", "#fix", "bash_before: echo b [main, headless]",
		`colors.trace.bash          = "#010203"       ` + DisplayPath(local), `colors.bg                  = ""`} {
		if !strings.Contains(desc, part) {
			t.Errorf("Describe lacks %q:\n%s", part, desc)
		}
	}

	// Without an expander, the raw text stays; completions are always raw.
	raw := Load(context.Background(), layers[:1], nil)
	if raw.Instructions[0].Text != "global ${{ hi }}" || raw.Completions["fix"] != "Fix ${{ x }}" || len(raw.Warnings) != 0 {
		t.Fatalf("raw: %+v", raw)
	}
	// A missing global file is fine; defaults apply.
	def := Load(context.Background(), []Layer{{"global", filepath.Join(dir, "none.toml")}}, nil)
	if len(def.Errors) != 0 || len(def.Layers) != 0 || !reflect.DeepEqual(def.Settings, DefaultSettings()) {
		t.Fatalf("defaults: %+v", def)
	}
	for _, content := range []string{
		"[hooks]\nnope = \"x\"", "[hooks]\nidle = [{ run = \"x\", match = \"(\" }]", "[settings]\ncompaction = -1",
		"[providers.x]\nbase_url = \"ftp://x\"", "instructions = [{ main = true }]", "[completions]\n\"a b\" = \"x\"",
	} {
		writeFile(t, bad, content)
		if c := Load(context.Background(), []Layer{{"config", bad}}, nil); len(c.Errors) != 1 || len(c.Layers) != 0 {
			t.Errorf("%q: errors %q", content, c.Errors)
		}
	}
}

func TestExpand(t *testing.T) {
	ctx := context.Background()
	run := fakeRun(nil)
	got, err := Expand(ctx, "a ${{ one }} b ${{two}} $${{ lit }} c", run)
	if err != nil || got != "a X:one b X:two ${{ lit }} c" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = Expand(ctx, "[${{ fail }}][${{ ok }}][${{ open", run)
	if err == nil || got != "[][X:ok][${{ open" || !strings.Contains(err.Error(), "${{ fail }}: boom") || !strings.Contains(err.Error(), "missing }}") {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := Expand(ctx, "${{ x }}", nil); got != "${{ x }}" || err != nil {
		t.Fatalf("nil run: %q, %v", got, err)
	}
	bash := func(ctx context.Context, script string) (string, error) {
		out, err := exec.CommandContext(ctx, "bash", "-c", script).Output()
		return string(out), err
	}
	if got, err := Expand(ctx, "<${{ printf 'a\\n\\n' }}>", bash); err != nil || got != "<a\n>" {
		t.Fatalf("bash: %q, %v", got, err)
	}
}

func TestEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "zel.toml")
	read := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if err := SetKey(path, "settings.accent", "blue"); err != nil || read() != "[settings]\naccent = \"blue\"\n" {
		t.Fatalf("create: %v %q", err, read())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	writeFile(t, path, `# my config

[settings] # ui
accent = "red" # favourite
compaction = 5
note = """
model = "fake"
"""

# local models
[providers.local]
base_url = "http://localhost"
list = [
  "x = 1",
]

# keep me
[providers.openai]
base_url = "https://api.openai.com"
`)
	steps := []struct {
		set   bool
		key   string
		value any
	}{
		{true, "settings.accent", "gr\"een\n"},
		{true, "settings.slash_menu", false},
		{true, "model", "m"},
		{true, "env.FOO", "bar"},
		{true, "providers.openai.api", "chat"},
		{false, "providers.local", nil},
		{false, "settings.compaction", nil},
	}
	for _, s := range steps {
		var err error
		if s.set {
			err = SetKey(path, s.key, s.value)
		} else {
			err = DeleteKey(path, s.key)
		}
		if err != nil {
			t.Fatalf("%s: %v", s.key, err)
		}
	}
	want := `# my config

model = "m"

[settings] # ui
accent = "gr\"een\n" # favourite
note = """
model = "fake"
"""
slash_menu = false

# keep me
[providers.openai]
base_url = "https://api.openai.com"
api = "chat"

[env]
FOO = "bar"
`
	if got := read(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := SetKey(path, "model", "n"); err != nil || !strings.Contains(read(), "\nmodel = \"n\"\n\n[settings]") {
		t.Fatalf("replace top-level: %v\n%s", err, read())
	}
	writeFile(t, path, "settings = { accent = \"x\" }\n")
	if err := SetKey(path, "settings.accent", "y"); err == nil || !strings.Contains(err.Error(), "edit it by hand") {
		t.Fatalf("inline table: %v", err)
	}
	if err := DeleteKey(filepath.Join(t.TempDir(), "none.toml"), "model"); err != nil {
		t.Fatal(err)
	}
}

func TestWatch(t *testing.T) {
	oldI, oldS := watchInterval, watchSettle
	watchInterval, watchSettle = 5*time.Millisecond, 20*time.Millisecond
	defer func() { watchInterval, watchSettle = oldI, oldS }()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
	writeFile(t, a, "x = 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() { Watch(ctx, []string{a, b}, func() { calls.Add(1) }); close(done) }()
	wait := func(n int32) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); calls.Load() < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("calls = %d, want %d", calls.Load(), n)
			}
		}
	}
	time.Sleep(20 * time.Millisecond)
	future := time.Now().Add(time.Hour)
	os.Chtimes(a, future, future) // same content
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("a touch without a change must not call fn")
	}
	writeFile(t, a, "x = 2\n")
	wait(1)
	writeFile(t, b, "new") // appears
	wait(2)
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	cancel()
	<-done
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

// The documented examples must load without errors.
func TestDocExamplesLoad(t *testing.T) {
	for _, name := range []string{"global-zel.toml", "project-zel.toml"} {
		path := filepath.Join("..", "..", "docs", "examples", name)
		cfg := Load(context.Background(), []Layer{{Name: "config", Path: path}}, nil)
		if len(cfg.Errors) > 0 || len(cfg.Layers) != 1 {
			t.Errorf("%s: %v", name, cfg.Errors)
		}
	}
}

// A bad color is a layer error; settings.accent still works as colors.accent, with a warning.
func TestColors(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{`[colors]` + "\naccent = \"red\"\n", "[colors]\nbg = \"#12345\"\n", "[colors]\nnope = \"#123456\"\n",
		"[colors.trace]\nuser = 1\n", "[colors.trace]\nbg = \"#123456\"\n", "[colors]\naccent = \"\"\n"} {
		path := filepath.Join(dir, "bad.toml")
		writeFile(t, path, bad)
		if cfg := Load(context.Background(), []Layer{{"config", path}}, nil); len(cfg.Errors) != 1 || !strings.Contains(cfg.Errors[0], "colors") {
			t.Errorf("%q: errors %q", bad, cfg.Errors)
		}
	}
	path := filepath.Join(dir, "old.toml")
	writeFile(t, path, "[settings]\naccent = \"#80b3ad\"\n[colors]\nbg = \"\"\n")
	cfg := Load(context.Background(), []Layer{{"config", path}}, nil)
	if cfg.Settings.Colors.Get("accent") != "#80b3ad" || cfg.Sources["colors.accent"] != path || cfg.Sources["settings.accent"] != path ||
		len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "colors.accent") || Validate(cfg.Settings) != nil {
		t.Fatalf("settings.accent: %v %v %q", cfg.Settings.Colors, cfg.Sources, cfg.Warnings)
	}
}
