package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"zel/internal/shell"
)

func TestLoadConfigLayers(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "projects", "app")
	t.Setenv("ZEL_TEST_KEY", "sk-secret")
	settings := DefaultSettings()
	settings.Provider, settings.Model = "p", "m"
	settings.Providers["p"] = Provider{BaseURL: "http://127.0.0.1:1/v1", APIKey: "{{$ZEL_TEST_KEY}}"}
	settings.UI.SlashMenu = false
	if err := WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "ZEL.md"), "Global prompt")
	writeFile(t, filepath.Join(root, "projects", "ZEL.md"), "Parent prompt")
	writeFile(t, filepath.Join(dir, "ZEL.md"), "Local prompt")
	flagged := filepath.Join(root, "override.md")
	writeFile(t, flagged, "Flag prompt")
	t.Setenv("ZEL_CONFIG", flagged)
	loaded, err := LoadConfig(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["p"].APIKey != "sk-secret" || loaded.UI.SlashMenu {
		t.Fatalf("loaded %+v", loaded)
	}
	var prompts []string
	for _, b := range loaded.System {
		prompts = append(prompts, b.Value)
	}
	if strings.Join(prompts, "|") != "Global prompt|Parent prompt|Local prompt|Flag prompt" {
		t.Fatalf("layers %+v", loaded)
	}
	if Redact(loaded, "key sk-secret") != "key [redacted]" {
		t.Fatal("API key not redacted")
	}
	if err := WriteConfig(home, loaded); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(home, dir)
	if err != nil || len(again.System) != 4 {
		t.Fatalf("project state was persisted: %+v %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("TOML file created")
	}
	t.Setenv("ZEL_CONFIG", filepath.Join(root, "missing.md"))
	if _, err := LoadConfig(home, dir); err == nil {
		t.Fatal("missing explicit file accepted")
	}
}

func TestProjectPromptBuildsTheRequest(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) { writeFile(t, filepath.Join(dir, "ZEL.md"), "You are Zel.") })
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.requests()[0], `"instructions":"You are Zel."`) {
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
	if err == nil || !strings.Contains(err.Error(), "in use by another zel process") {
		t.Fatalf("second owner: %v", err)
	}
}

func TestBashToolEnvParentSession(t *testing.T) {
	t.Setenv("ZEL_PARENT", "")
	a := newTestAgent(t, &fakeProvider{}, Options{})
	env := a.Shell.Env()
	if got, want := env["ZEL_PARENT"], a.SessionID(); got != want {
		t.Fatalf("unset ZEL_PARENT: got %q, want %q", got, want)
	}
	if got := ParentSession(); got != "" {
		t.Fatalf("unset ParentSession: got %q, want empty", got)
	}
	res, _ := a.Shell.Run(context.Background(), "echo $ZEL_PARENT", time.Second, 100, nil)
	if got, want := strings.TrimSpace(res.Output), a.SessionID(); got != want {
		t.Fatalf("executed script with unset ZEL_PARENT: got %q, want %q", got, want)
	}

	const inherited = "root-parent-123"
	t.Setenv("ZEL_PARENT", inherited)
	env = a.Shell.Env()
	if got, want := env["ZEL_PARENT"], inherited; got != want {
		t.Fatalf("inherited ZEL_PARENT: got %q, want %q", got, want)
	}
	if got := ParentSession(); got != inherited {
		t.Fatalf("inherited ParentSession: got %q, want %q", got, inherited)
	}
	res, _ = a.Shell.Run(context.Background(), "echo $ZEL_PARENT", time.Second, 100, nil)
	if got, want := strings.TrimSpace(res.Output), inherited; got != want {
		t.Fatalf("executed script with inherited ZEL_PARENT: got %q, want %q", got, want)
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

func TestSaveDefaultUsesDatabase(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	s := DefaultSettings()
	s.Providers["p"] = Provider{BaseURL: "http://localhost:1"}
	path := filepath.Join(dir, "ZEL.md")
	original := "---\nname: Untouched\n---\nPrompt"
	writeFile(t, path, original)
	if err := SaveDefault(home, s, selection{"p", "new", "high"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(home, dir)
	if err != nil || loaded.Model != "new" || loaded.Effort != "high" {
		t.Fatalf("settings %+v %v", loaded, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Fatal("ZEL.md changed")
	}
	info, err := os.Stat(filepath.Join(home, "zel.db"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %v %v", info, err)
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
	for _, effort := range []string{"medium", "high"} {
		home, project := t.TempDir(), t.TempDir()
		settings := DefaultSettings()
		settings.Effort = effort
		if err := WriteConfig(home, settings); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(home, project)
		if err != nil || loaded.Effort != effort {
			t.Fatalf("effort %s, %v", loaded.Effort, err)
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
	t.Setenv("ZEL_HOME", t.TempDir())
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

func TestTraceMergesSourcesChronologically(t *testing.T) {
	t.Setenv("ZEL_HOME", t.TempDir())
	a, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a.message("user", "first")
	a.setItemMeta(0, Metadata{At: base})
	a.message("assistant", "last")
	a.setItemMeta(1, Metadata{At: base.Add(5 * time.Second)})
	// Two records with equal timestamps must keep their original history order.
	a.message("assistant", "same-time")
	a.setItemMeta(2, Metadata{At: base.Add(5 * time.Second)})
	if _, err := a.db.Exec(`INSERT INTO tasks(session,id,command,status,started,log) VALUES(?, 't1','build','done',?,'')`, a.id, base.Add(time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	path := EventsLog(a.home, a.id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := base.Add(2*time.Second).Format(time.RFC3339Nano) + " error failure\n    detail\n" + base.Add(3*time.Second).Format(time.RFC3339Nano) + " notice recovery\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	entries := a.Trace()
	var order []string
	for _, e := range entries {
		order = append(order, e.Kind+":"+e.Text)
	}
	want := "user:first|task:t1 $ build · done|error:failure\ndetail|notice:recovery|assistant:last|assistant:same-time"
	if strings.Join(order, "|") != want {
		t.Fatalf("trace order: %q", order)
	}
}

func TestTraceEventTimestampsSupportLegacyLocalAndPreciseNewLogs(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("test", 3*60*60)
	defer func() { time.Local = previous }()
	t.Setenv("ZEL_HOME", t.TempDir())
	a, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	path := EventsLog(a.home, a.id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("2026-01-02 03:04:05 error legacy\n    multiline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	a.LogEvent("notice", "new")
	after := time.Now()
	entries := a.eventTrace()
	expected := time.Date(2026, 1, 2, 0, 4, 5, 0, time.UTC)
	if len(entries) != 2 || !entries[0].At.Equal(expected) || entries[0].Text != "legacy\nmultiline" {
		t.Fatalf("legacy log: %+v", entries)
	}
	if entries[1].At.Before(before) || entries[1].At.After(after) {
		t.Fatalf("new timestamp lost precision/zone: %+v", entries[1])
	}
}

func TestPackageBoundaries(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate package sources")
	}
	root := filepath.Dir(filepath.Dir(source))
	allowed := map[string]map[string]bool{
		"entities": {}, "config": {},
		"provider": {"entities": true}, "store": {"entities": true}, "shell": {"entities": true},
		"agent": {"entities": true},
		"tui":   {"core": true, "entities": true}, "cli": {"core": true, "entities": true},
	}
	for name, dependencies := range allowed {
		files, err := filepath.Glob(filepath.Join(root, name, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range parsed.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if dependency, internal := strings.CutPrefix(path, "zel/internal/"); internal && !dependencies[dependency] {
					t.Errorf("%s imports forbidden package %s", file, dependency)
				}
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "*", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines += bytes.Count(data, []byte{'\n'})
	}
	main, err := os.ReadFile(filepath.Join(filepath.Dir(root), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines += bytes.Count(main, []byte{'\n'})
	const maxProductionLines = 10700 // Keep a strict cap while accommodating 0.3.x features.
	if lines >= maxProductionLines {
		t.Errorf("production Go code has %d lines; keep it below %d", lines, maxProductionLines)
	}
}
