package core

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zel/internal/luart"
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

func (h *luaHolder) noticeHandler() func(string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.notice
}

func (h *luaHolder) setNotice(fn func(string)) {
	h.mu.Lock()
	h.notice = fn
	h.mu.Unlock()
}

// set makes cfg the current snapshot and closes the one it replaces.
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
func (s *session) luaSources() []luart.Source {
	var sources []luart.Source
	for _, l := range LuaLayers(s.home, s.dir) {
		sources = append(sources, luart.Source{Name: l.Name, Path: l.Path})
	}
	return sources
}

// loadLua runs the init.lua layers into a fresh config VM: the snapshot of this
// session. A layer that fails to load is skipped with a notice.
func (s *session) loadLua() {
	s.lua.set(luart.LoadConfig(luaEnv{s}, s.luaSources()))
}

// reloadLua builds a new config VM for Reload. If a layer fails to load the old
// snapshot stays, so a typo in init.lua cannot take the plugins away mid-session.
func (s *session) reloadLua() {
	next := luart.LoadConfig(luaEnv{s}, s.luaSources())
	if old := s.lua.get(); old != nil && len(next.LoadErrs) > 0 {
		next.Close()
		s.notify("Reload failed: the previous init.lua snapshot stays")
		return
	}
	s.lua.set(next)
}

// SetNoticeHandler routes notices raised from Lua (load errors, failing instructions) to the UI.
func (a *Runtime) SetNoticeHandler(fn func(string)) { a.session().lua.setNotice(fn) }

func (s *session) notify(msg string) {
	s.LogEvent("notice", msg)
	if fn := s.lua.noticeHandler(); fn != nil {
		fn(msg)
	}
}

func (a *Runtime) runLua(ctx context.Context, code string) luart.Result {
	return a.session().runLua(ctx, code)
}

// runLua executes one lua tool call.
func (s *session) runLua(ctx context.Context, code string) luart.Result {
	if s.vm == nil {
		s.vm = &luart.Runner{Env: luaEnv{s}}
	}
	s.vm.Cfg = s.lua.get()
	return s.vm.Run(ctx, code)
}

// luaEnv adapts one owned *session to the host API of the Lua runtime.
// It never follows Runtime.cur across session replacements.
type luaEnv struct{ s *session }

func (e luaEnv) Info() luart.SessionInfo {
	s := e.s
	st := s.Settings()
	cwd := s.dir
	info := luart.SessionInfo{ID: s.id, Cwd: cwd, Root: s.projectRoot(), Parent: s.parentRoot, Depth: s.depth,
		Model: st.Model, Effort: st.Effort, Home: s.home, TmpDir: filepath.Join(os.TempDir(), "zel-"+s.id)}
	info.LuaPath = []string{filepath.Join(s.home, "lua")}
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

func (a *Runtime) projectRoot() string { return a.session().projectRoot() }

// projectRoot is the git root of the session directory, else the directory itself.
func (s *session) projectRoot() string {
	h := s.lua
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.root == "" {
		h.root = s.dir
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		cmd.Dir = s.dir
		if out, err := cmd.Output(); err == nil {
			if root := strings.TrimSpace(string(out)); root != "" {
				h.root = root
			}
		}
	}
	return h.root
}

func (e luaEnv) ShellEnv() map[string]string {
	return e.s.shellEnv()
}

// View attaches an image to the next model request.
func (e luaEnv) View(path string) error {
	s := e.s
	if s.ended.Load() {
		return errors.New("session has ended")
	}
	image, err := CaptureImageCopy(path, filepath.Join(s.home, "attachments"))
	if err != nil {
		return err
	}
	if !s.slots.reserve(1) {
		return fmt.Errorf("at most %d new images per model request", MaxPendingImages)
	}
	if !s.queue.Push(withImages(notification("view: "+filepath.Base(path)+" attached"), image)) {
		s.slots.release(1)
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
	s := e.s
	settings := s.Settings()
	key := fmt.Sprint(settings.Providers)
	m := &s.lua.models
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
	return e.s.beginActivity(Redact(e.s.Settings(), label))
}

func (e luaEnv) Notice(msg string) { e.s.notify(msg) }

func (e luaEnv) Log(level, msg string) { e.s.LogEvent("lua-"+level, msg) }

func (e luaEnv) Rename(name string) error { return e.s.Rename(name) }

// sessionStart runs the session_start hooks once per started or resumed
// session; text they return is queued as a notification.
func (s *session) sessionStart() {
	if cfg := s.lua.get(); cfg != nil && cfg.HasHook("session_start") {
		if text := cfg.SessionStart(context.Background()); text != "" {
			s.queue.PushNotice(notification(text))
		}
	}
}

// sessionEnd leaves the session: its jobs are cancelled and then the session_end
// hooks run with what is left of the shutdown budget (see tasks.close).
func (s *session) sessionEnd(reason string) {
	t := s.tasks
	if t == nil {
		return
	}
	t.close(func(ctx context.Context) {
		if cfg := s.lua.get(); cfg != nil && cfg.HasHook("session_end") {
			cfg.SessionEnd(ctx, reason)
		}
	})
}

func (a *Runtime) hookInput(ctx context.Context, input Input) Input {
	return a.session().hookInput(ctx, input)
}

// hookInput lets message hooks observe notifications or rewrite what the user typed.
func (s *session) hookInput(ctx context.Context, input Input) Input {
	cfg := s.lua.get()
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
func (s *session) turnEnd(ctx context.Context) string {
	cfg := s.lua.get()
	if cfg == nil || !cfg.HasHook("turn_end") {
		return ""
	}
	return cfg.TurnEnd(ctx, s.FinalText(), s.runningTasks())
}

// runningTasks lists the background tasks that still run, oldest first.
func (s *session) runningTasks() []luart.RunningTask {
	var out []luart.RunningTask
	for _, t := range s.ActiveTasks() {
		out = append(out, luart.RunningTask{ID: t.ID, Name: t.Name, Ms: time.Since(t.Started).Milliseconds()})
	}
	return out
}
