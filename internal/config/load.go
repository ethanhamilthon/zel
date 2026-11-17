package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	entryName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// ModeName is what a mode may be called: in modes = [...] and in --mode.
	ModeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
	return &Config{Path: path, Settings: DefaultSettings(), Hooks: map[string][]Hook{}, Completions: map[string]Completion{}}
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
	do(exp, "model", &s.Model)
	c.splitModel()
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
		p, key := s.Providers[name], "provider."+name
		do(exp, key+".base_url", &p.BaseURL)
		do(exp, key+".api", &p.API)
		do(exp, key+".api_key", &p.APIKey)
		s.Providers[name] = p
	}
	kept := c.Instructions[:0]
	for _, in := range c.Instructions {
		do(exp, "instruction."+in.Name, &in.Text)
		if strings.TrimSpace(in.Text) != "" {
			kept = append(kept, in)
		}
	}
	c.Instructions = kept
}

// splitModel turns model = "PROVIDER/MODEL" into the provider and the model.
// The model ID may contain slashes itself; the provider name cannot. Without a
// slash the only configured provider is meant.
func (c *Config) splitModel() {
	s := &c.Settings
	if s.Model == "" {
		return
	}
	if prov, model, ok := strings.Cut(s.Model, "/"); ok && prov != "" {
		s.Provider, s.Model = prov, model
		return
	}
	if len(s.Providers) == 1 {
		for name := range s.Providers {
			s.Provider = name
		}
		return
	}
	c.Warnings = append(c.Warnings, fmt.Sprintf("model: %q must be PROVIDER/MODEL", s.Model))
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
			return nil, nil, errors.New(parseError(path, pe))
		}
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	p := &parser{md: &md, cfg: newConfig(path)}
	if err := p.file(data); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", path, err)
	}
	return p.cfg, p.env, nil
}

// parseError is "PATH:LINE:COL: message" with the line and a caret under the
// error, so a broken file shows where it is broken.
func parseError(path string, pe toml.ParseError) string {
	pos := pe.Position
	msg := fmt.Sprintf("%s:%d:%d: %s", path, pos.Line, max(pos.Col, 1), strings.TrimSuffix(pe.Message, "."))
	data, err := os.ReadFile(path)
	lines := strings.Split(string(data), "\n")
	if err != nil || pos.Line < 1 || pos.Line > len(lines) {
		return msg
	}
	line := strings.ReplaceAll(strings.TrimRight(lines[pos.Line-1], "\r"), "\t", " ")
	col := min(max(pos.Col, 1), len(line)+1)
	width := min(max(pos.Len, 1), max(len(line)-col+1, 1))
	num := fmt.Sprint(pos.Line)
	pad := strings.Repeat(" ", len(num))
	return fmt.Sprintf("%s\n %s | %s\n %s | %s%s", msg, num, line, pad, strings.Repeat(" ", col-1), strings.Repeat("^", width))
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

// renamed are the keys of the old (0.9.1) format, with what replaced them.
var renamed = map[string]string{
	"providers":    "[provider.NAME]",
	"instructions": "[instruction.NAME] with text = ...",
	"hooks":        "[hook.NAME] with event = ... and run = ...",
	"completions":  "[completion.NAME] with text = ...",
}

func (p *parser) file(data map[string]any) error {
	for _, k := range p.keys(data) {
		v := data[k]
		var err error
		switch k {
		case "model", "effort":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s: must be a string", k)
			}
			if k == "effort" {
				p.cfg.Settings.Effort = s
			} else {
				p.cfg.Settings.Model = s
			}
		case "settings":
			err = p.settings(v)
		case "colors":
			err = p.colors(v)
		case "provider":
			if _, ok := v.(string); ok {
				return fmt.Errorf(`provider: the selected model is one key now, model = "PROVIDER/MODEL"; [provider.NAME] configures a provider`)
			}
			err = p.providers(v)
		case "env":
			err = p.strings(v, "env", envName, func(name, s string) { p.env = append(p.env, named[string]{name, s}) })
		case "completion":
			err = p.completions(v)
		case "instruction":
			err = p.instructions(v)
		case "hook":
			err = p.hooks(v)
		default:
			if now, ok := renamed[k]; ok {
				return fmt.Errorf("%s: this is %s now (zel docs config)", k, now)
			}
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
		case "editor":
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s: must be a command", where)
			}
			st.Editor = s
		case "show_bash", "trace_hooks":
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("%s: must be true or false", where)
			}
			if k == "show_bash" {
				st.ShowBash = b
			} else {
				st.TraceHooks = b
			}
		default:
			return fmt.Errorf("unknown key %q", where)
		}
	}
	return nil
}

func (p *parser) providers(v any) error {
	m, err := asTable(v, "provider")
	if err != nil {
		return err
	}
	for _, name := range p.keys(m, "provider") {
		where := "provider." + name
		if !entryName.MatchString(name) {
			return fmt.Errorf("provider: invalid name %q", name)
		}
		pm, err := asTable(m[name], where)
		if err != nil {
			return err
		}
		var pr Provider
		for _, k := range p.keys(pm, "provider", name) {
			if k == "modes" {
				f := fields{m: pm, where: where}
				pr.Modes = f.modes(nil)
				if f.err != nil {
					return f.err
				}
				continue
			}
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

// modes reads a list of mode names; def when the key is missing.
func (f *fields) modes(def []string) []string {
	v, ok := f.m["modes"]
	if !ok {
		return def
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		f.fail("modes", "must be a non-empty list of mode names")
		return def
	}
	var out []string
	for _, x := range list {
		s, ok := x.(string)
		if !ok || !ModeName.MatchString(s) {
			f.fail("modes", fmt.Sprintf("invalid mode %v", x))
			return def
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
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

// entries reads a [kind.NAME] map in file order. A string value is the table
// {short = value}, when the kind has a shorthand.
func (p *parser) entries(v any, kind, short string, add func(f *fields, name string) error) error {
	m, err := asTable(v, kind)
	if err != nil {
		return err
	}
	for _, name := range p.keys(m, kind) {
		where := kind + "." + name
		if !entryName.MatchString(name) {
			return fmt.Errorf("%s: invalid name %q", kind, name)
		}
		item := m[name]
		if s, ok := item.(string); ok && short != "" {
			item = map[string]any{short: s}
		}
		t, ok := item.(map[string]any)
		if !ok {
			if short != "" {
				return fmt.Errorf("%s: must be a string or a table", where)
			}
			return fmt.Errorf("%s: must be a table", where)
		}
		if err := add(&fields{m: t, where: where}, name); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) instructions(v any) error {
	return p.entries(v, "instruction", "text", func(f *fields, name string) error {
		f.only("text", "modes")
		in := Instruction{Name: name, Text: f.str("text", true), Modes: f.modes([]string{ModeMain})}
		if f.err != nil {
			return f.err
		}
		p.cfg.Instructions = append(p.cfg.Instructions, in)
		return nil
	})
}

func (p *parser) completions(v any) error {
	return p.entries(v, "completion", "text", func(f *fields, name string) error {
		f.only("text", "modes")
		c := Completion{Text: f.str("text", true), Modes: f.modes([]string{ModeMain})}
		if f.err != nil {
			return f.err
		}
		p.cfg.Completions[name] = c
		return nil
	})
}

func (p *parser) hooks(v any) error {
	return p.entries(v, "hook", "", func(f *fields, name string) error {
		f.only("event", "run", "match", "timeout", "async", "modes")
		h := Hook{Name: name, Run: f.str("run", true), Match: f.str("match", false), Async: f.flag("async", false), Modes: f.modes([]string{ModeMain})}
		var events []string
		switch e := f.m["event"].(type) {
		case string:
			events = []string{e}
		case []any:
			for _, x := range e {
				s, ok := x.(string)
				if !ok {
					f.fail("event", "must be an event or a list of events")
				}
				events = append(events, s)
			}
		case nil:
			f.fail("event", "is required")
		default:
			f.fail("event", "must be an event or a list of events")
		}
		if t, ok := f.m["timeout"]; ok {
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
		if len(events) == 0 {
			return fmt.Errorf("%s.event: is required", f.where)
		}
		if strings.TrimSpace(h.Run) == "" {
			return fmt.Errorf("%s.run: empty command", f.where)
		}
		if _, err := regexp.Compile(h.Match); err != nil {
			return fmt.Errorf("%s.match: %v", f.where, err)
		}
		for _, event := range events {
			if !slices.Contains(Events, event) {
				return fmt.Errorf("%s.event: unknown event %q", f.where, event)
			}
			if _, ok := MatchField[event]; h.Match != "" && !ok {
				return fmt.Errorf("%s.match: only message, notification, turn_start, bash_before and bash_after have text to match", f.where)
			}
			e := h
			e.Event = event
			p.cfg.Hooks[event] = append(p.cfg.Hooks[event], e)
		}
		return nil
	})
}
