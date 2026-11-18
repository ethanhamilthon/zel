package luart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"zel/internal/config"
)

type Config struct {
	data     config.Config
	v        *vm
	pkg      *lua.LTable
	funcs    map[string]*lua.LFunction
	builtins map[string]bool
	hooks    []*lua.LFunction
	// The token serializes all calls to the config VM, with cancellable acquisition.
	token   chan struct{}
	life    context.Context
	cancel  context.CancelFunc
	close   sync.Once
	asyncMu sync.Mutex // prevents adding a new hook after cancellation
	async   sync.WaitGroup
}

func LoadConfig(ctx context.Context, env Env, paths config.Paths, base config.Settings) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	life, stop := context.WithCancel(context.Background())
	c := &Config{
		data:  config.Config{Paths: paths, Settings: base, Prompts: map[string]string{}},
		token: make(chan struct{}, 1), life: life, cancel: stop, funcs: map[string]*lua.LFunction{},
	}
	c.v = newVM(env, nil, false)
	c.pkg = c.v.L.GetGlobal("package").(*lua.LTable)
	c.builtins = map[string]bool{}
	for _, t := range []*lua.LTable{c.v.L.G.Global, c.v.api} {
		t.ForEach(func(k, v lua.LValue) { c.builtins[k.String()] = true })
	}
	c.v.loadingEnv = func() map[string]string { return c.data.Settings.Env }
	c.watchRequires()
	done := make(chan error, 1)
	go func() { done <- c.load(ctx, paths) }()
	select {
	case err := <-done:
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		c.v.cfg, c.v.loadingEnv = c, nil
		return c, nil
	case <-ctx.Done():
		go func() { <-done; c.Close() }()
		return nil, ctx.Err()
	}
}

func (c *Config) Data() *config.Config { return &c.data }

func (c *Config) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.life.Done():
		return nil, errors.New("config is closed")
	case c.token <- struct{}{}:
	}
	release := func() { <-c.token }
	if err := c.life.Err(); err != nil {
		release()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (c *Config) Close() {
	c.close.Do(func() {
		c.asyncMu.Lock()
		c.cancel()
		c.asyncMu.Unlock()
		// An uninterruptible native call may still own the VM. In that case
		// close it only once that call releases its token.
		free := func() { c.v.L.Close(); <-c.token }
		select {
		case c.token <- struct{}{}:
			free()
		default:
			go func() { c.token <- struct{}{}; free() }()
		}
		c.async.Wait()
	})
}

func (c *Config) load(ctx context.Context, paths config.Paths) error {
	defer c.v.use(ctx, io.Discard)()
	if paths.Connect != "" {
		c.watch(paths.Connect)
	}
	for _, file := range []string{paths.Init, paths.Local} {
		if file == "" {
			continue
		}
		if err := c.loadFile(file, false); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Record modules loaded by require during the snapshot build, too.
func (c *Config) watchRequires() {
	L := c.v.L
	loaders := c.pkg.RawGetString("loaders").(*lua.LTable)
	loader := loaders.RawGetInt(2)
	loaders.RawSetInt(2, L.NewFunction(func(L *lua.LState) int {
		L.Push(loader)
		L.Push(L.Get(1))
		L.Call(1, 1)
		if c.v.loadingEnv != nil {
			if fn, ok := L.Get(-1).(*lua.LFunction); ok && fn.Proto != nil {
				path := fn.Proto.SourceName
				if abs, err := filepath.Abs(path); err == nil {
					c.watch(abs)
				}
			}
		}
		return 1
	}))
}

func (c *Config) watch(path string) {
	if !slices.Contains(c.data.Files, path) {
		c.data.Files = append(c.data.Files, path)
	}
}

func (c *Config) loadFile(path string, nested bool) error {
	if err := c.v.ctx.Err(); err != nil {
		return err
	}
	c.watch(path)
	file, _, err := openPlain(path, os.O_RDONLY, 0, false)
	if !nested && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer file.Close()
	L := c.v.L
	current := c.pkg.RawGetString("path").String()
	c.pkg.RawSetString("path", lua.LString(filepath.Join(filepath.Dir(path), "?.lua")+";"+current))
	fn, err := L.Load(file, "@"+path)
	if err != nil {
		return fmt.Errorf("%s", errorMessage(err))
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		return fmt.Errorf("%s: %s", path, errorMessage(err))
	}
	table, ok := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	if !ok {
		return fmt.Errorf("%s: expected a returned table", path)
	}
	loads, err := c.merge(path, table, nested)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range loads {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		if err := c.loadFile(p, true); err != nil {
			return err
		}
	}
	return nil
}

var configName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func fields(value lua.LValue, allowed ...string) (map[string]lua.LValue, error) {
	t, ok := value.(*lua.LTable)
	if !ok {
		return nil, errors.New("expected a table")
	}
	out := make(map[string]lua.LValue)
	var err error
	t.ForEach(func(k, v lua.LValue) {
		if err != nil {
			return
		}
		s, ok := k.(lua.LString)
		if !ok {
			err = errors.New("expected string keys")
			return
		}
		key := string(s)
		if len(allowed) > 0 && !slices.Contains(allowed, key) {
			err = fmt.Errorf("unknown key %q", key)
			return
		}
		out[key] = v
	})
	return out, err
}

func configList(value lua.LValue) ([]lua.LValue, error) {
	t, ok := value.(*lua.LTable)
	if !ok || !isArray(t) {
		return nil, errors.New("expected a list")
	}
	out := make([]lua.LValue, t.Len())
	for i := range out {
		out[i] = t.RawGetInt(i + 1)
	}
	return out, nil
}

func configStrings(value lua.LValue) ([]string, error) {
	list, err := configList(value)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(list))
	for i, v := range list {
		s, ok := v.(lua.LString)
		if !ok || s == "" {
			return nil, fmt.Errorf("item %d: expected a nonempty string", i+1)
		}
		out[i] = string(s)
	}
	return out, nil
}

func settingsMap(value lua.LValue, depth int) (map[string]any, error) {
	if depth > maxDepth {
		return nil, errors.New("settings nested too deeply or cyclic")
	}
	keys, err := fields(value)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(keys))
	for _, k := range sortedKeys(keys) {
		v := keys[k]
		if _, ok := v.(*lua.LTable); ok {
			out[k], err = settingsMap(v, depth+1)
		} else {
			out[k], err = toGo(v)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	return out, nil
}

func (c *Config) merge(path string, value lua.LValue, nested bool) ([]string, error) {
	f, err := fields(value, "settings", "systems", "prompts", "functions", "hooks", "loads")
	if err != nil {
		return nil, err
	}
	if value := f["settings"]; value != nil {
		raw, err := settingsMap(value, 0)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		c.data.Settings, err = config.ParseSettings(raw, c.data.Settings)
		if err != nil {
			return nil, err
		}
	}
	for _, section := range []string{"systems", "prompts", "functions"} {
		value := f[section]
		if value == nil {
			continue
		}
		entries, err := fields(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", section, err)
		}
		for _, name := range sortedKeys(entries) {
			if !configName.MatchString(name) {
				return nil, fmt.Errorf("%s: invalid name %q", section, name)
			}
			replace, err := c.mergeEntry(section, name, entries[name])
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", section, name, err)
			}
			if replace {
				c.data.Warnings = append(c.data.Warnings, fmt.Sprintf("%s: %s.%s replaces an earlier definition", path, section, name))
			}
		}
	}
	if value := f["hooks"]; value != nil {
		list, err := configList(value)
		if err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
		for i, v := range list {
			if err := c.addHook(path, v); err != nil {
				return nil, fmt.Errorf("hooks[%d]: %w", i+1, err)
			}
		}
	}
	if value := f["loads"]; value != nil {
		if nested {
			return nil, errors.New("loads are not allowed in a loaded file")
		}
		loads, err := configStrings(value)
		if err != nil {
			return nil, fmt.Errorf("loads: %w", err)
		}
		return loads, nil
	}
	return nil, nil
}

func (c *Config) mergeEntry(section, name string, value lua.LValue) (bool, error) {
	if section == "functions" {
		if c.builtins[name] {
			return false, errors.New("collides with a built-in")
		}
		f, err := fields(value, "description", "fn")
		if err != nil {
			return false, err
		}
		description, ok := f["description"].(lua.LString)
		if !ok {
			return false, errors.New("description must be a string")
		}
		fn, ok := f["fn"].(*lua.LFunction)
		if !ok {
			return false, errors.New("fn must be a function")
		}
		_, replaced := c.funcs[name]
		c.funcs[name] = fn
		c.data.Functions = append(slices.DeleteFunc(c.data.Functions, func(f config.Function) bool { return f.Name == name }), config.Function{Name: name, Description: string(description)})
		return replaced, nil
	}
	s, ok := value.(lua.LString)
	if !ok {
		return false, errors.New("expected a string")
	}
	if section == "prompts" {
		_, replaced := c.data.Prompts[name]
		c.data.Prompts[name] = string(s)
		return replaced, nil
	}
	n := len(c.data.Systems)
	c.data.Systems = slices.DeleteFunc(c.data.Systems, func(s config.System) bool { return s.Name == name })
	replaced := len(c.data.Systems) != n
	c.data.Systems = append(c.data.Systems, config.System{Name: name, Text: string(s)})
	return replaced, nil
}

func (c *Config) addHook(path string, value lua.LValue) error {
	f, err := fields(value, "events", "fn", "async")
	if err != nil {
		return err
	}
	events, err := configStrings(f["events"])
	if err != nil || len(events) == 0 {
		return errors.New("events must be a nonempty list of event names")
	}
	for _, event := range events {
		if !slices.Contains(config.Events, event) {
			return fmt.Errorf("unknown event %q", event)
		}
	}
	fn, ok := f["fn"].(*lua.LFunction)
	if !ok {
		return errors.New("fn must be a function")
	}
	async := false
	if val, exists := f["async"]; exists {
		b, ok := val.(lua.LBool)
		if !ok {
			return errors.New("async must be a boolean")
		}
		async = bool(b)
	}
	source := path
	if fn.Proto != nil {
		source = fmt.Sprintf("%s:%d", strings.TrimPrefix(fn.Proto.SourceName, "@"), fn.Proto.LineDefined)
	}
	c.data.Hooks = append(c.data.Hooks, config.Hook{Events: events, Async: async, Ref: len(c.hooks), Source: source})
	c.hooks = append(c.hooks, fn)
	return nil
}
