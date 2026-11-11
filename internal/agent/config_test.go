package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
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
	settings.UI.Editor = "nvim --clean"
	if err := WriteConfig(home, settings); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "ZEL.md"), "---\nname: Global\ncolor: sage\nhooks:\n  turn_start: echo global\n---\nGlobal prompt")
	writeFile(t, filepath.Join(root, "projects", "ZEL.md"), "---\ndescription: Parent description\nhooks:\n  turn_start: echo parent\n---\nParent prompt")
	writeFile(t, filepath.Join(dir, "ZEL.md"), "---\nname: Project\nhooks:\n  turn_start: [echo local, echo other]\n---\nLocal prompt")
	flagged := filepath.Join(root, "override.md")
	writeFile(t, flagged, "---\ncolor: blue\n---\nFlag prompt")
	t.Setenv("ZEL_CONFIG", flagged)
	loaded, err := LoadConfig(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "Project" || loaded.Description != "Parent description" || loaded.Color != "blue" || loaded.Providers["p"].APIKey != "sk-secret" || loaded.UI.Editor != "nvim --clean" {
		t.Fatalf("loaded %+v", loaded)
	}
	var prompts []string
	for _, b := range loaded.System {
		prompts = append(prompts, b.Value)
	}
	if strings.Join(prompts, "|") != "Global prompt|Parent prompt|Local prompt|Flag prompt" || len(loaded.Hooks) != 4 {
		t.Fatalf("layers %+v", loaded)
	}
	if Redact(loaded, "key sk-secret") != "key [redacted]" {
		t.Fatal("API key not redacted")
	}
	if err := WriteConfig(home, loaded); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(home, dir)
	if err != nil || len(again.System) != 4 || len(again.Hooks) != 4 {
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

func TestLoadConfigRejectsMalformedFrontMatter(t *testing.T) {
	for _, text := range []string{"---\nmodle: x\n---\n", "---\ncolor: unknown\n---\n", "---\nhooks:\n  unknown: echo hi\n---\n", "---\nname: x", "---\nhooks: []\n---\n", "---\nhooks:\n  turn_start: ''\n---\n"} {
		home, dir := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(dir, "ZEL.md"), text)
		if _, err := LoadConfig(home, dir); err == nil {
			t.Fatalf("accepted %q", text)
		}
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

func TestHookOutputIsBoundedAndFailuresAreTraceOnly(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	a.cfg.mu.Lock()
	base := a.cfg.base
	base.ShellOutput = 4096
	base.Hooks = []Hook{
		{Name: "large", Events: []string{"turn_start"}, Run: "printf '%02048d' 0 | tr '0' 'A'; printf '%03000d' 0 | tr '0' 'X'; printf '%02048d' 0 | tr '0' 'Z'"},
		{Name: "failed", Events: []string{"turn_start"}, Run: "echo rejected; echo stderr-message >&2; exit 7"},
		{Name: "empty", Events: []string{"turn_start"}, Run: "printf '   '"},
	}
	if err := a.applyLocked(base); err != nil {
		t.Fatal(err)
	}
	a.cfg.mu.Unlock()
	a.fire("turn_start", nil)
	output := a.takePasted()
	if len(output) != 1 || !strings.Contains(output[0], strings.Repeat("A", 2048)+"\n[output truncated:") || !strings.Contains(output[0], "first 2048 + last 2048 kept)") || !strings.Contains(output[0], strings.Repeat("Z", 2048)+"\n</hook-output>") || strings.Contains(output[0], "XXXX") {
		t.Fatalf("output: %q", output)
	}
	var trace string
	for _, e := range a.Trace() {
		trace += e.Kind + ":" + e.Text + "\n"
	}
	if !strings.Contains(trace, "stderr-message") || !strings.Contains(trace, "failed: exit status 7") {
		t.Fatalf("trace: %s", trace)
	}
	if len(a.takePasted()) != 0 {
		t.Fatal("output consumed twice")
	}
}

func TestHookLifecycleAndTurnBoundaries(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "printf bash-output")
		}
		return textReply("done")
	}}
	var logPath string
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		logPath = filepath.Join(dir, "events.txt")
		var hooks strings.Builder
		for _, event := range HookEvents {
			hooks.WriteString("  " + event + ": './hook.sh'\n")
		}
		writeFile(t, filepath.Join(dir, "ZEL.md"), "---\nhooks:\n"+hooks.String()+"---\nProject instructions")
		writeFile(t, filepath.Join(dir, "hook.sh"), `#!/bin/sh
printf '%s:%s:%s:%s\n' "$ZEL_EVENT" "$ZEL_SESSION_ID" "$ZEL_MESSAGE" "$ZEL_COMMAND" >> events.txt
case "$ZEL_EVENT" in
 turn_start) echo before-turn ;;
 model_request_start) echo before-request ;;
 bash_end) printf 'after-bash:%s\n' "$ZEL_OUTPUT" ;;
 turn_end) echo after-turn ;;
 app_exit|session_close) echo closing-only ;;
esac
`)
		if err := os.Chmod(filepath.Join(dir, "hook.sh"), 0700); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := turn(t, a, "first"); err != nil {
		t.Fatal(err)
	}
	if len(fp.requests()) != 2 {
		t.Fatal("hook output triggered an additional request")
	}
	reqs := fp.requests()
	if !strings.Contains(reqs[0], "before-turn") || !strings.Contains(reqs[0], "before-request") || strings.Contains(reqs[0], "after-turn") || !strings.Contains(reqs[1], "after-bash:bash-output") {
		t.Fatalf("requests: %v", reqs)
	}
	if _, err := turn(t, a, "second"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.requests()[2], "after-turn") {
		t.Fatal("turn-end output not delivered on next user turn")
	}
	id := a.SessionID()
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "resumed"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp.requests()[3], "closing-only") {
		t.Fatal("closing output reached model")
	}
	a.Close()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for event, want := range map[string]int{"first_message:": 1, "user_message:": 3, "app_start:": 1, "app_exit:": 1, "session_start:": 3, "session_close:": 3, "bash_start:": 1, "bash_end:": 1} {
		count := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, event) {
				count++
			}
		}
		if count != want {
			t.Fatalf("%s count %d want %d\n%s", event, count, want, text)
		}
	}
	if !strings.Contains(text, "user_message:"+id+":first:") {
		t.Fatalf("missing event environment: %s", text)
	}
}

func TestHookTimeoutKillsProcessGroup(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	a.cfg.mu.Lock()
	base := a.cfg.base
	base.Hooks = []Hook{{Name: "timeout", Events: []string{"app_exit"}, Run: "sleep 60 & wait"}}
	if err := a.applyLocked(base); err != nil {
		t.Fatal(err)
	}
	a.cfg.mu.Unlock()
	started := time.Now()
	a.fire("app_exit", nil)
	elapsed := time.Since(started)
	if elapsed < 9*time.Second || elapsed > 13*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if len(a.takePasted()) != 0 {
		t.Fatal("closing hook output queued")
	}
	base.Hooks = nil
	a.cfg.mu.Lock()
	_ = a.applyLocked(base)
	a.cfg.mu.Unlock()
	var errors string
	for _, e := range a.Trace() {
		if e.Kind == "error" {
			errors += e.Text
		}
	}
	if !strings.Contains(errors, "Hook timeout failed") {
		t.Fatalf("timeout not traced: %s", errors)
	}
}
