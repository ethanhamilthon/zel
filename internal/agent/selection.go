package agent

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type ModelChoice struct {
	Provider, Model string
	ContextWindow   int64
}

func AllModels(ctx context.Context, providers map[string]Provider) (choices []ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []modelInfo
		err    error
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p Provider) {
			models, err := newClient(p).modelCatalog(ctx)
			if err != nil && p.key() != "" {
				err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
			}
			results <- result{name, models, err}
		}(name, p)
	}
	errs = map[string]error{}
	for range providers {
		r := <-results
		if r.err != nil {
			errs[r.name] = r.err
			continue
		}
		for _, m := range r.models {
			choices = append(choices, ModelChoice{Provider: r.name, Model: m.ID, ContextWindow: m.window()})
		}
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Provider != choices[j].Provider {
			return choices[i].Provider < choices[j].Provider
		}
		return choices[i].Model < choices[j].Model
	})
	return choices, errs
}

func ProviderModels(ctx context.Context, p Provider) ([]string, error) {
	models, err := newClient(p).models(ctx)
	if err != nil && p.key() != "" {
		err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
	}
	sort.Strings(models)
	return models, err
}

// SelectSession changes the model of this session and makes it the default for new
// sessions: each key is written to the config layer it came from (see SaveDefault).
// Saved sessions keep their own model. Call between turns. Failed validation or
// persistence of the session restores the old selection.
func (a *Agent) SelectSession(ctx context.Context, provider, model, effort string) error {
	if effort == "" {
		effort = "medium"
	}
	if provider == "" || model == "" {
		return errors.New("provider and model are required")
	}
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.sel
	c.sel = selection{provider, model, effort}
	err := a.applyLocked(c.base)
	if err == nil {
		_, err = a.db.ExecContext(ctx, `UPDATE sessions SET provider = ?, model = ?, effort = ? WHERE id = ?`, provider, model, effort, a.id)
	}
	if err != nil {
		c.sel = previous
		_ = a.applyLocked(c.base)
		return err
	}
	if err := SaveDefault(a.home, c.base, c.sel); err != nil {
		return fmt.Errorf("model selected for this session, but the default was not saved: %w", err)
	}
	if base, err := LoadConfig(a.home, a.Shell.Dir); err == nil {
		_ = a.applyLocked(base)
	}
	return nil
}

// SaveDefault writes a model selection as the [agent] default. Each key goes to the
// layer that set it, so it is not shadowed; a key no layer sets goes where model
// came from, else to $home/config.toml. Unchanged keys are not written.
func SaveDefault(home string, base Settings, sel selection) error {
	sel.Effort = cmp.Or(sel.Effort, "medium")
	fallback := cmp.Or(base.Sources["model"], filepath.Join(home, "config.toml"))
	edits := map[string][][2]string{}
	var order []string
	for _, kv := range [][3]string{{"provider", base.Provider, sel.Provider}, {"model", base.Model, sel.Model}, {"effort", base.Effort, sel.Effort}} {
		if kv[1] == kv[2] {
			continue
		}
		path := cmp.Or(base.Sources[kv[0]], fallback)
		if edits[path] == nil {
			order = append(order, path)
		}
		edits[path] = append(edits[path], [2]string{kv[0], kv[2]})
	}
	for _, path := range order {
		if err := setAgentKeys(path, edits[path]); err != nil {
			return err
		}
	}
	return nil
}

var (
	tableHeader = regexp.MustCompile(`^\s*\[`)
	agentHeader = regexp.MustCompile(`^\s*\[\s*agent\s*\]\s*(#.*)?$`)
)

// setAgentKeys sets string keys of the [agent] table in place, keeping comments and
// every other line. The result is decoded again before the file is replaced.
func setAgentKeys(path string, keys [][2]string) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	mode := os.FileMode(0600)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	start := -1
	for i, line := range lines {
		if agentHeader.MatchString(line) {
			start = i
			break
		}
	}
	if start < 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines, start = append(lines, "[agent]"), len(lines)
	}
	for _, kv := range keys {
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			if tableHeader.MatchString(lines[i]) {
				end = i
				break
			}
		}
		quoted := strconv.Quote(kv[1])
		line := regexp.MustCompile(`^(\s*` + kv[0] + `\s*=\s*)("(?:[^"\\]|\\.)*"|'[^']*')(.*)$`)
		key := regexp.MustCompile(`^\s*` + kv[0] + `\s*=`)
		found, last := false, start
		for i := start + 1; i < end; i++ {
			if m := line.FindStringSubmatch(lines[i]); m != nil {
				lines[i], found = m[1]+quoted+m[3], true
				break
			} else if key.MatchString(lines[i]) {
				return fmt.Errorf("%s: cannot edit agent.%s in place", path, kv[0])
			}
			if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
				last = i
			}
		}
		if !found {
			lines = append(lines[:last+1], append([]string{kv[0] + " = " + quoted}, lines[last+1:]...)...)
		}
	}
	out := []byte(strings.Join(lines, "\n") + "\n")
	var check fileConfig
	if _, err := toml.NewDecoder(bytes.NewReader(out)).Decode(&check); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	got := map[string]*string{"provider": check.Agent.Provider, "model": check.Agent.Model, "effort": check.Agent.Effort}
	for _, kv := range keys {
		if v := got[kv[0]]; v == nil || *v != kv[1] {
			return fmt.Errorf("%s: cannot edit agent.%s in place", path, kv[0])
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := errors.Join(tmp.Chmod(mode), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
