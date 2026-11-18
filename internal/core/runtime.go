package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zel/internal/config"
	"zel/internal/entities"
	"zel/internal/ipc"
	"zel/internal/luart"
	"zel/internal/store"
)

type Options struct {
	Mode                    string               // main (the TUI, default) or headless (zel -p)
	Custom                  string               // --mode: a custom mode the session runs in besides Mode
	Resume                  string               // open this stored session of the project instead of creating one
	Provider, Model, Effort string               // Optional initial-session overrides; defaults are not changed.
	Configs                 []string             // absolute paths of --config, loaded after the default files
	NoDefaultConfigs        bool                 // --no-default-configs: no init.lua and no ./zel.lua
	Prompt                  string               // headless: the prompt, shown in the parent's row of a child
	SessionReady            func(*Runtime) error // May persist and announce the session.
}

// Runtime is the stable application handle held by callers and the controller
// across NewSession/Resume. Each opened session owns an independent *session so
// surviving background work (hooks, abandoned lua calls) keeps its own
// session identity and environment without racing with session replacement.
type Runtime struct {
	opts      Options
	home      string
	state     string
	store     *store.Store
	project   string
	ask       *chooser
	changes   *notifier // children changed
	parent    *parentLink
	depth     int
	closeOnce sync.Once

	mu  sync.RWMutex
	cur *session
}

// session is the owned state of one Zel session. It is never overwritten in
// place when switching sessions.
type session struct {
	opts      Options
	home      string
	dir       string
	store     *store.Store // shared handle of the sessions folder
	project   string
	id        string
	created   time.Time
	cfg       *settingsState
	conf      *confHolder
	path      string      // the config file
	token     string      // key of the session socket
	sock      *ipc.Server // nil until the session is acquired
	changes   *notifier
	parent    *parentLink // set when this is a zel -p started from another session
	childMu   sync.Mutex
	children  map[string]SubagentRun // zel -p children that run now, by session ID
	childPIDs map[string]int
	hookCtx   context.Context
	stopHooks context.CancelFunc
	outputMu  sync.Mutex // numbers the files of cut call outputs

	luaMu        sync.Mutex
	luaConf      *luart.Config // the config VM of the snapshot in conf
	agent        *luart.Agent  // the agent VM, created on first call
	turning      atomic.Bool
	idleSent     atomic.Bool
	histMu       sync.RWMutex
	saveMu       sync.Mutex
	logMu        sync.Mutex
	traceMu      sync.Mutex
	traceSize    int64
	traceMod     time.Time
	traceCache   []TraceEntry
	History      []json.RawMessage
	cacheMu      sync.Mutex
	projections  []historyProjection
	firstLine    string
	metaVersion  uint64
	itemMeta     map[int]*Metadata
	saved        int
	stored       bool
	owner        *os.File
	prompt       string
	promptFrozen bool
	title        string
	lastRequest  Metadata
	requestedLen int            // History length when lastRequest was sent
	compacted    compaction     // latest automatic compaction boundaries
	choices      []store.Choice // answered questions shown in the chat, by history position
	ask          *chooser       // the questions of choice() the UI shows
	depth        int            // zel -p nesting depth: 0 for a session you started
	acts         *activities    // foreground host operations in progress
	queue        *Steering
	slots        *imageSlots
	life         *lifecycle
	liveSteps    map[string]bool // steps streamed by the response in flight, by item ID; see closeTurn
	ended        atomic.Bool
}

func (a *Runtime) session() *session {
	a.mu.RLock()
	s := a.cur
	a.mu.RUnlock()
	return s
}

func (a *Runtime) Home() string      { return a.home }
func (a *Runtime) StateHome() string { return a.state }

func New(dir string, opts Options) (*Runtime, error) {
	home, err := ZelHome(dir)
	if err != nil {
		return nil, err
	}
	state, err := StateHome(dir)
	if err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = "main"
	}
	if !opts.NoDefaultConfigs {
		if err := config.EnsureHome(home); err != nil {
			return nil, err
		}
	}
	a := &Runtime{opts: opts, home: home, state: state, ask: &chooser{}, changes: &notifier{}}
	if opts.Mode == "headless" {
		// A zel -p started by sh() of another session is its child.
		if a.parent, err = parentFromEnv(); err != nil {
			return nil, err
		}
		if a.parent != nil {
			a.depth = a.parent.depth + 1
			if a.depth > MaxDepth {
				return nil, fail(CodeDenied, "EDENIED: zel -p is nested too deeply (max depth %d)", MaxDepth)
			}
		}
	}
	if a.store, err = store.Open(state); err != nil {
		return nil, err
	}
	s := a.newSessionState(dir, nil, nil)
	a.cur = s
	if err := s.openProject(dir, opts.Resume); err != nil {
		a.Close()
		return nil, err
	}
	a.project = s.project
	// Initial selection: CLI flags, then the parent's model for a child, then the config.
	provider, model, effort := opts.Provider, opts.Model, opts.Effort
	if a.parent != nil && opts.Resume == "" && provider == "" && model == "" {
		provider, model, effort = a.parent.provider, a.parent.model, cmp.Or(effort, a.parent.effort)
	}
	if provider != "" || model != "" || effort != "" {
		st := s.Settings()
		if err := s.ConfigureSession(context.Background(), cmp.Or(provider, st.Provider), cmp.Or(model, st.Model), cmp.Or(effort, st.Effort)); err != nil {
			s.shutdown("quit")
			return nil, err
		}
	}
	if opts.SessionReady != nil {
		if err := opts.SessionReady(a); err != nil {
			s.shutdown("quit")
			return nil, err
		}
	}
	s.announceChild()
	if opts.Resume == "" {
		s.sessionCreated()
	} else {
		s.fireHooks("session_resumed", map[string]any{})
	}
	return a, nil
}

func (a *Runtime) newSessionState(dir string, wake func(), notice func(string)) *session {
	hookCtx, stopHooks := context.WithCancel(context.Background())
	s := &session{
		hookCtx: hookCtx, stopHooks: stopHooks,
		opts:    a.opts,
		home:    a.home,
		dir:     dir,
		store:   a.store,
		project: a.project,
		ask:     a.ask,
		changes: a.changes,
		parent:  a.parent,
		depth:   a.depth,
		token:   ipc.Token(),
		slots:   &imageSlots{},
		queue:   &Steering{wake: wake},
		conf:    &confHolder{notice: notice},
		acts:    &activities{},
	}
	s.path = s.configFile()
	return s
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func (s *session) message(role, text string) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.History = append(s.History, raw(map[string]any{"role": role, "content": text}))
}

func (s *session) saveHistory() error {
	if err := s.Save(); err != nil {
		return fmt.Errorf("could not save history: %w", err)
	}
	return nil
}

func (a *Runtime) Run(ctx context.Context, text string, emit func(TurnEvent)) error {
	return a.RunInput(ctx, Input{Text: text}, emit)
}

func (a *Runtime) RunInput(ctx context.Context, input Input, emit func(TurnEvent)) error {
	return a.session().RunInput(ctx, input, emit)
}

func (s *session) RunInput(ctx context.Context, input Input, emit func(TurnEvent)) (err error) {
	started, turnSeq := time.Now(), -1
	s.turning.Store(true)
	s.idleSent.Store(false)
	defer s.turning.Store(false)
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			emit(TurnEvent{Kind: "error", Text: err.Error()})
			s.fireHooks("error", map[string]any{"kind": "turn", "message": err.Error()})
		}
		if text, ok := turnEndText(ctx, err); ok && turnSeq >= 0 {
			s.closeTurn(turnSeq, text, started, emit)
		}
		if s.HasSavedSession() {
			if e := s.saveHistory(); e != nil {
				err = errors.Join(err, e)
				emit(TurnEvent{Kind: "error", Text: e.Error()})
			}
		}
	}()

	// A crash may leave calls without outputs. Their effects are unknown, so
	// never rerun them; supply an explicit interrupted result on continuation.
	s.histMu.RLock()
	interrupted := interruptedOutputs(s.History)
	s.histMu.RUnlock()
	for _, output := range interrupted {
		s.histMu.Lock()
		s.History = append(s.History, output)
		s.histMu.Unlock()
		for _, r := range RecordsForItem(output) {
			emit(TurnEvent{Kind: r.Kind, Text: r.Text, Final: true})
		}
	}

	// Only the turn goroutine mutates history. Steering waits until all outputs
	// for the current response have been committed, preserving call/output pairs.
	acceptSteering := func() bool {
		if ctx.Err() != nil {
			return false
		}
		accepted := false
		for _, instruction := range batch(s.queue.Take()) {
			if strings.TrimSpace(instruction.Text) != "" || len(instruction.Images) > 0 {
				instruction, ok := s.hookInput(ctx, instruction, emit)
				if !ok {
					continue
				}
				s.addInput(instruction, emit)
				accepted = true
			}
		}
		return accepted
	}

	input, ok := s.hookInput(ctx, input, emit)
	if !ok {
		return nil
	}
	shown := inputEvent(input)
	shown.ID = fmt.Sprintf("input-%d", time.Now().UnixNano())
	s.commitInput(input)
	s.histMu.RLock()
	turnSeq = len(s.History) - 1
	shown.Meta = s.itemMeta[turnSeq]
	s.histMu.RUnlock()
	emit(shown)
	s.fireHooks("turn_start", map[string]any{"text": input.Text})

	return (&turnHost{s: s, emit: emit, accept: acceptSteering, turnSeq: turnSeq}).run(ctx)
}

// turnEndType is the history item that closes a turn ending with an error or a stop.
const turnEndType = "turn_end"

// turnEndText says how a turn ended unsuccessfully: the error text, or "stopped
// by user" when it was cancelled. ok is false when the turn finished.
func turnEndText(ctx context.Context, err error) (text string, ok bool) {
	switch {
	case err == nil:
		return "", false
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return "stopped by user", true
	}
	return err.Error(), true
}

// closeTurn appends the closing message of a failed or stopped turn. The chat shows
// it, also after resume; the model never sees it. Its time is the wall clock since
// the turn started, as the working indicator shows it. Its steps are the turn's,
// including streamed steps of a response that never committed.
func (s *session) closeTurn(turnSeq int, text string, started time.Time, emit func(TurnEvent)) {
	item := raw(map[string]any{"type": turnEndType, "text": text})
	s.histMu.Lock()
	s.History = append(s.History, item)
	seq := len(s.History) - 1
	steps := countSteps(s.History[turnSeq:seq]) + len(s.liveSteps)
	s.liveSteps = nil
	s.histMu.Unlock()
	m := Metadata{Duration: time.Since(started), Steps: steps, At: time.Now()}
	s.setItemMeta(seq, m)
	for _, r := range RecordsForItem(item) {
		emit(TurnEvent{Kind: r.Kind, Text: r.Text, Meta: &m, Final: true})
	}
}

// noteSteps remembers the steps a response streams, by item ID, so a turn that
// ends before the response is committed still counts them. Repeated events of
// one item, such as partial arguments or a retried stream, count once.
func (s *session) noteSteps(emit func(TurnEvent)) func(TurnEvent) {
	return func(e TurnEvent) {
		if e.ID != "" && e.Kind == "code" {
			if s.liveSteps == nil {
				s.liveSteps = map[string]bool{}
			}
			s.liveSteps[e.ID] = true
		}
		emit(e)
	}
}

// countSteps counts tool calls, excluding reasoning blocks, for the calls total.
func countSteps(items []json.RawMessage) (steps int) {
	for _, item := range items {
		for _, r := range RecordsForItem(item) {
			if r.Kind == "code" {
				steps++
			}
		}
	}
	return steps
}

type functionCall = entities.ToolCall

func parseCall(item json.RawMessage) (call functionCall, err error) {
	err = json.Unmarshal(item, &call)
	return call, err
}

func callOutput(callID, output string) json.RawMessage {
	return raw(map[string]any{"type": "function_call_output", "call_id": callID, "output": output})
}

// runTool executes a lua tool call and returns the text the model sees. Messages
// the user types meanwhile stay queued until the call ends.
func (s *session) runTool(ctx context.Context, callID, name, arguments string, emit func(TurnEvent)) string {
	if ctx.Err() != nil {
		return "error: lua: ECANCELED: the call was cancelled"
	}
	if name != "lua" {
		return "error: unknown tool: " + name
	}
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "error: lua: EINVAL: the tool input must be Lua code (" + err.Error() + ")"
	}
	return s.runLua(ctx, args.Code)
}

func (s *session) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", s.lastRequest.RequestID, index)
}

const (
	tokenBytes  = 4
	imageTokens = 1200
)

func estimateTokens(history []json.RawMessage) int64 {
	var total int64
	for _, item := range history {
		var v struct{ Content []struct{ Type string } }
		if json.Unmarshal(item, &v) == nil {
			for _, part := range v.Content {
				if part.Type == "input_image" {
					total += imageTokens
				}
			}
		}
		total += int64(textBytes(item)) / tokenBytes
	}
	return total
}

func (a *Runtime) ContextTokens() int64 { return a.session().ContextTokens() }

func (s *session) ContextTokens() int64 {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	if s.lastRequest.ContextInput != nil {
		// Items added since that request (tool outputs, steering) are estimated.
		return *s.lastRequest.ContextInput + s.tokensFrom(s.requestedLen)
	}
	var total int64
	marked := s.compacted.Drop == 0
	for i := range s.History {
		p := s.projection(i)
		if !p.sent {
			continue
		}
		if !marked && i >= s.compacted.Drop {
			total += compactionNoticeTokens
			marked = true
		}
		if _, ok, tokens := s.compactedProjection(i); ok {
			total += tokens
		}
	}
	return total + int64(len(s.prompt))/tokenBytes
}

func (a *Runtime) FinalText() string { return a.session().FinalText() }

func (s *session) FinalText() string {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	var texts []string
	for i := len(s.History) - 1; i >= 0; i-- {
		var v struct {
			Role, Type string
		}
		if json.Unmarshal(s.History[i], &v) != nil || v.Role == "user" || v.Type == "function_call" || v.Type == "function_call_output" {
			break
		}
		for _, r := range RecordsForItem(s.History[i]) {
			if r.Kind == "assistant" && r.Text != "" {
				texts = append([]string{r.Text}, texts...)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func (a *Runtime) SessionID() string { return a.session().id }

func luaParameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"code": map[string]any{"type": "string", "description": "Lua code"}},
		"required":             []string{"code"},
		"additionalProperties": false,
	}
}

func (s *session) setItemMeta(seq int, m Metadata) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	if s.itemMeta == nil {
		s.itemMeta = map[int]*Metadata{}
	}
	s.itemMeta[seq] = &m
	s.metaVersion++
	if seq < s.saved {
		s.saved = seq
	}
}
