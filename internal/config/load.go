package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	completionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type named[T any] struct {
	name string
	v    T
}

// Load reads the config file at path. An unreadable or invalid file gives the
// defaults and a message in Errors. With a non-nil run, ${{ }} in env values
// (each with the env known so far), provider, model, effort, colors, provider
// fields and instruction texts is expanded with the Expander run returns. A
// color that is not valid after expansion keeps its default, with a warning.
// Load does not call Validate.
func Load(ctx context.Context, path string, run func(env map[string]string) Expander) *Config {
	cfg, env, err := readFile(path)
	if err != nil {
		cfg = newConfig(path)
		cfg.Errors = append(cfg.Errors, err.Error())
		return cfg
	}
	cfg.expand(ctx, env, run)
	return cfg
}

func newConfig(path string) *Config {
	return &Config{Path: path, Settings: DefaultSettings(), Hooks: map[string][]Hook{}, Completions: map[string]string{}}
}

func (c *Config) expand(ctx context.Context, env []named[string], run func(map[string]string) Expander) {
	s := &c.Settings
	do := func(exp Expander, key string, v *string) {
		out, err := Expand(ctx, *v, exp)
		if err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %v", key, err))
		}
		*v = out
	}
	var exp Expander
	for _, e := range env {
		v := e.v
		if run != nil {
			do(run(maps.Clone(s.Env)), "env."+e.name, &v)
		}
		s.Env[e.name] = v
	}
	if run != nil {
		exp = run(maps.Clone(s.Env))
	}
	do(exp, "provider", &s.Provider)
	do(exp, "model", &s.Model)
	do(exp, "effort", &s.Effort)
	defaults := DefaultColors()
	for i, key := range ColorKeys {
		if run == nil || !strings.Contains(s.Colors[i], "${{") {
			continue
		}
		do(exp, "colors."+key, &s.Colors[i])
		if err := CheckColor(key, s.Colors[i]); err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%v; the default stays", err))
			s.Colors[i] = defaults[i]
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Providers)) {
		p, key := s.Providers[name], "providers."+name
		do(exp, key+".base_url", &p.BaseURL)
		do(exp, key+".api", &p.API)
		do(exp, key+".api_key", &p.APIKey)
		s.Providers[name] = p
	}
	kept := c.Instructions[:0]
	for _, in := range c.Instructions {
		do(exp, "instructions", &in.Text)
		if strings.TrimSpace(in.Text) != "" {
			kept = append(kept, in)
		}
	}
	c.Instructions = kept
}

// readFile parses and validates the file. The env values come back in file
// order, not yet expanded.
func readFile(path string) (*Config, []named[string], error) {
	var data map[string]any
	md, err := toml.DecodeFile(path, &data)
	if err != nil {
		var pe toml.ParseError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil, fmt.Errorf("%s: file not found", path)
		case errors.As(err, &pe):
			return nil, nil, fmt.Errorf("%s:%d: %s", path, pe.Position.Line, pe.Message)
		}
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	p := &parser{md: &md, cfg: newConfig(path)}
	if err := p.file(data); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	return p.cfg, p.env, nil
}

type parser struct {
	md  *toml.MetaData
	cfg *Config
	env []named[string]
}

// keys returns the keys of m (the table at prefix) in file order.
func (p *parser) keys(m map[string]any, prefix ...string) []string {
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

func (p *parser) file(data map[string]any) error {
	for _, k := range p.keys(data) {
		v := data[k]
		var err error
		switch k {
		case "provider", "model", "effort":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s: must be a string", k)
			}
			switch k {
			case "provider":
				p.cfg.Settings.Provider = s
			case "model":
				p.cfg.Settings.Model = s
			default:
				p.cfg.Settings.Effort = s
			}
		case "settings":
			err = p.settings(v)
		case "colors":
			err = p.colors(v)
		case "providers":
			err = p.providers(v)
		case "env":
			err = p.strings(v, "env", envName, func(name, s string) { p.env = append(p.env, named[string]{name, s}) })
		case "completions":
			err = p.strings(v, "completions", completionName, func(name, s string) {
				p.cfg.Completions[name] = s
			})
		case "instructions":
			err = p.instructions(v)
		case "hooks":
			err = p.hooks(v)
		default:
			err = fmt.Errorf("unknown key %q", k)
		}
		if err != nil {
			return err
		}
	}
	return nil
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

func (p *parser) settings(v any) error {
	m, err := asTable(v, "settings")
	if err != nil {
		return err
	}
	st := &p.cfg.Settings
	for _, k := range p.keys(m, "settings") {
		v, where := m[k], "settings."+k
		switch k {
		case "compaction", "bash_timeout", "bash_output":
			n, ok := v.(int64)
			switch {
			case k == "compaction" && (!ok || n < 0):
				return fmt.Errorf("%s: must be an integer >= 0", where)
			case k == "bash_timeout" && (!ok || n <= 0):
				return fmt.Errorf("%s: must be an integer > 0 (seconds)", where)
			case k == "bash_output" && (!ok || n < 0 || (n > 0 && n < 1000)):
				return fmt.Errorf("%s: must be 0 (no cut) or an integer >= 1000 (bytes)", where)
			}
			switch k {
			case "compaction":
				st.CompactionThreshold = int(n)
			case "bash_timeout":
				st.BashTimeout = int(n)
			default:
				st.BashOutput = int(n)
			}
		case "show_bash":
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("%s: must be true or false", where)
			}
			st.ShowBash = b
		default:
			return fmt.Errorf("unknown key %q", where)
		}
	}
	return nil
}

func (p *parser) providers(v any) error {
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
		p.cfg.Settings.Providers[name] = pr
	}
	return nil
}

// strings reads a table of strings whose names match re.
func (p *parser) strings(v any, where string, re *regexp.Regexp, add func(name, s string)) error {
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

func (p *parser) instructions(v any) error {
	list, ok := asList(v)
	if !ok {
		return fmt.Errorf("instructions: must be an array")
	}
	for i, item := range list {
		in := Instruction{Main: true}
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
		p.cfg.Instructions = append(p.cfg.Instructions, in)
	}
	return nil
}

func (p *parser) hooks(v any) error {
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
			h := Hook{Event: event, Main: true}
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
			p.cfg.Hooks[event] = append(p.cfg.Hooks[event], h)
		}
	}
	return nil
}
