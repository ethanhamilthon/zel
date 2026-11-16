package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Layers lists the zel.toml files of a session in merge order: the global
// home/zel.toml (always, even if missing), ./zel.toml of cwd if it exists, then
// the extra (--config) files, kept even if missing. Duplicates are dropped.
func Layers(home, cwd string, extra []string) []Layer {
	var layers []Layer
	add := func(name, path string) {
		path = filepath.Clean(path)
		for _, l := range layers {
			if l.Path == path || sameFile(l.Path, path) {
				return
			}
		}
		layers = append(layers, Layer{name, path})
	}
	add("global", filepath.Join(home, "zel.toml"))
	if local := filepath.Join(cwd, "zel.toml"); fileExists(local) {
		add("local", local)
	}
	for _, p := range extra {
		if r, err := ResolvePath(p); err == nil {
			p = r
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		add("config", p)
	}
	return layers
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

var (
	completionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// layerConf is one validated layer, not yet merged or expanded.
type layerConf struct {
	settings     []setting
	providers    []named[Provider]
	env          []named[string]
	instructions []Instruction
	hooks        []Hook
	completions  []named[string]
}

type setting struct {
	key   string // Sources key
	apply func(*Settings)
}

type named[T any] struct {
	name string
	v    T
}

type envEntry struct{ path, name, value string }

// Load reads and merges the layers. Unreadable or invalid layers are skipped
// and reported in Errors. With a non-nil run, ${{ }} in env values (in merge
// order, each with the env known so far), provider, model, effort,
// settings.accent, provider fields and instruction texts is expanded with the
// Expander run returns. Load does not call Validate.
func Load(ctx context.Context, layers []Layer, run func(env map[string]string) Expander) *Config {
	cfg := &Config{Settings: DefaultSettings(), Hooks: map[string][]Hook{}, Completions: map[string]string{}, Sources: map[string]string{}}
	var env []envEntry // in order of first definition, with the last value
	envAt := map[string]int{}
	compFrom := map[string]string{}
	for _, l := range layers {
		lc, err := readLayer(l)
		if err != nil {
			cfg.Errors = append(cfg.Errors, err.Error())
			continue
		}
		if lc == nil {
			continue
		}
		cfg.Layers = append(cfg.Layers, l)
		for _, s := range lc.settings {
			s.apply(&cfg.Settings)
			cfg.Sources[s.key] = l.Path
		}
		for _, p := range lc.providers {
			cfg.Settings.Providers[p.name] = p.v
			cfg.Sources["providers."+p.name] = l.Path
		}
		for _, e := range lc.env {
			if i, ok := envAt[e.name]; ok {
				env[i] = envEntry{l.Path, e.name, e.v}
			} else {
				envAt[e.name] = len(env)
				env = append(env, envEntry{l.Path, e.name, e.v})
			}
			cfg.Sources["env."+e.name] = l.Path
		}
		cfg.Instructions = append(cfg.Instructions, lc.instructions...)
		for _, h := range lc.hooks {
			cfg.Hooks[h.Event] = append(cfg.Hooks[h.Event], h)
		}
		for _, c := range lc.completions {
			if prev, ok := compFrom[c.name]; ok {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("completion #%s of %s replaces the one of %s", c.name, l.Path, prev))
			}
			compFrom[c.name] = l.Path
			cfg.Completions[c.name] = c.v
		}
	}
	cfg.expand(ctx, env, run)
	return cfg
}

func (c *Config) expand(ctx context.Context, env []envEntry, run func(map[string]string) Expander) {
	s := &c.Settings
	do := func(exp Expander, path, key string, v *string) {
		out, err := Expand(ctx, *v, exp)
		if err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %s: %v", path, key, err))
		}
		*v = out
	}
	var exp Expander
	for _, e := range env {
		v := e.value
		if run != nil {
			do(run(maps.Clone(s.Env)), e.path, "env."+e.name, &v)
		}
		s.Env[e.name] = v
	}
	if run != nil {
		exp = run(maps.Clone(s.Env))
	}
	do(exp, c.Sources["provider"], "provider", &s.Provider)
	do(exp, c.Sources["model"], "model", &s.Model)
	do(exp, c.Sources["effort"], "effort", &s.Effort)
	do(exp, c.Sources["settings.accent"], "settings.accent", &s.UI.Accent)
	for _, name := range slices.Sorted(maps.Keys(s.Providers)) {
		p, path, key := s.Providers[name], c.Sources["providers."+name], "providers."+name
		do(exp, path, key+".base_url", &p.BaseURL)
		do(exp, path, key+".api", &p.API)
		do(exp, path, key+".api_key", &p.APIKey)
		s.Providers[name] = p
	}
	kept := c.Instructions[:0]
	for _, in := range c.Instructions {
		do(exp, in.Layer, "instructions", &in.Text)
		if strings.TrimSpace(in.Text) != "" {
			kept = append(kept, in)
		}
	}
	c.Instructions = kept
}

// readLayer returns nil, nil for a missing global layer.
func readLayer(l Layer) (*layerConf, error) {
	if _, err := os.Stat(l.Path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %v", l.Path, errors.Unwrap(err))
		}
		if l.Name == "global" {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: file not found", l.Path)
	}
	var data map[string]any
	md, err := toml.DecodeFile(l.Path, &data)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("%s:%d: %s", l.Path, pe.Position.Line, pe.Message)
		}
		return nil, fmt.Errorf("%s: %v", l.Path, err)
	}
	p := parser{md: &md, path: l.Path}
	lc, err := p.layer(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", l.Path, err)
	}
	return lc, nil
}

type parser struct {
	md   *toml.MetaData
	path string
}

// keys returns the keys of m (the table at prefix) in file order.
func (p parser) keys(m map[string]any, prefix ...string) []string {
	var out []string
	for _, k := range p.md.Keys() {
		if len(k) == len(prefix)+1 && slices.Equal(k[:len(prefix)], prefix) {
			if _, ok := m[k[len(prefix)]]; ok && !slices.Contains(out, k[len(prefix)]) {
				out = append(out, k[len(prefix)])
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

func (p parser) layer(data map[string]any) (*layerConf, error) {
	lc := &layerConf{}
	for _, k := range p.keys(data) {
		v := data[k]
		var err error
		switch k {
		case "provider", "model", "effort":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: must be a string", k)
			}
			lc.settings = append(lc.settings, setting{k, func(st *Settings) {
				switch k {
				case "provider":
					st.Provider = s
				case "model":
					st.Model = s
				default:
					st.Effort = s
				}
			}})
		case "settings":
			err = p.settings(lc, v)
		case "providers":
			err = p.providers(lc, v)
		case "env":
			err = p.strings(v, "env", envName, func(name, s string) { lc.env = append(lc.env, named[string]{name, s}) })
		case "completions":
			err = p.strings(v, "completions", completionName, func(name, s string) {
				lc.completions = append(lc.completions, named[string]{name, s})
			})
		case "instructions":
			err = p.instructions(lc, v)
		case "hooks":
			err = p.hooks(lc, v)
		default:
			err = fmt.Errorf("unknown key %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return lc, nil
}

func asTable(v any, where string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a table", where)
	}
	return m, nil
}

// asList accepts an array (of values or tables) or a single string.
func asList(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, m := range v {
			out[i] = m
		}
		return out, true
	case string:
		return []any{v}, true
	}
	return nil, false
}

func (p parser) settings(lc *layerConf, v any) error {
	m, err := asTable(v, "settings")
	if err != nil {
		return err
	}
	for _, k := range p.keys(m, "settings") {
		v, where := m[k], "settings."+k
		var apply func(*Settings)
		switch k {
		case "compaction", "bash_timeout":
			n, ok := v.(int64)
			if k == "compaction" && (!ok || n < 0) {
				return fmt.Errorf("%s: must be an integer >= 0", where)
			}
			if k == "bash_timeout" && (!ok || n <= 0) {
				return fmt.Errorf("%s: must be an integer > 0 (seconds)", where)
			}
			apply = func(s *Settings) {
				if k == "compaction" {
					s.CompactionThreshold = int(n)
				} else {
					s.BashTimeout = int(n)
				}
			}
		case "reasoning_summary", "slash_menu":
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("%s: must be true or false", where)
			}
			apply = func(s *Settings) {
				if k == "slash_menu" {
					s.UI.SlashMenu = b
				} else {
					s.ReasoningSummary = b
				}
			}
		case "accent":
			str, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s: must be a string", where)
			}
			apply = func(s *Settings) { s.UI.Accent = str }
		default:
			return fmt.Errorf("unknown key %q", where)
		}
		lc.settings = append(lc.settings, setting{where, apply})
	}
	return nil
}

func (p parser) providers(lc *layerConf, v any) error {
	m, err := asTable(v, "providers")
	if err != nil {
		return err
	}
	for _, name := range p.keys(m, "providers") {
		where := "providers." + name
		pm, err := asTable(m[name], where)
		if err != nil {
			return err
		}
		var pr Provider
		for _, k := range p.keys(pm, "providers", name) {
			s, ok := pm[k].(string)
			if !ok {
				return fmt.Errorf("%s.%s: must be a string", where, k)
			}
			switch k {
			case "base_url":
				pr.BaseURL = s
			case "api":
				if s != "" && s != APIResponses && s != APIChat && !strings.Contains(s, "${{") {
					return fmt.Errorf("%s.api: must be %q or %q", where, APIResponses, APIChat)
				}
				pr.API = s
			case "api_key":
				pr.APIKey = s
			default:
				return fmt.Errorf("unknown key %q", where+"."+k)
			}
		}
		if !strings.Contains(pr.BaseURL, "${{") {
			u, err := url.Parse(pr.BaseURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("%s.base_url: must be an http(s) URL", where)
			}
		}
		lc.providers = append(lc.providers, named[Provider]{name, pr})
	}
	return nil
}

// strings reads a table of strings whose names match re.
func (p parser) strings(v any, where string, re *regexp.Regexp, add func(name, s string)) error {
	m, err := asTable(v, where)
	if err != nil {
		return err
	}
	for _, k := range p.keys(m, where) {
		if !re.MatchString(k) {
			return fmt.Errorf("%s: invalid name %q", where, k)
		}
		s, ok := m[k].(string)
		if !ok {
			return fmt.Errorf("%s.%s: must be a string", where, k)
		}
		add(k, s)
	}
	return nil
}

// fields reads typed values of an inline table, remembering the first problem.
type fields struct {
	m     map[string]any
	where string
	err   error
}

func (f *fields) fail(k, msg string) {
	if f.err == nil {
		f.err = fmt.Errorf("%s.%s: %s", f.where, k, msg)
	}
}

func (f *fields) only(keys ...string) {
	for _, k := range slices.Sorted(maps.Keys(f.m)) {
		if !slices.Contains(keys, k) {
			f.fail(k, "unknown key")
		}
	}
}

func (f *fields) str(k string, required bool) string {
	v, ok := f.m[k]
	if !ok {
		if required {
			f.fail(k, "is required")
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		f.fail(k, "must be a string")
	}
	return s
}

func (f *fields) flag(k string, def bool) bool {
	v, ok := f.m[k]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		f.fail(k, "must be true or false")
	}
	return b
}

func (p parser) instructions(lc *layerConf, v any) error {
	list, ok := asList(v)
	if !ok {
		return fmt.Errorf("instructions: must be an array")
	}
	for i, item := range list {
		in := Instruction{Main: true, Layer: p.path}
		switch item := item.(type) {
		case string:
			in.Text = item
		case map[string]any:
			f := fields{m: item, where: fmt.Sprintf("instructions[%d]", i)}
			f.only("text", "main", "headless")
			in.Text, in.Main, in.Headless = f.str("text", true), f.flag("main", true), f.flag("headless", false)
			if f.err != nil {
				return f.err
			}
		default:
			return fmt.Errorf("instructions[%d]: must be a string or a table", i)
		}
		lc.instructions = append(lc.instructions, in)
	}
	return nil
}

func (p parser) hooks(lc *layerConf, v any) error {
	m, err := asTable(v, "hooks")
	if err != nil {
		return err
	}
	for _, event := range p.keys(m, "hooks") {
		if !slices.Contains(Events, event) {
			return fmt.Errorf("hooks: unknown event %q", event)
		}
		list, ok := asList(m[event])
		if !ok {
			return fmt.Errorf("hooks.%s: must be a string or an array", event)
		}
		for i, item := range list {
			h := Hook{Event: event, Main: true, Layer: p.path}
			where := fmt.Sprintf("hooks.%s[%d]", event, i)
			switch item := item.(type) {
			case string:
				h.Run = item
			case map[string]any:
				f := fields{m: item, where: where}
				f.only("run", "match", "timeout", "async", "main", "headless")
				h.Run, h.Match = f.str("run", true), f.str("match", false)
				h.Async, h.Main, h.Headless = f.flag("async", false), f.flag("main", true), f.flag("headless", false)
				if t, ok := item["timeout"]; ok {
					var sec float64
					switch t := t.(type) {
					case int64:
						sec = float64(t)
					case float64:
						sec = t
					default:
						f.fail("timeout", "must be a number of seconds")
					}
					if sec < 0 {
						f.fail("timeout", "must be >= 0")
					}
					h.Timeout = time.Duration(sec * float64(time.Second))
				}
				if f.err != nil {
					return f.err
				}
				if _, err := regexp.Compile(h.Match); err != nil {
					return fmt.Errorf("%s.match: %v", where, err)
				}
			default:
				return fmt.Errorf("%s: must be a string or a table", where)
			}
			if strings.TrimSpace(h.Run) == "" {
				return fmt.Errorf("%s: empty command", where)
			}
			lc.hooks = append(lc.hooks, h)
		}
	}
	return nil
}
