package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"zel/internal/agent"
	"zel/internal/entities"
	"zel/internal/luart"
	"zel/internal/provider"
	"zel/internal/shell"
	"zel/internal/store"
)

var codeField = regexp.MustCompile(`"code"\s*:\s*"`)

func partialCode(arguments string) string {
	loc := codeField.FindStringIndex(arguments)
	if loc == nil {
		return ""
	}
	quoted := arguments[loc[1]-1:]
	var text string
	if json.NewDecoder(strings.NewReader(quoted)).Decode(&text) == nil {
		return text
	}
	for end := len(quoted); end > 0 && end >= len(quoted)-12; end-- {
		candidate := quoted[:end]
		if !utf8.ValidString(candidate) {
			continue
		}
		if json.Unmarshal([]byte(candidate+`"`), &text) == nil {
			return text
		}
	}
	return ""
}

type Options struct {
	Mode                    string               // tui or headless, exported as ZEL_MODE
	Provider, Model, Effort string               // Optional initial-session overrides; defaults are not changed.
	SessionReady            func(*Runtime) error // May persist and announce the session.
	link                    *agentLink           // set for in-process agents
}

// Runtime is the stable application handle held by callers and the controller
// across NewSession/Resume. Each opened session owns an independent *session so
// surviving background work (hooks, tasks, abandoned Lua calls) keeps its own
// session identity and environment without racing with session replacement.
type Runtime struct {
	opts       Options
	home       string
	db         *store.Store
	project    string
	Shell      *shell.Runner
	tree       *agentTree
	parentRoot string
	depth      int
	link       *agentLink
	closeOnce  sync.Once

	mu  sync.RWMutex
	cur *session
}

// session is the owned state of one Zel session. It is never overwritten in
// place when switching sessions.
type session struct {
	opts          Options
	home          string
	dir           string
	db            *store.Store // shared handle; never cleared so late callers see closed DB cleanly
	project       string
	id            string
	cfg           *settingsState
	histMu        sync.RWMutex
	saveMu        sync.Mutex
	logMu         sync.Mutex
	History       []json.RawMessage
	itemMeta      map[int]*Metadata
	saved         int
	stored        bool
	owner         *os.File
	prompt        string
	promptFrozen  bool
	title         string
	lastRequest   Metadata
	requestedLen  int        // History length when lastRequest was sent
	compacted     compaction // latest automatic compaction boundaries
	imageWarnings []Record
	tree          *agentTree  // running agents of this session tree
	parentRoot    string      // root session of the tree when this session is an agent
	depth         int         // agent nesting depth
	link          *agentLink  // set when this session is an agent: its row in the tree
	acts          *activities // foreground host operations in progress
	queue         *Steering
	slots         *imageSlots
	tasks         *tasks
	lua           *luaHolder
	vm            *luart.Runner   // the session's main Lua VM, created on first use
	liveSteps     map[string]bool // steps streamed by the response in flight, by item ID; see closeTurn
	ended         atomic.Bool
}

func (a *Runtime) session() *session {
	a.mu.RLock()
	s := a.cur
	a.mu.RUnlock()
	return s
}

func (a *Runtime) Home() string { return a.home }

func New(dir string, opts Options) (*Runtime, error) {
	home, err := ZelHome(dir)
	if err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = "tui"
	}
	a := &Runtime{
		opts:  opts,
		home:  home,
		tree:  newAgentTree(),
		Shell: &shell.Runner{Dir: dir},
	}
	if opts.link != nil {
		a.tree, a.parentRoot, a.depth, a.link = opts.link.tree, opts.link.root, opts.link.depth, opts.link
	} else {
		a.depth, _ = strconv.Atoi(os.Getenv("ZEL_DEPTH"))
	}
	a.Shell.Env = a.shellEnv
	s := a.newSessionState(dir, nil, nil)
	a.cur = s
	if err := s.loadConfig(dir); err != nil {
		return nil, err
	}
	if a.db, err = openDB(s.Settings().DBPath); err != nil {
		return nil, err
	}
	s.db = a.db
	if err := s.openProject(dir); err != nil {
		a.Close()
		return nil, err
	}
	a.project, a.Shell.Dir = s.project, s.dir
	s.loadLua()
	// Initial selection: CLI flags, then the stored preferences.
	provider, model, effort := opts.Provider, opts.Model, opts.Effort
	if provider != "" || model != "" || effort != "" {
		st := s.Settings()
		if err := s.ConfigureSession(context.Background(), cmp.Or(provider, st.Provider), cmp.Or(model, st.Model), cmp.Or(effort, st.Effort)); err != nil {
			s.shutdown("quit")
			a.closeDB()
			return nil, err
		}
	}
	if opts.SessionReady != nil {
		if err := opts.SessionReady(a); err != nil {
			s.shutdown("quit")
			a.closeDB()
			return nil, err
		}
	}
	s.sessionStart()
	return a, nil
}

func (a *Runtime) newSessionState(dir string, wake func(), notice func(string)) *session {
	return &session{
		opts:       a.opts,
		home:       a.home,
		dir:        dir,
		db:         a.db,
		project:    a.project,
		tree:       a.tree,
		parentRoot: a.parentRoot,
		depth:      a.depth,
		link:       a.link,
		slots:      &imageSlots{},
		queue:      &Steering{wake: wake},
		lua:        &luaHolder{notice: notice},
		acts:       &activities{},
	}
}

// ReloadConfig reads preferences and init.lua; the session's model selection stays.
func (a *Runtime) ReloadConfig() error { return a.session().ReloadConfig() }

func (s *session) ReloadConfig() error {
	return s.loadConfig(s.dir)
}

func (s *session) loadConfig(dir string) error {
	if s.cfg == nil {
		s.cfg = &settingsState{}
	}
	base, err := LoadConfig(s.home)
	if err != nil {
		return err
	}
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	if err := s.applyLocked(base); err != nil {
		// A stale session model must not block a valid config.
		s.cfg.sel = selection{}
		return s.applyLocked(base)
	}
	return nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func (a *Runtime) message(role, text string) { a.session().message(role, text) }

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
	defer func() {
		err = s.safeError(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			emit(TurnEvent{Kind: "error", Text: err.Error()})
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
				s.addInput(s.hookInput(ctx, instruction), emit)
				accepted = true
			}
		}
		return accepted
	}

	input = s.hookInput(ctx, input)
	shown := inputEvent(input)
	shown.ID = fmt.Sprintf("input-%d", time.Now().UnixNano())
	s.commitInput(input)
	s.histMu.RLock()
	turnSeq = len(s.History) - 1
	shown.Meta = s.itemMeta[turnSeq]
	s.histMu.RUnlock()
	emit(shown)

	return agent.Run(ctx, &turnHost{s: s, emit: emit, accept: acceptSteering, turnSeq: turnSeq})
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
		if e.ID != "" && (e.Kind == "code" || e.Kind == "reasoning" || e.Kind == "reasoning_delta") {
			if s.liveSteps == nil {
				s.liveSteps = map[string]bool{}
			}
			s.liveSteps[e.ID] = true
		}
		emit(e)
	}
}

// countSteps counts the lua calls and thinking blocks of items: the steps a turn summary shows.
func countSteps(items []json.RawMessage) (steps int) {
	for _, item := range items {
		for _, r := range RecordsForItem(item) {
			if r.Kind == "code" || r.Kind == "reasoning" {
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
		return "error: lua: " + luart.ECANCELED + ": the call was cancelled"
	}
	if name != "lua" {
		return "error: unknown tool: " + name
	}
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "error: lua: " + luart.EINVAL + ": the tool input must be Lua code (" + err.Error() + ")"
	}
	return s.runLua(ctx, args.Code).Output
}

func (s *session) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", s.lastRequest.RequestID, index)
}

func (s *session) request(ctx context.Context, emit func(TurnEvent)) (items []json.RawMessage, err error) {
	defer func() { err = s.safeError(err) }()
	settings, client := s.snapshot()
	if hint := settings.SetupHint(); hint != "" {
		return nil, errors.New(hint)
	}
	prompt, err := s.instructions(ctx)
	if err != nil {
		return nil, err
	}
	s.histMu.RLock()
	for i, item := range s.History {
		if !json.Valid(item) {
			s.histMu.RUnlock()
			return nil, fmt.Errorf("invalid history item %d", i)
		}
	}
	s.histMu.RUnlock()
	request := provider.Request{Model: settings.Model, Instructions: prompt, Input: s.requestInput(), Effort: settings.Effort, ReasoningSummary: settings.ReasoningSummary,
		Tools: []provider.Tool{{Name: "lua", Description: LuaToolDescription, Parameters: luaParameters(), Freeform: "code"}}}
	started := time.Now()
	s.histMu.Lock()
	s.lastRequest = Metadata{RequestID: fmt.Sprintf("r-%d", len(s.History))}
	s.requestedLen = len(s.History)
	s.histMu.Unlock()
	s.slots.sent()
	emit = s.noteSteps(emit)
	var completed []json.RawMessage
	arguments := map[int64]string{}
	names := map[int64]string{}
	err = client.Stream(ctx, request, func(event provider.StreamEvent) error {
		switch event.Type {
		case "retry":
			emit(TurnEvent{Kind: "notice", Text: Redact(settings, event.Message)})
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			if event.Type == "response.function_call_arguments.delta" {
				arguments[event.OutputIndex] += event.Delta
			} else {
				arguments[event.OutputIndex] = event.Arguments
			}
			if names[event.OutputIndex] == "lua" {
				emit(TurnEvent{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: s.itemID(event.OutputIndex), Final: true})
			}
		case "response.output_item.added", "response.output_item.done":
			var item struct{ Type, Name, Arguments string }
			_ = json.Unmarshal(event.Item, &item)
			// Some providers deliver complete arguments without argument deltas.
			if item.Type == "function_call" {
				names[event.OutputIndex] = item.Name
				if item.Arguments != "" {
					arguments[event.OutputIndex] = item.Arguments
				}
				if item.Name == "lua" {
					emit(TurnEvent{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: s.itemID(event.OutputIndex), Final: true})
					return nil
				}
			}
			if event.Type == "response.output_item.added" {
				for _, record := range RecordsForItem(event.Item) {
					emit(TurnEvent{Kind: record.Kind, Text: record.Text, ID: s.itemID(event.OutputIndex), Final: true, Provisional: true})
				}
			}
		case "response.output_text.delta":
			emit(TurnEvent{Kind: "delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(TurnEvent{Kind: "reasoning_delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.refusal.delta":
			emit(TurnEvent{Kind: "refusal_delta", Text: event.Delta, ID: s.itemID(event.OutputIndex), Provisional: true})
		case "response.completed":
			s.histMu.Lock()
			s.lastRequest.Duration = time.Since(started)
			if u := event.Response.Usage; u != nil {
				n := u.InputTokens
				s.lastRequest.ContextInput = &n
				cached := int64(-1)
				if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
					cached = *u.InputTokensDetails.CachedTokens
				}
				s.lastRequest.Usage = &Usage{Input: n, Output: u.OutputTokens, Cached: cached}
			}
			s.histMu.Unlock()
			completed = append([]json.RawMessage{}, event.Response.Output...)
			return provider.ErrStreamDone
		case "response.failed", "response.incomplete", "error":
			return provider.StreamFailure(event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, provider.ErrIncompleteStream
	}
	return completed, nil
}

const (
	tokenBytes  = 4
	imageTokens = 1200
)

var dataURLPayload = regexp.MustCompile(`data:image/[^"]*`)

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
		total += int64(len(dataURLPayload.ReplaceAllString(string(item), "data:image"))) / tokenBytes
	}
	return total
}

func (a *Runtime) ContextTokens() int64 { return a.session().ContextTokens() }

func (s *session) ContextTokens() int64 {
	s.histMu.RLock()
	defer s.histMu.RUnlock()
	if s.lastRequest.ContextInput != nil {
		// Items added since that request (tool outputs, steering) are estimated.
		var added []json.RawMessage
		if s.requestedLen <= len(s.History) {
			added = s.History[s.requestedLen:]
		}
		return *s.lastRequest.ContextInput + estimateTokens(added)
	}
	return estimateTokens(s.requestInputLocked()) + int64(len(s.prompt))/tokenBytes
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
		"properties":           map[string]any{"code": map[string]any{"type": "string", "description": "Lua source"}},
		"required":             []string{"code"},
		"additionalProperties": false,
	}
}

func (a *Runtime) setItemMeta(seq int, m Metadata) { a.session().setItemMeta(seq, m) }

func (s *session) setItemMeta(seq int, m Metadata) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	if s.itemMeta == nil {
		s.itemMeta = map[int]*Metadata{}
	}
	s.itemMeta[seq] = &m
}
