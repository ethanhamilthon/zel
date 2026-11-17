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

func TestDirectoryLayoutAndKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZEL_HOME", "")
	root, err := ZelHome(home)
	if err != nil || root != filepath.Join(home, ".config", "zel") {
		t.Fatalf("home: %s %v", root, err)
	}
	if err := EnsureHome(root); err != nil {
		t.Fatal(err)
	}
	path := File(root)
	writeFile(t, path, "[keys]\nmenu = 'alt+k'\n")
	writeFile(t, filepath.Join(root, "prompts", "plan.md"), "Plan first. ${{ echo preview }}")
	writeFile(t, filepath.Join(root, "instructions", "rules.toml"), "[instruction.rules]\ntext='Read first'\nmodes=['headless']\n")
	writeFile(t, filepath.Join(root, "hooks", "notify.toml"), "[hook.notify]\nevent='turn_end'\nrun='true'\n")
	cfg := Load(context.Background(), path, fakeRun)
	if len(cfg.Errors) != 0 || cfg.Settings.Keys["menu"] != "alt+k" || len(cfg.Completions) != 1 || len(cfg.Instructions) != 1 || len(cfg.Hooks["turn_end"]) != 1 {
		t.Fatalf("config: %+v", cfg)
	}
	if cfg.Completions["plan"].Text != "Plan first. ${{ echo preview }}" {
		t.Fatal("preview executed expansion")
	}
	before := treeStamp(path)
	writeFile(t, filepath.Join(root, "prompts", "new.md"), "new")
	if treeStamp(path) == before {
		t.Fatal("addition not watched")
	}
	if err := os.Remove(filepath.Join(root, "prompts", "new.md")); err != nil {
		t.Fatal(err)
	}
	if treeStamp(path) != before {
		t.Fatal("removal not watched")
	}
	writeFile(t, filepath.Join(root, "prompts", "duplicate.toml"), "[completion]\nplan='duplicate'\n")
	if cfg := Load(context.Background(), path, nil); len(cfg.Errors) == 0 {
		t.Fatal("duplicate accepted")
	}
}

func TestInvalidKeyBindings(t *testing.T) {
	for _, text := range []string{"menu='ctrl+q'", "unknown='ctrl+x'", "menu='f999'", "menu=42"} {
		path := filepath.Join(t.TempDir(), "settings.toml")
		writeFile(t, path, "[keys]\n"+text)
		if cfg := Load(context.Background(), path, nil); len(cfg.Errors) == 0 {
			t.Fatalf("accepted %s", text)
		}
	}
	path := filepath.Join(t.TempDir(), "settings.toml")
	writeFile(t, path, "[keys]\nmenu=''\n")
	if cfg := Load(context.Background(), path, nil); len(cfg.Errors) != 0 || cfg.Settings.Keys["menu"] != "" {
		t.Fatalf("disable: %+v", cfg)
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

func TestEnsureHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "zel")
	if err := EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	if cfg := Load(context.Background(), File(home), nil); len(cfg.Errors) != 0 || cfg.Settings.Provider != "" {
		t.Fatalf("the default config must load and set nothing: %+v", cfg)
	}
	ignore, _ := os.ReadFile(filepath.Join(home, ".gitignore"))
	if string(ignore) != ".env\n.sessions/\n" {
		t.Fatalf(".gitignore = %q", ignore)
	}
	writeFile(t, File(home), "model = \"mine\"\n")
	if err := EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(File(home)); string(data) != "model = \"mine\"\n" {
		t.Fatalf("an existing config must stay: %q", data)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path, bad, syntax := filepath.Join(dir, "c.toml"), filepath.Join(dir, "bad.toml"), filepath.Join(dir, "syn.toml")
	writeFile(t, path, `
model = "openai/m1"
[instruction.global]
text = "global ${{ hi }}"
[instruction.sub]
text = "headless only"
modes = ["headless", "plan"]
[instruction]
empty = ""
[settings]
compaction = 1000
bash_timeout = 5
bash_output = 2000
editor = "code -w"
trace_hooks = false
[colors]
accent = "${{ fail }}#123456"
user = "#111111"
[colors.trace]
bash = "#010203"
[provider.openai]
base_url = "https://api.openai.com/v1"
api_key = "${{ env KEY }}"
[env]
KEY = "k-${{ one }}"
OTHER = "${{ env KEY }}"
[hook.a]
event = "bash_before"
run = "echo a"
[hook.b]
event = ["bash_before", "bash_after"]
run = "echo b"
match = "^git"
timeout = 1.5
async = true
modes = ["main", "headless"]
[hook.idle]
event = "idle"
run = "echo idle"
[completion]
fix = "Fix ${{ x }}"
plan = { text = "plan", modes = ["plan"] }
`)
	writeFile(t, bad, "[settings]\ncompaction = 1\nunknown = 2\n")
	writeFile(t, syntax, "model = \"x\"\n\nmodel = = 3\n")
	cfg := Load(context.Background(), path, fakeRun)

	if cfg.Path != path || len(cfg.Errors) != 0 {
		t.Fatalf("path %q, errors %q", cfg.Path, cfg.Errors)
	}
	s := cfg.Settings
	if s.Provider != "openai" || s.Model != "m1" || s.Effort != "medium" || s.CompactionThreshold != 1000 || s.BashTimeout != 5 || s.BashOutput != 2000 || s.Editor != "code -w" || s.TraceHooks || !DefaultSettings().TraceHooks {
		t.Fatalf("settings: %+v", s)
	}
	wantColors := DefaultColors()
	wantColors.Set("accent", "#123456")
	wantColors.Set("user", "#111111")
	wantColors.Set("trace.bash", "#010203")
	if s.Colors != wantColors {
		t.Fatalf("colors: %v", s.Colors)
	}
	if p := s.Providers["openai"]; p.BaseURL != "https://api.openai.com/v1" || p.APIKey != "k-X:one" {
		t.Fatalf("provider: %+v", p)
	}
	if s.Env["KEY"] != "k-X:one" || s.Env["OTHER"] != "k-X:one" {
		t.Fatalf("env: %v", s.Env)
	}
	want := []Instruction{{"global", "global X:hi", []string{"main"}}, {"sub", "headless only", []string{"headless", "plan"}}}
	if !reflect.DeepEqual(cfg.Instructions, want) {
		t.Fatalf("instructions = %+v", cfg.Instructions)
	}
	hooks := cfg.Hooks["bash_before"]
	b := Hook{Name: "b", Event: "bash_before", Run: "echo b", Match: "^git", Timeout: 1500 * time.Millisecond, Async: true, Modes: []string{"main", "headless"}}
	if len(hooks) != 2 || !reflect.DeepEqual(hooks[0], Hook{Name: "a", Event: "bash_before", Run: "echo a", Modes: []string{"main"}}) ||
		!reflect.DeepEqual(hooks[1], b) || len(cfg.Hooks["idle"]) != 1 || len(cfg.Hooks["bash_after"]) != 1 {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}
	if cfg.Completions["fix"].Text != "Fix ${{ x }}" || !reflect.DeepEqual(cfg.Completions["plan"].Modes, []string{"plan"}) {
		t.Fatalf("completions = %v", cfg.Completions)
	}
	if len(cfg.Warnings) != 1 || !strings.HasPrefix(cfg.Warnings[0], "colors.accent: ${{ fail }}: boom") {
		t.Fatalf("warnings = %q", cfg.Warnings)
	}
	desc := cfg.Describe()
	for _, part := range []string{"model", "m1", "api_key=set", "Env: KEY, OTHER", "#fix", "#plan [plan]", "b (bash_before): echo b [main, headless]", "sub: headless only [headless, plan]",
		`colors.trace.bash          = "#010203"`, `colors.bg                  = ""`, "settings.trace_hooks       = false"} {
		if !strings.Contains(desc, part) {
			t.Errorf("Describe lacks %q:\n%s", part, desc)
		}
	}

	// Without an expander, the raw text stays; completions are always raw.
	raw := Load(context.Background(), path, nil)
	if raw.Instructions[0].Text != "global ${{ hi }}" || raw.Completions["fix"].Text != "Fix ${{ x }}" || len(raw.Warnings) != 0 {
		t.Fatalf("raw: %+v", raw)
	}
	// A file that cannot be read or is invalid gives the defaults and one error.
	missing := filepath.Join(dir, "missing.toml")
	for path, prefix := range map[string]string{bad: bad + ": ", syntax: syntax + ":3:9: ", missing: missing + ": "} {
		c := Load(context.Background(), path, nil)
		if len(c.Errors) != 1 || !strings.HasPrefix(c.Errors[0], prefix) || !reflect.DeepEqual(c.Settings, DefaultSettings()) {
			t.Errorf("%s: %+v", path, c)
		}
	}
	// A syntax error shows its line with a caret under the error.
	writeFile(t, bad, "[instruction.main]\ntext = \"x\"\n[instruction.main]\n")
	dup := bad + ":3:2: Key 'instruction.main' has already been defined\n 3 | [instruction.main]\n   |  ^"
	if c := Load(context.Background(), bad, nil); len(c.Errors) != 1 || !strings.HasPrefix(c.Errors[0], dup) {
		t.Errorf("duplicate table: errors %q", c.Errors)
	}
	for _, content := range []string{
		"[hook.x]\nevent = \"nope\"\nrun = \"x\"", "[hook.x]\nevent = \"idle\"\nrun = \"x\"\nmatch = \"(\"", "[settings]\ncompaction = -1",
		"[provider.x]\nbase_url = \"ftp://x\"", "[instruction.x]\nmodes = [\"main\"]", "[completion]\n\"a b\" = \"x\"",
		"[settings]\naccent = \"#123456\"", "[hook.x]\nevent = \"idle\"\nrun = \"x\"\nmatch = \"y\"", "[hook]\nx = \"echo\"",
		"[instruction.x]\ntext = \"a\"\nmodes = []", "provider = \"openai\"", "[providers.x]\nbase_url = \"https://x\"",
	} {
		writeFile(t, bad, content)
		if c := Load(context.Background(), bad, nil); len(c.Errors) != 1 {
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
	path := filepath.Join(t.TempDir(), "sub", "settings.toml")
	read := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if err := SetKey(path, "model", "m1"); err != nil || read() != "model = \"m1\"\n" {
		t.Fatalf("create: %v %q", err, read())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	writeFile(t, path, `# my config
provider = "openai" # default
instructions = [
  """
[not a table]
model = "fake"
""",
]

[settings] # ui
compaction = 5
model = "inside a table"

[providers.openai]
base_url = "https://api.openai.com"
`)
	for _, kv := range [][2]string{{"model", "gr\"een\n"}, {"effort", "high"}, {"provider", "local"}} {
		if err := SetKey(path, kv[0], kv[1]); err != nil {
			t.Fatalf("%s: %v", kv[0], err)
		}
	}
	want := `# my config
provider = "local"
instructions = [
  """
[not a table]
model = "fake"
""",
]
model = "gr\"een\n"
effort = "high"

[settings] # ui
compaction = 5
model = "inside a table"

[providers.openai]
base_url = "https://api.openai.com"
`
	if got := read(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	writeFile(t, path, "[settings]\ncompaction = 5\n")
	if err := SetKey(path, "model", "n"); err != nil || read() != "model = \"n\"\n\n[settings]\ncompaction = 5\n" {
		t.Fatalf("before the first table: %v\n%s", err, read())
	}
}

func TestWatch(t *testing.T) {
	oldI, oldS := watchInterval, watchSettle
	watchInterval, watchSettle = 5*time.Millisecond, 20*time.Millisecond
	defer func() { watchInterval, watchSettle = oldI, oldS }()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	writeFile(t, a, "x = 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() { Watch(ctx, a, func() { calls.Add(1) }); close(done) }()
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
	os.Remove(a) // disappears
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
	path := filepath.Join("..", "..", "docs", "examples", "settings.toml")
	if cfg := Load(context.Background(), path, nil); len(cfg.Errors) > 0 {
		t.Errorf("%s: %v", path, cfg.Errors)
	}
}

// A bad color makes the file invalid.
func TestColors(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{`[colors]` + "\naccent = \"red\"\n", "[colors]\nbg = \"#12345\"\n", "[colors]\nnope = \"#123456\"\n",
		"[colors.trace]\nuser = 1\n", "[colors.trace]\nbg = \"#123456\"\n", "[colors]\naccent = \"\"\n"} {
		path := filepath.Join(dir, "bad.toml")
		writeFile(t, path, bad)
		if cfg := Load(context.Background(), path, nil); len(cfg.Errors) != 1 || !strings.Contains(cfg.Errors[0], "colors") {
			t.Errorf("%q: errors %q", bad, cfg.Errors)
		}
	}
}
