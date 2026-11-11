package agent

import (
	"context"
	"dashy/internal/shell"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigLayers(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "projects", "app")
	t.Setenv("DASHY_TEST_KEY", "sk-secret")
	writeFile(t, filepath.Join(home, "config.toml"), `
[agent]
provider = "p"
model = "m"
effort = "low"
db_path = "~/x.db"

[ui]
bash_lines = -1
editor = "global-editor"

[ui.events]
tasks = true
hook_output = false

[provider.p]
base_url = "http://127.0.0.1:1/v1"

[system.b]
type = "text"
value = "global b"

[system.a]
type = "text"
value = "global a"
`)
	// A parent directory layer: only the key of a provider defined globally.
	writeFile(t, filepath.Join(root, "projects", "dashy.toml"), `
[agent]
model = "parent"

[provider.p]
api_key = "{{$DASHY_TEST_KEY}}"

[system.b]
type = "text"
value = "parent b"
`)
	writeFile(t, filepath.Join(dir, "dashy.toml"), `
[agent]
model = "project"

[ui]
editor = "project-editor --wait"

[system.c]
type = "bash"
value = "echo c"

[hooks.h]
events = ["turn_end"]
run = "true"
`)
	flagged := filepath.Join(root, "flag.toml")
	writeFile(t, flagged, "[agent]\neffort = \"high\"\n")
	t.Setenv("DASHY_CONFIG", flagged)
	s, err := LoadConfig(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	userHome, _ := os.UserHomeDir()
	p := s.Providers["p"]
	if s.Model != "project" || s.Effort != "high" || p.APIKey != "sk-secret" || p.BaseURL != "http://127.0.0.1:1/v1" || s.DBPath != filepath.Join(userHome, "x.db") {
		t.Fatalf("merged %+v", s)
	}
	if s.UI.Editor != "project-editor --wait" {
		t.Fatalf("editor = %q", s.UI.Editor)
	}
	if !s.UI.Show("tasks") || s.UI.Show("hook_output") || !s.UI.Show("errors") || !s.UI.Show("untouched") {
		t.Fatalf("ui %+v", s.UI)
	}
	var names []string
	for _, b := range s.System {
		names = append(names, b.Name+"="+b.Value)
	}
	if got := strings.Join(names, ","); got != "b=parent b,a=global a,c=echo c" {
		t.Fatalf("system order %q", got)
	}
	if len(s.Hooks) != 1 || s.Hooks[0].Timeout != HookTimeout || !s.Hooks[0].Enabled {
		t.Fatalf("hooks %+v", s.Hooks)
	}
	if Redact(s, "key sk-secret") != "key [redacted]" {
		t.Fatal("api key not redacted")
	}
	// The --config file must exist; the other layers are optional.
	t.Setenv("DASHY_CONFIG", filepath.Join(root, "missing.toml"))
	if _, err := LoadConfig(home, dir); err == nil || !strings.Contains(err.Error(), "missing.toml") {
		t.Fatalf("missing --config: %v", err)
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), "[agent]\nmodle = \"x\"\n")
	if _, err := LoadConfig(home, t.TempDir()); err == nil || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemBlocksBuildThePrompt(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		s, err := LoadConfig(home, dir)
		if err != nil {
			t.Fatal(err)
		}
		s.System = []SystemBlock{{"main", "text", "You are Dashy."}, {"pwd", "bash", "echo cwd=$(basename \"$PWD\")"}, {"fail", "bash", "echo nope; exit 1"}}
		if err := WriteConfig(home, s); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	want := "You are Dashy.\n\ncwd=" + filepath.Base(a.Shell.Dir)
	if !strings.Contains(fp.requests()[0], `"instructions":`+string(raw(want))) {
		t.Fatalf("request: %s", fp.requests()[0])
	}
}

func TestSessionSelectionRollback(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	original := a.Settings()
	for _, choice := range [][3]string{{"", "m", "high"}, {original.Provider, "", "high"}, {"unknown", "m", "high"}, {original.Provider, "m", "invalid"}} {
		if err := a.SelectSession(context.Background(), choice[0], choice[1], choice[2]); err == nil {
			t.Fatalf("accepted invalid selection: %v", choice)
		}
		if got := a.Settings(); got.Provider != original.Provider || got.Model != original.Model || got.Effort != original.Effort {
			t.Fatal("failed validation changed selection")
		}
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`CREATE TRIGGER fail_selection BEFORE UPDATE ON sessions BEGIN SELECT RAISE(FAIL, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.SelectSession(context.Background(), original.Provider, "other", "high"); err == nil {
		t.Fatal("storage failure ignored")
	}
	if got := a.Settings(); got.Model != original.Model || got.Effort != original.Effort {
		t.Fatal("storage failure did not restore selection")
	}
}

func TestSessionLockIsExclusive(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.Shell.Dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.Resume(a.SessionID())
	if err == nil || !strings.Contains(err.Error(), "in use by another dashy process") {
		t.Fatalf("second owner: %v", err)
	}
}

func TestValidImage(t *testing.T) {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG())
	cases := map[string]bool{
		png: true,
		"data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")): false,
		"https://example.com/a.png": false,
	}
	for url, want := range cases {
		if got := validImage(shell.Image{URL: url}); got != want {
			t.Errorf("validImage(%.40s...) = %v", url, got)
		}
	}
}

func TestSessionListIsScopedAndOrdered(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	for _, row := range [][]any{
		{"a", a.project, "Custom", "first", 3},
		{"b", a.project, "", "Saved first line", 2},
		{"c", "/elsewhere", "", "Other project", 1},
	} {
		if _, err := a.db.Exec(`INSERT INTO sessions (id, project, title, first, updated) VALUES (?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := a.Sessions()
	if err != nil || len(sessions) != 2 || sessions[0].ID != "a" || sessions[0].Title != "Custom" || sessions[1].Title != "Saved first line" {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
	if err := a.openSession("c"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("another project's session opened: %v", err)
	}
}

func TestRecordsDerivedFromHistory(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "say hi"); err != nil {
		t.Fatal(err)
	}
	records := a.Records()
	var kinds []string
	for _, r := range records {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "user,code,output,assistant" {
		t.Fatalf("records %+v", records)
	}
}

func TestSaveDefaultEditsTheSourceLayers(t *testing.T) {
	root := t.TempDir()
	home, dir := filepath.Join(root, "home"), filepath.Join(root, "proj")
	global := filepath.Join(home, "config.toml")
	writeFile(t, global, "# my config\n[agent]\nprovider = \"p\" # main\neffort = 'low'\n\n[provider.p]\nbase_url = \"http://127.0.0.1:1/v1\"\n")
	writeFile(t, filepath.Join(dir, "dashy.toml"), "[ui]\nbash_lines = 1\n\n[agent]\nmodel = \"old\"\n\n[ui.events]\ntasks = true\n")
	os.Chmod(global, 0640)
	base, err := LoadConfig(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveDefault(home, base, selection{"p", "new", "high"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(global); string(data) != "# my config\n[agent]\nprovider = \"p\" # main\neffort = \"high\"\n\n[provider.p]\nbase_url = \"http://127.0.0.1:1/v1\"\n" {
		t.Fatalf("global: %q", data)
	}
	if info, _ := os.Stat(global); info.Mode().Perm() != 0640 {
		t.Fatalf("mode %v", info.Mode())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "dashy.toml")); string(data) != "[ui]\nbash_lines = 1\n\n[agent]\nmodel = \"new\"\n\n[ui.events]\ntasks = true\n" {
		t.Fatalf("project: %q", data)
	}
	s, err := LoadConfig(home, dir)
	if err != nil || s.Model != "new" || s.Effort != "high" {
		t.Fatalf("reloaded %+v %v", s, err)
	}

	// Keys no layer sets go to the global config; an [agent] table is added when missing.
	empty := filepath.Join(root, "empty")
	writeFile(t, filepath.Join(empty, "config.toml"), "[ui]\nbash_lines = 2\n")
	if err := SaveDefault(empty, DefaultSettings(), selection{"p", "m", ""}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(empty, "config.toml")); string(data) != "[ui]\nbash_lines = 2\n\n[agent]\nprovider = \"p\"\nmodel = \"m\"\n" {
		t.Fatalf("added table: %q", data)
	}
}

func TestMediumReasoningDefault(t *testing.T) {
	if DefaultSettings().Effort != "medium" {
		t.Fatal("medium must be the default")
	}
	for _, effort := range Efforts {
		if effort == "" || effort == "default" {
			t.Fatal("default effort option is still listed")
		}
	}
	for _, value := range []string{"", "effort = \"\"", "effort = \"high\""} {
		home, project := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(home, "config.toml"), "[agent]\n"+value+"\n")
		s, err := LoadConfig(home, project)
		want := "medium"
		if strings.Contains(value, "high") {
			want = "high"
		}
		if err != nil || s.Effort != want {
			t.Fatalf("loaded effort = %q, %v", s.Effort, err)
		}
	}
}

func TestLegacyAndOmittedSessionEffortUseMedium(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	provider := a.Settings().Provider
	a.fixSelection(selection{provider, "legacy", ""})
	if got := a.Settings(); got.Effort != "medium" || got.Model != "legacy" {
		t.Fatalf("legacy selection: %+v", got)
	}
	if err := a.SelectSession(context.Background(), provider, "new", ""); err != nil {
		t.Fatal(err)
	}
	if a.Settings().Effort != "medium" {
		t.Fatal("omitted selection effort did not resolve to medium")
	}
	s, err := LoadConfig(a.home, a.Shell.Dir)
	if err != nil || s.Effort != "medium" {
		t.Fatalf("saved effort = %q, %v", s.Effort, err)
	}
}

func TestTraceListsHistoryTasksAndEvents(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.LogEvent("error", "boom\nsecond line")
	a.LogEvent("notice", "task finished")
	var kinds []string
	var logs []string
	for _, e := range a.Trace() {
		if e.Kind == "error" || e.Kind == "notice" {
			kinds = append(kinds, e.Kind)
			logs = append(logs, e.Text)
		}
	}
	if strings.Join(kinds, ",") != "error,notice" {
		t.Fatalf("events %v", kinds)
	}
	if logs[0] != "boom\nsecond line" || logs[1] != "task finished" {
		t.Fatalf("event text %q", logs)
	}
}

func TestEditorConfigurationRoundTrip(t *testing.T) {
	home := t.TempDir()
	settings := DefaultSettings()
	settings.UI.Editor = "nvim --clean"
	if err := WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UI.Editor != settings.UI.Editor {
		t.Fatalf("editor %q, want %q", loaded.UI.Editor, settings.UI.Editor)
	}
}
