package core

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"zel/internal/luart"
)

//go:embed lua.md
var luaMD string

// LuaToolDescription is the description of the one tool the model gets.
var LuaToolDescription = strings.TrimSpace(luaMD)

// luaHolder keeps the config VM of init.lua layers. It is shared by pointer so a
// session switch and Reload see the same value. A snapshot that a running subagent
// still owes lifecycle hooks is pinned: when it is replaced it stays open until the
// last pin is released, so the subagent's end hook reaches the snapshot that ran
// its start hook.
type luaHolder struct {
	mu     sync.RWMutex
	cfg    *luart.Config
	pins   map[*luart.Config]int
	root   string
	notice func(string)
	models modelListing
}

// modelListing is the session's list of provider models for session.models().
// mu guards the fields; it is never held while providers are asked.
type modelListing struct {
	mu       sync.Mutex
	key      string // the providers the list was made for
	ready    bool
	choices  []ModelChoice
	failures map[string]string
	flight   chan struct{} // closed when the listing in progress ends
}

func (h *luaHolder) get() *luart.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// set makes cfg the current snapshot. The one it replaces closes now, unless
// pinned; the last unpin closes it then.
func (h *luaHolder) set(cfg *luart.Config) {
	h.mu.Lock()
	old := h.cfg
	h.cfg = cfg
	closeNow := old != nil && h.pins[old] == 0
	h.mu.Unlock()
	if closeNow {
		old.Close()
	}
}

// pin takes a reference on the current snapshot (nil when none is loaded).
func (h *luaHolder) pin() *luart.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg != nil {
		if h.pins == nil {
			h.pins = map[*luart.Config]int{}
		}
		h.pins[h.cfg]++
	}
	return h.cfg
}

// unpin releases a reference taken by pin. A snapshot that was replaced in the
// meantime closes with its last reference.
func (h *luaHolder) unpin(cfg *luart.Config) {
	if cfg == nil {
		return
	}
	h.mu.Lock()
	h.pins[cfg]--
	closeNow := h.pins[cfg] <= 0 && cfg != h.cfg
	if h.pins[cfg] <= 0 {
		delete(h.pins, cfg)
	}
	h.mu.Unlock()
	if closeNow {
		cfg.Close()
	}
}

// luaSources are the init.lua files of this session, in load order.
func (a *Runtime) luaSources() []luart.Source {
	var sources []luart.Source
	for _, l := range LuaLayers(a.home, a.Shell.Dir) {
		sources = append(sources, luart.Source{Name: l.Name, Path: l.Path})
	}
	return sources
}

// loadLua runs the init.lua layers into a fresh config VM: the snapshot of this
// session. A layer that fails to load is skipped with a notice.
func (a *Runtime) loadLua() {
	a.lua.set(luart.LoadConfig(luaEnv{a}, a.luaSources()))
}

// reloadLua builds a new config VM for Reload. If a layer fails to load the old
// snapshot stays, so a typo in init.lua cannot take the plugins away mid-session.
func (a *Runtime) reloadLua() {
	next := luart.LoadConfig(luaEnv{a}, a.luaSources())
	if old := a.lua.get(); old != nil && len(next.LoadErrs) > 0 {
		next.Close()
		a.notify("Reload failed: the previous init.lua snapshot stays")
		return
	}
	a.lua.set(next)
}

// LuaDefaults returns the merged zel.defaults.
func (a *Runtime) LuaDefaults() luart.Defaults {
	if cfg := a.lua.get(); cfg != nil {
		return cfg.Defaults()
	}
	return luart.Defaults{}
}

// SetNoticeHandler routes notices raised from Lua (log.notice, load errors) to the UI.
func (a *Runtime) SetNoticeHandler(fn func(string)) {
	a.lua.mu.Lock()
	a.lua.notice = fn
	a.lua.mu.Unlock()
}

func (a *Runtime) notify(msg string) {
	a.LogEvent("notice", msg)
	a.lua.mu.RLock()
	fn := a.lua.notice
	a.lua.mu.RUnlock()
	if fn != nil {
		fn(msg)
	}
}

// runLua executes one lua tool call.
func (a *Runtime) runLua(ctx context.Context, code string) luart.Result {
	if a.vm == nil {
		a.vm = &luart.Runner{Env: luaEnv{a}}
	}
	a.vm.Cfg = a.lua.get()
	return a.vm.Run(ctx, code)
}

// luaEnv adapts the runtime to the host API of the Lua runtime.
type luaEnv struct{ a *Runtime }

func (e luaEnv) Info() luart.SessionInfo {
	a := e.a
	s := a.Settings()
	cwd := a.Shell.Dir
	info := luart.SessionInfo{ID: a.id, Cwd: cwd, Root: a.projectRoot(), Parent: a.parentRoot, Depth: a.depth,
		Model: s.Model, Effort: s.Effort, Home: a.home, TmpDir: filepath.Join(os.TempDir(), "zel-"+a.id)}
	info.LuaPath = []string{filepath.Join(a.home, "lua")}
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".zel")); err == nil {
			info.LuaPath = append(info.LuaPath, filepath.Join(d, ".zel", "lua"))
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return info
}

// projectRoot is the git root of the session directory, else the directory itself.
func (a *Runtime) projectRoot() string {
	h := a.lua
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.root == "" {
		h.root = a.Shell.Dir
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		cmd.Dir = a.Shell.Dir
		if out, err := cmd.Output(); err == nil {
			if root := strings.TrimSpace(string(out)); root != "" {
				h.root = root
			}
		}
	}
	return h.root
}

func (e luaEnv) ShellEnv() map[string]string {
	return e.a.shellEnv()
}

func (e luaEnv) State() luart.StateStore { return kvState{e.a} }

// View attaches an image to the next model request, like `zel view`.
func (e luaEnv) View(path string) error {
	a := e.a
	image, err := CaptureImageCopy(path, filepath.Join(a.home, "attachments"))
	if err != nil {
		return err
	}
	if !a.slots.reserve(1) {
		return fmt.Errorf("at most %d new images per model request", MaxPendingImages)
	}
	if !a.queue.Push(withImages(notification("view: "+filepath.Base(path)+" attached"), image)) {
		a.slots.release(1)
		return fmt.Errorf("input queue is full")
	}
	return nil
}

// Models lists the models of the providers, as the model menu does. The list is
// made once per session, failures included, and again only after the providers
// change. One listing runs at a time and never under the lock: other callers wait
// for it or for their own ctx, whichever ends first. A cancelled listing is not
// kept; the caller reports the cancelled call.
func (e luaEnv) Models(ctx context.Context) ([]luart.ModelInfo, map[string]string) {
	a := e.a
	settings := a.Settings()
	key := fmt.Sprint(settings.Providers)
	m := &a.lua.models
	for ctx.Err() == nil {
		m.mu.Lock()
		if m.ready && m.key == key {
			choices, failures := m.choices, m.failures
			m.mu.Unlock()
			models := make([]luart.ModelInfo, 0, len(choices))
			for _, c := range choices {
				models = append(models, luart.ModelInfo{Provider: c.Provider, Model: c.Model, ContextWindow: c.ContextWindow})
			}
			return models, failures
		}
		if running := m.flight; running != nil {
			m.mu.Unlock()
			select {
			case <-running:
			case <-ctx.Done():
			}
			continue
		}
		done := make(chan struct{})
		m.flight = done
		m.mu.Unlock()

		choices, errs := AllModels(ctx, settings.Providers)
		failures := map[string]string{}
		for name, err := range errs {
			failures[name] = Redact(settings, err.Error())
		}
		m.mu.Lock()
		if ctx.Err() == nil {
			m.key, m.ready, m.choices, m.failures = key, true, choices, failures
		}
		m.flight = nil
		close(done)
		m.mu.Unlock()
	}
	return nil, nil
}

// Begin reports a foreground host operation; background work is not shown.
func (e luaEnv) Begin(label string, background bool) (end func()) {
	if background {
		return func() {}
	}
	// Labels come from arguments, so configured API keys are removed here too.
	return e.a.beginActivity(Redact(e.a.Settings(), label))
}

func (e luaEnv) Notice(msg string) { e.a.notify(msg) }

func (e luaEnv) Log(level, msg string) { e.a.LogEvent("lua-"+level, msg) }

func (e luaEnv) Rename(name string) error { return e.a.Rename(name) }

// kvState stores Lua state in the database: "session" per session, "project" per git root.
type kvState struct{ a *Runtime }

func (s kvState) scope(scope string) string {
	if scope == "project" {
		return "project:" + s.a.projectRoot()
	}
	return "session:" + s.a.id
}

func (s kvState) Get(scope, key string) (string, bool, error) {
	return s.a.db.KVGet(s.scope(scope), key)
}
func (s kvState) Set(scope, key, value string) error { return s.a.db.KVSet(s.scope(scope), key, value) }
func (s kvState) Delete(scope, key string) error     { return s.a.db.KVDelete(s.scope(scope), key) }
func (s kvState) List(scope, prefix string) ([]string, error) {
	return s.a.db.KVList(s.scope(scope), prefix)
}
func (s kvState) Size(scope string) (int, error) { return s.a.db.KVSize(s.scope(scope)) }

// sessionStart runs the session_start hooks once per started or resumed
// session; text they return is queued as a notification.
func (a *Runtime) sessionStart() {
	if cfg := a.lua.get(); cfg != nil && cfg.HasHook("session_start") {
		if text := cfg.SessionStart(context.Background()); text != "" {
			a.queue.PushNotice(notification(text))
		}
	}
}

// sessionEnd leaves the session: its jobs are cancelled, their end hooks run, and then
// the session_end hooks run with what is left of the shutdown budget (see tasks.close).
func (a *Runtime) sessionEnd(reason string) {
	t := a.tasks
	if t == nil {
		return
	}
	t.close(func(ctx context.Context) {
		if cfg := a.lua.get(); cfg != nil && cfg.HasHook("session_end") {
			cfg.SessionEnd(ctx, reason)
		}
	})
}

// hookInput lets message hooks observe notifications or rewrite what the user typed.
func (a *Runtime) hookInput(ctx context.Context, input Input) Input {
	cfg := a.lua.get()
	if cfg == nil || !cfg.HasHook("message") || input.Text == "" {
		return input
	}
	if input.Notice {
		cfg.Notice(ctx, input.Text)
		return input
	}
	input.Text = cfg.Message(ctx, input.Text)
	return input
}

// turnEnd runs the turn_end hooks; returned text becomes a notification that continues the turn.
func (a *Runtime) turnEnd(ctx context.Context) string {
	cfg := a.lua.get()
	if cfg == nil || !cfg.HasHook("turn_end") {
		return ""
	}
	return cfg.TurnEnd(ctx, a.FinalText())
}
