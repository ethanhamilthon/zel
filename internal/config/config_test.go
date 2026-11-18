package config

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	base := DefaultSettings()
	s, err := ParseSettings(map[string]any{
		"compaction":  float64(100000),
		"timeout":     float64(45),
		"output":      float64(0),
		"show_code":   false,
		"show_prompt": true,
		"trace_hooks": false,
		"editor":      "code -w",
		"colors": map[string]any{
			"accent": "#112233",
			"trace":  map[string]any{"lua": "#010203"},
		},
		"keys": map[string]any{"menu": "alt+k"},
		"env":  map[string]any{"FOO": "bar"},
	}, base)
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	if s.CompactionThreshold != 100000 || s.Timeout != 45 || s.Output != 0 ||
		s.ShowCode || !s.ShowPrompt || s.TraceHooks || s.Editor != "code -w" {
		t.Fatalf("unexpected settings: %+v", s)
	}
	if s.Colors.Get("accent") != "#112233" || s.Colors.Get("trace.lua") != "#010203" {
		t.Fatalf("unexpected colors: %v", s.Colors)
	}
	if s.Keys["menu"] != "alt+k" || s.Env["FOO"] != "bar" {
		t.Fatalf("unexpected keys/env: %v %v", s.Keys, s.Env)
	}
	// Merging a second table keeps earlier overrides and does not mutate base.
	if base.Keys["menu"] != "ctrl+k" || len(base.Env) != 0 {
		t.Fatalf("base was mutated")
	}
	s2, err := ParseSettings(map[string]any{
		"colors": map[string]any{"bg": "#000000"},
		"env":    map[string]any{"BAZ": "qux"},
	}, s)
	if err != nil {
		t.Fatalf("merge ParseSettings: %v", err)
	}
	if s2.Colors.Get("accent") != "#112233" || s2.Colors.Get("bg") != "#000000" {
		t.Fatalf("colors did not deep merge: %v", s2.Colors)
	}
	if s2.Env["FOO"] != "bar" || s2.Env["BAZ"] != "qux" {
		t.Fatalf("env did not deep merge: %v", s2.Env)
	}
}

func TestParseSettingsErrors(t *testing.T) {
	base := DefaultSettings()
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"unknown key", map[string]any{"bogus": true}},
		{"float timeout", map[string]any{"timeout": 1.5}},
		{"zero timeout", map[string]any{"timeout": float64(0)}},
		{"small output", map[string]any{"output": float64(500)}},
		{"bad color", map[string]any{"colors": map[string]any{"accent": "red"}}},
		{"old trace.bash", map[string]any{"colors": map[string]any{"trace": map[string]any{"bash": "#112233"}}}},
		{"duplicate key", map[string]any{"keys": map[string]any{"menu": "ctrl+m"}}},
		{"bad env name", map[string]any{"env": map[string]any{"1BAD": "x"}}},
	}
	for _, tc := range cases {
		if _, err := ParseSettings(tc.raw, base); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestConnectLoadAndSaveSelection(t *testing.T) {
	t.Setenv("TEST_API_KEY", "secret-123")
	t.Setenv("TEST_HOST", "api.example.com")
	dir := t.TempDir()
	path := filepath.Join(dir, "connect.json")

	if _, err := LoadConnect(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}

	raw := `{
  "provider": "openai",
  "model": "gpt-5",
  "effort": "high",
  "extra": {"keep": true},
  "providers": {
    "openai": {
      "base_url": "https://${TEST_HOST}/v1",
      "api": "responses",
      "api_key": "$TEST_API_KEY",
      "literal": "$$100"
    }
  }
}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConnect(path)
	if err != nil {
		t.Fatalf("LoadConnect: %v", err)
	}
	p := c.Providers["openai"]
	if p.BaseURL != "https://api.example.com/v1" || p.APIKey != "secret-123" {
		t.Fatalf("unexpected provider expansion: %+v", p)
	}
	if expandEnv("$$100 $UNSET ${TEST_HOST}") != "$100  api.example.com" {
		t.Fatalf("expandEnv mismatch: %q", expandEnv("$$100 $UNSET ${TEST_HOST}"))
	}

	if err := SaveSelection(path, "local", "llama3", "low"); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(saved, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["provider"] != "local" || obj["model"] != "llama3" || obj["effort"] != "low" {
		t.Fatalf("selection not updated: %v", obj)
	}
	if extra, ok := obj["extra"].(map[string]any); !ok || extra["keep"] != true {
		t.Fatalf("extra key not preserved: %v", obj)
	}
}

func TestDefaultPathsAndEnsureHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "zelhome")
	cwd := filepath.Join(t.TempDir(), "proj")

	p := DefaultPaths(home, cwd, "")
	if p.Init != filepath.Join(home, "init.lua") ||
		p.Local != filepath.Join(cwd, "zel.lua") ||
		p.Connect != filepath.Join(home, "connect.json") {
		t.Fatalf("unexpected DefaultPaths: %+v", p)
	}

	pFlag := DefaultPaths(home, cwd, "custom.lua")
	if pFlag.Init != filepath.Join(cwd, "custom.lua") || pFlag.Local != "" || pFlag.Connect != filepath.Join(home, "connect.json") {
		t.Fatalf("unexpected DefaultPaths with flag: %+v", pFlag)
	}

	if err := EnsureHome(home); err != nil {
		t.Fatalf("EnsureHome: %v", err)
	}
	for _, name := range []string{"init.lua", "connect.json", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if _, err := LoadConnect(filepath.Join(home, "connect.json")); err != nil {
		t.Errorf("default connect.json invalid: %v", err)
	}
	// Existing files are not overwritten.
	if err := os.WriteFile(filepath.Join(home, "init.lua"), []byte("-- custom\nreturn {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "init.lua"))
	if !strings.Contains(string(data), "-- custom") {
		t.Fatalf("EnsureHome overwrote init.lua")
	}
}

func TestWatchDynamicFiles(t *testing.T) {
	oldInt, oldSettle := watchInterval, watchSettle
	watchInterval, watchSettle = 10*time.Millisecond, 25*time.Millisecond
	defer func() { watchInterval, watchSettle = oldInt, oldSettle }()

	dir := t.TempDir()
	f1 := filepath.Join(dir, "init.lua")
	f2 := filepath.Join(dir, "extra.lua")
	_ = os.WriteFile(f1, []byte("return {}"), 0600)

	var list atomic.Value
	list.Store([]string{f1})
	var calls atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()
	go func() {
		defer close(done)
		Watch(ctx, func() []string { return list.Load().([]string) }, func() { calls.Add(1) })
	}()

	time.Sleep(60 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("unexpected initial call")
	}

	_ = os.WriteFile(f2, []byte("return {}"), 0600)
	list.Store([]string{f1, f2})
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 watch call after adding file, got %d", calls.Load())
	}
}

func TestDescribeAndValidate(t *testing.T) {
	s := DefaultSettings()
	s.Provider, s.Model = "openai", "gpt-5"
	s.Providers["openai"] = Provider{BaseURL: "https://api.openai.com/v1", APIKey: "sk-secret", API: APIResponses}
	s.Env["TOKEN"] = "supersecret"
	if err := Validate(s); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cfg := &Config{
		Paths:     Paths{Init: "/tmp/init.lua", Local: "/tmp/zel.lua", Connect: "/tmp/connect.json"},
		Settings:  s,
		Systems:   []System{{Name: "style", Text: "Reply short."}},
		Prompts:   map[string]string{"review": "Review."},
		Functions: []Function{{Name: "ping", Description: "Ping host"}},
		Hooks:     []Hook{{Events: []string{"idle"}, Async: true, Source: "init.lua:10"}},
	}
	out := Describe(cfg)
	if strings.Contains(out, "sk-secret") || strings.Contains(out, "supersecret") {
		t.Fatalf("Describe leaked secret:\n%s", out)
	}
	for _, want := range []string{"settings.timeout", "settings.output", "settings.show_code", "settings.show_prompt", "colors.trace.lua", "#review", "ping: Ping host", "init.lua:10 (idle) [async]"} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe missing %q:\n%s", want, out)
		}
	}
}
