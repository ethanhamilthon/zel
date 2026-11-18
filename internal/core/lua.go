package core

import (
	"context"
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
	return luart.SessionInfo{ID: s.id, Name: name, Cwd: s.dir, Root: s.dir, Mode: s.mode(), Modes: s.modes(),
		Depth: s.depth, Model: st.Model, Effort: st.Effort, Provider: st.Provider, TmpDir: filepath.Join(s.sessionDir(), "tmp")}
}

func (e luaEnv) ShellEnv() []string {
	var out []string
	for k, v := range e.s.shellEnv() {
		out = append(out, k+"="+v)
	}
	return out
}

func (e luaEnv) Log(kind, msg string) { e.s.LogEvent(kind, msg) }

// LoadConfig reads the config of a folder without a session, for zel config
// and zel models: connect.json and the config files (the defaults unless
// noDefaults, then configs). Errors are in the Errors of the result.
func LoadConfig(home, cwd string, configs []string, noDefaults bool) *config.Config {
	p := config.DefaultPaths(home, cwd, configs, noDefaults)
	base, err := baseSettings(p.Connect)
	if err == nil {
		var lc *luart.Config
		if lc, err = luart.LoadConfig(context.Background(), bareEnv{cwd}, p, base); err == nil {
			cfg := *lc.Data()
			lc.Close()
			return &cfg
		}
	}
	return &config.Config{Paths: p, Settings: base, Prompts: map[string]string{}, Files: existing(p.Files), Errors: []string{err.Error()}}
}

// existing keeps the files that exist.
func existing(files []string) []string {
	var out []string
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// bareEnv is the host of a config VM outside a session.
type bareEnv struct{ cwd string }

func (e bareEnv) Info() luart.SessionInfo {
	return luart.SessionInfo{Cwd: e.cwd, Root: e.cwd, Mode: config.ModeMain, Modes: []string{config.ModeMain}}
}
func (bareEnv) ShellEnv() []string { return nil }
func (bareEnv) Log(string, string) {}
