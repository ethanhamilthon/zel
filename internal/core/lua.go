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
	"zel/internal/shell"
)

//go:embed lua.md
var luaMD string

// LuaToolDescription is the description of the one tool the model gets.
var LuaToolDescription = strings.TrimSpace(luaMD)

// luaHolder keeps the config VM of init.lua layers. It is shared by pointer so a
// session switch and Reload see the same value.
type luaHolder struct {
	mu     sync.RWMutex
	cfg    *luart.Config
	root   string
	notice func(string)
}

func (h *luaHolder) get() *luart.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *luaHolder) set(cfg *luart.Config) {
	h.mu.Lock()
	old := h.cfg
	h.cfg = cfg
	h.mu.Unlock()
	if old != nil {
		old.Close()
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
	s := a.Settings()
	runner := &luart.Runner{Env: luaEnv{a}, Cfg: a.lua.get(),
		OutputLimit: s.ShellOutput}
	return runner.Run(ctx, code)
}

// bgDone lets bg_done hooks rewrite or drop the result message of a finished job.
func (a *Runtime) bgDone(kind, id string, result map[string]any) (string, bool) {
	cfg := a.lua.get()
	if cfg == nil || !cfg.HasHook("bg_done") {
		return "", false
	}
	return cfg.BgDone(context.Background(), kind, id, result)
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
	if !a.queue.Push(Input{Text: "<view>\nImage queued by view().\n</view>", Images: []shell.Image{image}, Notice: true}) {
		a.slots.release(1)
		return fmt.Errorf("input queue is full")
	}
	return nil
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

// sessionStart runs the session_start hooks once per started or resumed session.
func (a *Runtime) sessionStart() {
	if cfg := a.lua.get(); cfg != nil && cfg.HasHook("session_start") {
		cfg.SessionStart(context.Background())
	}
}

// hookInput lets message hooks rewrite what the user typed.
func (a *Runtime) hookInput(ctx context.Context, input Input) Input {
	cfg := a.lua.get()
	if input.Notice || cfg == nil || !cfg.HasHook("message") || input.Text == "" {
		return input
	}
	input.Text = cfg.Message(ctx, input.Text)
	return input
}

// turnEnd runs the turn_end hooks; a returned text becomes a new user message.
func (a *Runtime) turnEnd(ctx context.Context) string {
	cfg := a.lua.get()
	if cfg == nil || !cfg.HasHook("turn_end") {
		return ""
	}
	return cfg.TurnEnd(ctx, a.FinalText())
}
