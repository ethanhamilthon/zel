package luart

import (
	"context"
	"errors"
	"fmt"
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

// Config is the config VM: it ran the config files and keeps the original
// closures of the functions, hooks and commands. Sync hooks run in it; the
// session VM and the VMs of async hooks and commands get copies.
type Config struct {
	data    config.Config
	env     Env
	v       *vm
	pkgPath string
	funcs   map[string]*lua.LFunction
	hooks   []*lua.LFunction
	cmds    []*lua.LFunction
	// builtins are the globals of the VM: a function must not shadow them.
	builtins map[string]bool
	// The token serializes all use of the config VM, with cancellable acquisition.
	token   chan struct{}
	life    context.Context
	cancel  context.CancelFunc
	close   sync.Once
	asyncMu sync.Mutex // prevents adding a new hook after cancellation
	async   sync.WaitGroup
}

// packagePath is the package.path of every VM: the defaults module of the home,
// then the folder of each config file.
func packagePath(paths config.Paths, extra []string) string {
	var dirs []string
	if paths.Connect != "" {
		home := filepath.Dir(paths.Connect)
		dirs = append(dirs, filepath.Join(home, "defaults"))
	}
	for _, f := range paths.Files {
		dirs = append(dirs, filepath.Dir(f))
	}
	dirs = append(dirs, extra...)
	var parts []string
	seen := map[string]bool{}
	for i, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		parts = append(parts, filepath.Join(d, "?.lua"))
		if i == 0 && paths.Connect != "" {
			parts = append(parts, filepath.Join(d, "?", "init.lua"))
		}
	}
	return strings.Join(parts, ";")
}

// LoadConfig runs the config files of paths (and their loads) in a new config
// VM. A missing default file is skipped, a missing paths.Required file is an
// error. Everything the functions, hooks and commands captured must be
// copyable to another VM, or the config is rejected.
func LoadConfig(ctx context.Context, env Env, paths config.Paths, base config.Settings) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	life, stop := context.WithCancel(context.Background())
	c := &Config{
		data: config.Config{Paths: paths, Settings: base, Prompts: map[string]string{}, PromptSources: map[string]string{}},
		env:  env, token: make(chan struct{}, 1), life: life, cancel: stop, funcs: map[string]*lua.LFunction{},
	}
	c.pkgPath = packagePath(paths, nil)
	c.v = newVM(env, c.pkgPath)
	c.v.logPrint()
	c.v.settingsEnv = c.settingsEnv
	c.builtins = map[string]bool{}
	c.v.L.G.Global.ForEach(func(k, _ lua.LValue) { c.builtins[k.String()] = true })
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
		return c, nil
	case <-ctx.Done():
		go func() { <-done; c.Close() }()
		return nil, ctx.Err()
	}
}

func (c *Config) Data() *config.Config { return &c.data }

func (c *Config) settingsEnv() map[string]string { return c.data.Settings.Env }

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
		free := func() { c.v.close(); <-c.token }
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
	defer c.v.use(ctx)()
	for _, file := range paths.Files {
		if err := c.loadFile(file, false, slices.Contains(paths.Required, file)); err != nil {
			return err
		}
	}
	if err := c.checkCommandKeys(); err != nil {
		return err
	}
	if err := c.checkCopies(); err != nil {
		return err
	}
	return ctx.Err()
}

func (c *Config) addFile(path string) {
	if !slices.Contains(c.data.Files, path) {
		c.data.Files = append(c.data.Files, path)
	}
}

func (c *Config) loadFile(path string, nested, required bool) error {
	if err := c.v.ctx.Err(); err != nil {
		return err
	}
	file, _, err := openPlain(path, os.O_RDONLY, 0, false)
	if !required && !nested && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer file.Close()
	c.addFile(path)
	L := c.v.L
	if dir := filepath.Dir(path); nested && !strings.Contains(c.pkgPath, filepath.Join(dir, "?.lua")) {
		c.pkgPath += ";" + filepath.Join(dir, "?.lua")
		L.GetGlobal("package").(*lua.LTable).RawSetString("path", lua.LString(c.pkgPath))
	}
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
		if err := c.loadFile(p, true, true); err != nil {
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

// replaced is the warning of an entry that a later file redefines.
func (c *Config) replaced(path, what, old string) {
	c.data.Warnings = append(c.data.Warnings, fmt.Sprintf("%s: %s replaces a definition from %s", config.DisplayPath(path), what, config.DisplayPath(old)))
}

func (c *Config) merge(path string, value lua.LValue, nested bool) ([]string, error) {
	f, err := fields(value, "settings", "systems", "prompts", "functions", "hooks", "commands", "loads")
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
			if err := c.mergeEntry(path, section, name, entries[name]); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", section, name, err)
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
	if value := f["commands"]; value != nil {
		list, err := configList(value)
		if err != nil {
			return nil, fmt.Errorf("commands: %w", err)
		}
		for i, v := range list {
			if err := c.addCommand(path, v); err != nil {
				return nil, fmt.Errorf("commands[%d]: %w", i+1, err)
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

// luaFunction is a function a config entry may hold: a Lua function, because a
// Go function cannot be copied to another VM.
func luaFunction(v lua.LValue) (*lua.LFunction, bool) {
	fn, ok := v.(*lua.LFunction)
	return fn, ok && !fn.IsG
}

func (c *Config) mergeEntry(path, section, name string, value lua.LValue) error {
	what := section + "." + name
	switch section {
	case "functions":
		if c.builtins[name] {
			return errors.New("collides with a built-in")
		}
		f, err := fields(value, "description", "fn")
		if err != nil {
			return err
		}
		description, ok := f["description"].(lua.LString)
		if !ok {
			return errors.New("description must be a string")
		}
		fn, ok := luaFunction(f["fn"])
		if !ok {
			return errors.New("fn must be a Lua function")
		}
		for _, old := range c.data.Functions {
			if old.Name == name {
				c.replaced(path, what, old.Source)
			}
		}
		c.funcs[name] = fn
		c.data.Functions = append(slices.DeleteFunc(c.data.Functions, func(f config.Function) bool { return f.Name == name }),
			config.Function{Name: name, Description: string(description), Source: path})
		return nil
	case "prompts":
		s, ok := value.(lua.LString)
		if !ok {
			return errors.New("expected a string")
		}
		if old, ok := c.data.PromptSources[name]; ok {
			c.replaced(path, what, old)
		}
		c.data.Prompts[name], c.data.PromptSources[name] = string(s), path
		return nil
	}
	s, ok := value.(lua.LString)
	if !ok {
		return errors.New("expected a string")
	}
	for _, old := range c.data.Systems {
		if old.Name == name {
			c.replaced(path, what, old.Source)
		}
	}
	c.data.Systems = append(slices.DeleteFunc(c.data.Systems, func(s config.System) bool { return s.Name == name }),
		config.System{Name: name, Text: string(s), Source: path})
	return nil
}

func hookSource(path string, fn *lua.LFunction) string {
	if fn.Proto != nil {
		return fmt.Sprintf("%s:%d", strings.TrimPrefix(fn.Proto.SourceName, "@"), fn.Proto.LineDefined)
	}
	return path
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
	fn, ok := luaFunction(f["fn"])
	if !ok {
		return errors.New("fn must be a Lua function")
	}
	async := false
	if val, exists := f["async"]; exists {
		b, ok := val.(lua.LBool)
		if !ok {
			return errors.New("async must be a boolean")
		}
		async = bool(b)
	}
	c.data.Hooks = append(c.data.Hooks, config.Hook{Events: events, Async: async, Ref: len(c.hooks), Source: hookSource(path, fn)})
	c.hooks = append(c.hooks, fn)
	return nil
}

func (c *Config) addCommand(path string, value lua.LValue) error {
	f, err := fields(value, "name", "description", "key", "fn")
	if err != nil {
		return err
	}
	name, ok := f["name"].(lua.LString)
	if !ok || strings.TrimSpace(string(name)) == "" {
		return errors.New("name must be a nonempty string")
	}
	cmd := config.Command{Name: string(name), Source: path, Ref: len(c.cmds)}
	if v, ok := f["description"]; ok {
		s, isString := v.(lua.LString)
		if !isString {
			return errors.New("description must be a string")
		}
		cmd.Description = string(s)
	}
	if v, ok := f["key"]; ok {
		s, isString := v.(lua.LString)
		if !isString {
			return errors.New("key must be a string")
		}
		cmd.Key = strings.ToLower(strings.TrimSpace(string(s)))
		if cmd.Key != "" && !config.ValidKey(cmd.Key) {
			return errors.New("key: use ctrl+LETTER or alt+LETTER")
		}
	}
	fn, ok := luaFunction(f["fn"])
	if !ok {
		return errors.New("fn must be a Lua function")
	}
	c.cmds = append(c.cmds, fn)
	for i, old := range c.data.Commands {
		if old.Name == cmd.Name {
			c.replaced(path, "commands."+cmd.Name, old.Source)
			c.data.Commands[i] = cmd
			return nil
		}
	}
	c.data.Commands = append(c.data.Commands, cmd)
	return nil
}

// checkCommandKeys rejects a command key that clashes with settings.keys or
// with another command. It runs after all files, because a later file may set keys.
func (c *Config) checkCommandKeys() error {
	used := map[string]string{}
	for action, key := range c.data.Settings.Keys {
		if key != "" {
			used[key] = "keys." + action
		}
	}
	for _, cmd := range c.data.Commands {
		if cmd.Key == "" {
			continue
		}
		if other, ok := used[cmd.Key]; ok {
			return fmt.Errorf("commands.%s: key %s is already used by %s", cmd.Name, cmd.Key, other)
		}
		used[cmd.Key] = "commands." + cmd.Name
	}
	return nil
}

// checkCopies copies everything once into a scratch VM, so what cannot cross
// (a userdata upvalue, ...) is a config error now and not a surprise later.
func (c *Config) checkCopies() error {
	scratch := newVM(c.env, c.pkgPath)
	defer scratch.close()
	all := newCopier(c.v.L, scratch.L)
	for _, f := range c.data.Functions {
		if _, err := all.copyFunction(c.funcs[f.Name]); err != nil {
			return fmt.Errorf("functions.%s: %w", f.Name, err)
		}
	}
	for _, h := range c.data.Hooks {
		if !h.Async {
			continue
		}
		if _, err := newCopier(c.v.L, scratch.L).copyFunction(c.hooks[h.Ref]); err != nil {
			return fmt.Errorf("%s: %w", h.Source, err)
		}
	}
	for _, cmd := range c.data.Commands {
		if _, err := newCopier(c.v.L, scratch.L).copyFunction(c.cmds[cmd.Ref]); err != nil {
			return fmt.Errorf("commands.%s: %w", cmd.Name, err)
		}
	}
	return nil
}

// copyFunctions puts the functions of the config into the globals of v, copied
// in one pass: upvalues shared between functions stay shared in the copy.
func (c *Config) copyFunctions(ctx context.Context, v *vm) error {
	release, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cp := newCopier(c.v.L, v.L)
	for _, f := range c.data.Functions {
		fn, err := cp.copyFunction(c.funcs[f.Name])
		if err != nil {
			return fmt.Errorf("functions.%s: %w", f.Name, err)
		}
		v.L.SetGlobal(f.Name, fn)
	}
	return nil
}
