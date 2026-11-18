package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"zel/internal/config"
	"zel/internal/luart"
)

// Bounds of the working indicator label: the indicator cuts lines to its width
// and cycles through them, so longer ones are not needed.
const (
	labelLines    = 100
	labelLineRune = 200
)

// codeLabel is what the working indicator shows for a call: the code as is,
// its lines without empty ones and -- comments, one per row.
func codeLabel(command string) string {
	var lines []string
	for _, l := range strings.Split(command, "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "--") {
			continue
		}
		if r := []rune(l); len(r) > labelLineRune {
			l = string(r[:labelLineRune-1]) + "…"
		}
		if lines = append(lines, l); len(lines) == labelLines {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// openOutputFile creates the next outputs/b<N>.txt of the session folder.
func (s *session) openOutputFile() (*os.File, error) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	dir := filepath.Join(s.sessionDir(), "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	last := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "b"), ".txt")); err == nil && strings.HasPrefix(name, "b") {
			last = max(last, n)
		}
	}
	return os.OpenFile(filepath.Join(dir, "b"+strconv.Itoa(last+1)+".txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

// cutText keeps the first and last half of limit bytes of text, with the
// marker between them. total is the size of the full output. A cut never
// splits a UTF-8 character and moves to a line break within ~10% of the half.
func cutText(text string, total, limit int, path string) string {
	half := limit / 2
	slack := half / 10
	headEnd := min(half, len(text))
	for headEnd > 0 && headEnd < len(text) && !utf8.RuneStart(text[headEnd]) {
		headEnd--
	}
	if i := strings.LastIndexByte(text[max(0, headEnd-slack):headEnd], '\n'); i >= 0 {
		headEnd = max(0, headEnd-slack) + i + 1
	}
	tailStart := max(headEnd, len(text)-(limit-half))
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	if i := strings.IndexByte(text[tailStart:min(len(text), tailStart+slack)], '\n'); i >= 0 {
		tailStart += i + 1
	}
	head, tail := text[:headEnd], text[tailStart:]
	gap := "\n\n"
	if strings.HasSuffix(head, "\n") {
		gap = "\n"
	}
	omitted := total - len(head) - len(tail)
	return head + gap +
		fmt.Sprintf("======== [output cut: %d of %d bytes omitted]\n", omitted, total) +
		"======== full output: " + path + "\n" +
		"======== read it with slice(read(path), from, count) or grep()\n\n" + tail
}

// workDir is the folder Lua calls work in: the one set by cd(), else the
// folder the session started in.
func (s *session) workDir() string {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	return cmp.Or(s.workDirPath, s.dir)
}

// workDirSet is the folder set by cd(), empty when none.
func (s *session) workDirSet() string {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	return s.workDirPath
}

// changeDir is cd(): dir (relative to base, ~ allowed) becomes the folder of
// the next Lua calls; empty goes back to the start folder. It returns the new
// absolute path. Nothing changes when dir is missing or not a folder.
func (s *session) changeDir(dir, base string) (string, error) {
	path := s.dir
	if dir != "" {
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fail(CodeInvalid, "EINVAL: %v", err)
			}
			dir = filepath.Join(home, dir[1:])
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cmp.Or(base, s.workDir()), dir)
		}
		path = filepath.Clean(dir)
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fail(CodeNotFound, "ENOENT: %s: no such folder", path)
	case err != nil:
		return "", fail(CodeInvalid, "EINVAL: %v", err)
	case !info.IsDir():
		return "", fail(CodeInvalid, "EINVAL: %s: not a folder", path)
	}
	s.dirMu.Lock()
	if path == s.dir {
		s.workDirPath = ""
	} else {
		s.workDirPath = path
	}
	s.dirMu.Unlock()
	if s.HasSavedSession() {
		if err := s.Save(); err != nil {
			return path, nil
		}
	}
	return path, nil
}

// runLua runs one call of the agent: the lua_before hooks may block or
// rewrite the code, the lua_after hooks may add to its output.
func (s *session) runLua(ctx context.Context, code string) string {
	if s.hasHooks("lua_before") {
		res := s.runHooks(ctx, "lua_before", map[string]any{"code": code})
		if res.blocked {
			return "[blocked by a hook: " + strings.TrimSpace(res.reason) + "]"
		}
		code, _ = res.input["code"].(string)
	}
	agent := s.luaAgent()
	if agent == nil {
		return "error: lua: ECANCELED: the session has ended"
	}
	end := s.beginActivity(codeLabel(config.Redact(s.Settings(), code)))
	r := agent.Run(ctx, code, time.Duration(s.Settings().Timeout)*time.Second)
	end()
	out := r.Out
	if s.hasHooks("lua_after") {
		res := s.runHooks(ctx, "lua_after", map[string]any{"code": code, "out": out, "ok": r.OK,
			"ms": r.Ms, "timed_out": r.TimedOut})
		for _, extra := range res.outs {
			out = strings.TrimRight(out, "\n") + "\n" + extra
		}
	}
	return s.cutOutput(out)
}

// cutOutput applies settings.output to the text the agent gets; the full text
// goes to a file of the session folder.
func (s *session) cutOutput(text string) string {
	limit := s.Settings().Output
	if limit <= 0 || len(text) <= limit {
		return text
	}
	path := ""
	f, err := s.openOutputFile()
	if err == nil {
		_, err = f.WriteString(text)
		path = f.Name()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		path = "(not saved: " + err.Error() + ")"
	}
	return cutText(text, len(text), limit, path)
}

// luaAgent is the agent VM of the session, created on first use.
func (s *session) luaAgent() *luart.Agent {
	s.luaMu.Lock()
	defer s.luaMu.Unlock()
	if s.ended.Load() {
		return nil
	}
	if s.agent == nil {
		s.agent = luart.NewAgent(luaEnv{s}, s.luaConf)
	}
	return s.agent
}

// setLuaConfig makes cfg the config VM of the session. The old one is closed
// and the agent VM starts fresh with the new functions.
func (s *session) setLuaConfig(cfg *luart.Config) {
	s.luaMu.Lock()
	old, agent := s.luaConf, s.agent
	s.luaConf = cfg
	s.agent = nil
	if agent != nil {
		s.agent = luart.NewAgent(luaEnv{s}, cfg)
		s.agent.Reset()
	}
	s.luaMu.Unlock()
	if agent != nil {
		agent.Close()
	}
	if old != nil && old != cfg {
		old.Close()
	}
}

// closeLua frees the VMs of a session that ends.
func (s *session) closeLua() {
	s.luaMu.Lock()
	cfg, agent := s.luaConf, s.agent
	s.luaConf, s.agent = nil, nil
	s.luaMu.Unlock()
	if agent != nil {
		agent.Close()
	}
	if cfg != nil {
		cfg.Close()
	}
}

func (s *session) luaConfig() *luart.Config {
	s.luaMu.Lock()
	defer s.luaMu.Unlock()
	return s.luaConf
}

// luaEnv is the host of the Lua VMs of a session.
type luaEnv struct{ s *session }

func (e luaEnv) Info() luart.SessionInfo {
	s := e.s
	st := Settings{}
	if s.cfg != nil {
		st = s.Settings()
	}
	s.histMu.RLock()
	name := s.title
	if name == "" {
		name = firstUserLine(s.History)
	}
	s.histMu.RUnlock()
	return luart.SessionInfo{ID: s.id, Name: name, Cwd: s.workDir(), Root: s.dir, Mode: s.mode(), Modes: s.modes(),
		Depth: s.depth, Model: st.Model, Effort: st.Effort, Provider: st.Provider, TmpDir: filepath.Join(s.sessionDir(), "tmp")}
}

func (e luaEnv) ShellEnv() []string {
	var out []string
	for k, v := range e.s.shellEnv() {
		out = append(out, k+"="+v)
	}
	return out
}

func (e luaEnv) Cd(dir string) error {
	_, err := e.s.changeDir(dir, e.s.workDir())
	return err
}

func (e luaEnv) Attach(path string) error { return e.s.view(path) }

func (e luaEnv) Choice(question string, options []string) (string, error) {
	if e.s.mode() != "main" {
		return "", errors.New("EDENIED: choice needs the user: not available in zel -p")
	}
	id, err := e.s.ask.ask(e.s, question, options)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("c%d", id), nil
}

func (e luaEnv) Rename(name string) error { return e.s.Rename(name) }

func (e luaEnv) Models(ctx context.Context) ([]luart.ModelInfo, map[string]string) {
	list, failed := AllModels(ctx, e.s.Settings().Providers)
	out := make([]luart.ModelInfo, 0, len(list))
	for _, m := range list {
		out = append(out, luart.ModelInfo{Provider: m.Provider, Model: m.Model, ContextWindow: m.ContextWindow})
	}
	failures := map[string]string{}
	for name, err := range failed {
		failures[name] = err.Error()
	}
	return out, failures
}

func (e luaEnv) Tasks() luart.Tasks { return luaTasks{e.s.tasks} }

func (e luaEnv) Log(kind, msg string) { e.s.LogEvent(kind, msg) }

// luaTasks is the task registry as the Lua runtime sees it.
type luaTasks struct{ t *tasks }

func (l luaTasks) Spawn(name string, timeout time.Duration, run luart.TaskFunc) (string, error) {
	if l.t == nil {
		return "", errors.New("EDENIED: no tasks in this session")
	}
	j, err := l.t.spawnLua(name, timeout, run)
	if err != nil {
		return "", err
	}
	return j.id, nil
}

func (l luaTasks) List() []luart.TaskInfo {
	var out []luart.TaskInfo
	for _, j := range l.t.list() {
		out = append(out, j.info())
	}
	return out
}

func (l luaTasks) Get(id string) (luart.TaskInfo, bool) {
	j, err := l.t.find(id)
	if err != nil {
		return luart.TaskInfo{}, false
	}
	return j.info(), true
}

func (l luaTasks) Wait(ctx context.Context, ids []string, timeout time.Duration) []luart.TaskInfo {
	deadline := time.Now().Add(timeout)
	out := make([]luart.TaskInfo, 0, len(ids))
	for _, id := range ids {
		j, err := l.t.find(id)
		if err != nil {
			out = append(out, luart.TaskInfo{ID: id, State: "unknown"})
			continue
		}
		left := time.Duration(0)
		if timeout > 0 {
			left = max(time.Nanosecond, time.Until(deadline))
		}
		j.Wait(ctx, left)
		out = append(out, j.info())
	}
	return out
}

func (l luaTasks) Tail(id string, n int) (string, error) {
	j, err := l.t.find(id)
	if err != nil {
		return "", err
	}
	return tailLines(j.log, cmp.Or(n, 20)), nil
}

func (l luaTasks) Stop(id string) error {
	j, err := l.t.find(id)
	if err != nil {
		return err
	}
	j.Stop()
	return nil
}

// LoadConfig reads the config of a folder without a session, for zel config
// and zel models: connect.json, init.lua and zel.lua (unless flag names
// another init.lua). Errors are in the Errors of the result.
func LoadConfig(home, cwd, flag string) *config.Config {
	p := config.DefaultPaths(home, cwd, flag)
	if flag == "" && os.Getenv("ZEL_NO_LOCAL") == "1" {
		p.Local = ""
	}
	base, err := baseSettings(p.Connect)
	if err == nil {
		var lc *luart.Config
		if lc, err = luart.LoadConfig(context.Background(), bareEnv{cwd}, p, base); err == nil {
			cfg := *lc.Data()
			lc.Close()
			return &cfg
		}
	}
	return &config.Config{Paths: p, Settings: base, Prompts: map[string]string{}, Errors: []string{err.Error()}}
}

// bareEnv is the host of a config VM outside a session.
type bareEnv struct{ cwd string }

var errNoSession = errors.New("EDENIED: not available outside a session")

func (e bareEnv) Info() luart.SessionInfo {
	return luart.SessionInfo{Cwd: e.cwd, Root: e.cwd, Mode: config.ModeMain, Modes: []string{config.ModeMain}}
}
func (bareEnv) ShellEnv() []string                                            { return nil }
func (bareEnv) Cd(string) error                                               { return errNoSession }
func (bareEnv) Attach(string) error                                           { return errNoSession }
func (bareEnv) Choice(string, []string) (string, error)                       { return "", errNoSession }
func (bareEnv) Rename(string) error                                           { return errNoSession }
func (bareEnv) Models(context.Context) ([]luart.ModelInfo, map[string]string) { return nil, nil }
func (bareEnv) Tasks() luart.Tasks                                            { return nil }
func (bareEnv) Log(string, string)                                            {}
