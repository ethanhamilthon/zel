package agent

import (
	"cmp"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"zel/internal/shell"
)

var BashToolDescription = strings.TrimSpace(bashMD)

//go:embed bash.md
var bashMD string

var codeField = regexp.MustCompile(`"command"\s*:\s*"`)

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
	Mode                    string             // tui or headless, exported as ZEL_MODE
	Provider, Model, Effort string             // Optional initial-session overrides; defaults are not changed.
	SessionReady            func(*Agent) error // Called before startup hooks; may persist and announce the session.
}

type Agent struct {
	opts          Options
	home          string
	cfg           *settingsState
	histMu        *sync.RWMutex
	Shell         *shell.Runner
	History       []json.RawMessage
	itemMeta      map[int]*Metadata
	db            *sql.DB
	project       string
	id            string
	saved         int
	stored        bool
	owner         *os.File
	prompt        string
	promptFrozen  bool
	title         string
	lastRequest   Metadata
	imageWarnings []Record
	sock          *sessionSocket
	queue         *Steering
	slots         *imageSlots
	tasks         *tasks
	hooks         *hookRunner
}

func New(dir string, opts Options) (*Agent, error) {
	home, err := ZelHome(dir)
	if err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = "tui"
	}
	a := &Agent{opts: opts, home: home, histMu: &sync.RWMutex{}, Shell: &shell.Runner{Dir: dir}, slots: &imageSlots{}, queue: &Steering{}, hooks: &hookRunner{}}
	a.Shell.Env = a.shellEnv
	if err := a.loadConfig(dir); err != nil {
		return nil, err
	}
	if a.db, err = openDB(a.Settings().DBPath); err != nil {
		return nil, err
	}
	if err := a.openProject(dir); err != nil {
		a.Close()
		return nil, err
	}
	if opts.Provider != "" || opts.Model != "" || opts.Effort != "" {
		s := a.Settings()
		if err := a.ConfigureSession(context.Background(), cmp.Or(opts.Provider, s.Provider), cmp.Or(opts.Model, s.Model), cmp.Or(opts.Effort, s.Effort)); err != nil {
			a.release()
			a.db.Close()
			return nil, err
		}
	}
	if opts.SessionReady != nil {
		if err := opts.SessionReady(a); err != nil {
			a.release()
			a.db.Close()
			return nil, err
		}
	}
	a.fire("app_start", nil)
	a.fire("session_start", nil)
	return a, nil
}

// loadConfig reads preferences and ZEL.md; the session's model selection stays.
func (a *Agent) ReloadConfig() error {
	return a.loadConfig(a.Shell.Dir)
}

func (a *Agent) loadConfig(dir string) error {
	if a.cfg == nil {
		a.cfg = &settingsState{}
	}
	base, err := LoadConfig(a.home, dir)
	if err != nil {
		return err
	}
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	if err := a.applyLocked(base); err != nil {
		// A stale session model must not block a valid config.
		a.cfg.sel = selection{}
		return a.applyLocked(base)
	}
	return nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func (a *Agent) message(role, text string) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	a.History = append(a.History, raw(map[string]any{"role": role, "content": text}))
}

func (a *Agent) saveHistory() error {
	if err := a.Save(); err != nil {
		return fmt.Errorf("could not save history: %w", err)
	}
	return nil
}

func (a *Agent) Run(ctx context.Context, text string, emit func(Event)) error {
	return a.RunInput(ctx, Input{Text: text}, emit)
}

func (a *Agent) RunInput(ctx context.Context, input Input, emit func(Event)) (err error) {
	started := time.Now()
	if !input.Notice {
		if a.UserMessages() == 0 {
			a.fire("first_message", map[string]string{"ZEL_MESSAGE": input.Text})
		}
		a.fire("user_message", map[string]string{"ZEL_MESSAGE": input.Text})
	}
	a.fire("turn_start", map[string]string{"ZEL_TURN_SOURCE": map[bool]string{true: "notice", false: "user"}[input.Notice], "ZEL_MESSAGE": input.Text})
	defer func() {
		err = a.safeError(err)
		status := "ok"
		switch {
		case errors.Is(err, context.Canceled):
			status = "cancelled"
		case err != nil:
			status = "error"
			emit(Event{Kind: "error", Text: err.Error()})
			a.fire("error", map[string]string{"ZEL_ERROR": err.Error()})
		}
		a.fire("turn_end", map[string]string{"ZEL_TURN_STATUS": status, "ZEL_TURN_DURATION_MS": millis(time.Since(started))})
		if a.HasSavedSession() {
			if e := a.saveHistory(); e != nil {
				err = errors.Join(err, e)
				emit(Event{Kind: "error", Text: e.Error()})
			}
		}
	}()

	// A crash may leave calls without outputs. Their effects are unknown, so
	// never rerun them; supply an explicit interrupted result on continuation.
	for _, output := range interruptedOutputs(a.History) {
		a.histMu.Lock()
		a.History = append(a.History, output)
		a.histMu.Unlock()
		for _, r := range RecordsForItem(output) {
			emit(Event{Kind: r.Kind, Text: r.Text, Final: true})
		}
	}

	// Only the turn goroutine mutates history. Steering waits until all outputs
	// for the current response have been committed, preserving call/output pairs.
	acceptSteering := func() bool {
		if ctx.Err() != nil {
			return false
		}
		accepted := false
		for _, instruction := range a.queue.Take() {
			if strings.TrimSpace(instruction.Text) != "" || len(instruction.Images) > 0 {
				a.addInput(instruction, emit)
				accepted = true
				if !instruction.Notice {
					a.fire("user_message", map[string]string{"ZEL_MESSAGE": instruction.Text})
				}
			}
		}
		return accepted
	}

	for _, text := range a.takePasted() {
		a.addInput(Input{Text: text, Notice: true}, emit)
	}
	shown := inputEvent(input)
	shown.ID = fmt.Sprintf("input-%d", time.Now().UnixNano())
	a.commitInput(input)
	shown.Meta = a.itemMeta[len(a.History)-1]
	emit(shown)

	turnSeq := len(a.History) - 1
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		acceptSteering()
		if e := a.saveHistory(); e != nil {
			return e
		}
		items, e := a.requestWithRetries(ctx, emit)
		if e != nil {
			return e
		}
		meta := a.lastRequest
		meta.RequestSeq = len(a.History) + 1
		meta.TurnSeq = turnSeq
		meta.At = time.Now()
		calls := 0
		for i, item := range items {
			a.histMu.Lock()
			a.History = append(a.History, item)
			a.histMu.Unlock()
			a.setItemMeta(len(a.History)-1, meta)
			for _, r := range RecordsForItem(item) {
				m := meta
				if r.Kind == "assistant" {
					m = a.answerMetadata(len(a.History) - 1)
				}
				emit(Event{Kind: r.Kind, Text: r.Text, ID: a.itemID(int64(i)), Meta: &m, Final: true})
			}
		}
		if e := a.saveHistory(); e != nil {
			return e
		}
		for _, item := range items {
			call, e := parseCall(item)
			if e != nil {
				return e
			}
			if call.Type != "function_call" {
				continue
			}
			calls++
			started := time.Now()
			result := a.runTool(ctx, call.CallID, call.Name, call.Arguments, emit)
			output := callOutput(call.CallID, result)
			a.histMu.Lock()
			a.History = append(a.History, output)
			a.histMu.Unlock()
			m := Metadata{Duration: time.Since(started), At: time.Now()}
			a.setItemMeta(len(a.History)-1, m)
			for _, r := range RecordsForItem(output) {
				emit(Event{Kind: r.Kind, Text: r.Text, Meta: &m, Final: true})
			}
			if e := a.saveHistory(); e != nil {
				return e
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if steered := acceptSteering(); calls == 0 && !steered {
			return nil
		}
	}
}

type functionCall struct {
	Type, Name, Arguments string
	CallID                string `json:"call_id"`
}

func parseCall(item json.RawMessage) (call functionCall, err error) {
	err = json.Unmarshal(item, &call)
	return call, err
}

func callOutput(callID string, result shell.Result) json.RawMessage {
	return raw(map[string]any{"type": "function_call_output", "call_id": callID, "output": string(raw(result))})
}

func (a *Agent) runTool(ctx context.Context, callID, name, arguments string, emit func(Event)) (result shell.Result) {
	var args struct {
		Command string `json:"command"`
		Timeout *int64 `json:"timeout"`
	}
	err := json.Unmarshal([]byte(arguments), &args)
	if ctx.Err() != nil {
		return shell.Result{Output: ctx.Err().Error(), ExitCode: 130, Error: true}
	}
	if a.queue.hasTyped() {
		return shell.Result{Output: "Skipped: the user sent a new message before this call started. Read it first; run the command again if it is still needed."}
	}
	if name != "bash" {
		return shell.Result{Output: "Unknown tool: " + name, Error: true}
	}
	settings := a.Settings()
	timeout, limit := time.Duration(settings.ShellTimeout)*time.Second, settings.ShellOutput
	if err == nil && args.Timeout != nil {
		if *args.Timeout <= 0 || *args.Timeout > ShellMaxTimeout {
			err = fmt.Errorf("timeout must be 1..%d seconds", ShellMaxTimeout)
		} else {
			timeout = time.Duration(*args.Timeout) * time.Second
		}
	}
	if err != nil {
		return shell.Result{Output: err.Error(), ExitCode: 2, Error: true}
	}
	var detach <-chan struct{}
	if a.tasks != nil {
		a.queue.drainTyped()
		detach = a.queue.typedSignal()
	}
	a.fire("bash_start", map[string]string{"ZEL_COMMAND": args.Command})
	shellStarted := time.Now()
	result, bg := a.Shell.Run(ctx, args.Command, timeout, limit, detach)
	a.fire("bash_end", map[string]string{"ZEL_COMMAND": args.Command, "ZEL_EXIT_CODE": strconv.Itoa(result.ExitCode), "ZEL_OUTPUT": result.Output, "ZEL_DURATION_MS": millis(time.Since(shellStarted))})
	if bg == nil {
		return result
	}
	id, err := a.tasks.adopt(args.Command, bg)
	if err != nil {
		_ = bg.Cmd.Cancel()
		_ = bg.Wait()
		return shell.Result{Output: result.Output + "\n[zel: stopped; could not move it to the background: " + err.Error() + "]\n", ExitCode: 130, Error: true}
	}
	emit(Event{Kind: "notice", Text: "Command moved to the background as task " + id + "."})
	return shell.Result{Output: fmt.Sprintf("The user sent a new message, so this command moved to the background as task %s and keeps running. "+
		"Its result arrives as <task-result id=%q> when it ends; check it with: zel tasks status %s\n\nOutput so far:\n%s", id, id, id, cmp.Or(result.Output, "(none)"))}
}

func (a *Agent) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", a.lastRequest.RequestID, index)
}

func (a *Agent) request(ctx context.Context, emit func(Event)) (items []json.RawMessage, err error) {
	requestStarted := time.Now()
	a.fire("model_request_start", nil)
	for _, text := range a.takePasted() {
		a.addInput(Input{Text: text, Notice: true}, emit)
	}
	defer func() {
		vars := map[string]string{"ZEL_DURATION_MS": millis(time.Since(requestStarted))}
		if err != nil {
			vars["ZEL_ERROR"] = err.Error()
		}
		a.fire("model_request_end", vars)
	}()
	defer func() { err = a.safeError(err) }()
	settings, client := a.snapshot()
	if hint := settings.SetupHint(); hint != "" {
		return nil, errors.New(hint)
	}
	prompt, err := a.instructions(ctx)
	if err != nil {
		return nil, err
	}
	for i, item := range a.History {
		if !json.Valid(item) {
			return nil, fmt.Errorf("invalid history item %d", i)
		}
	}
	payload := map[string]any{
		"model":        settings.Model,
		"instructions": prompt,
		"input":        a.requestInput(),
		"store":        false,
		"tools": []any{map[string]any{
			"type": "function", "name": "bash", "description": BashToolDescription,
			"parameters": bashParameters(settings.ShellTimeout), "strict": true,
		}},
	}
	reasoning := map[string]any{}
	if settings.Effort != "" {
		reasoning["effort"] = settings.Effort
	}
	if settings.ReasoningSummary {
		reasoning["summary"] = "auto"
	}
	if len(reasoning) > 0 {
		payload["reasoning"] = reasoning
	}
	started := time.Now()
	a.histMu.Lock()
	a.lastRequest = Metadata{RequestID: fmt.Sprintf("r-%d", len(a.History))}
	a.histMu.Unlock()
	a.slots.sent()
	var completed []json.RawMessage
	arguments := map[int64]string{}
	names := map[int64]string{}
	err = client.stream(ctx, payload, func(event streamEvent) error {
		switch event.Type {
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			if event.Type == "response.function_call_arguments.delta" {
				arguments[event.OutputIndex] += event.Delta
			} else {
				arguments[event.OutputIndex] = event.Arguments
			}
			if names[event.OutputIndex] == "bash" {
				emit(Event{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: a.itemID(event.OutputIndex), Final: true})
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
				if item.Name == "bash" {
					emit(Event{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: a.itemID(event.OutputIndex), Final: true})
					return nil
				}
			}
			if event.Type == "response.output_item.added" {
				for _, record := range RecordsForItem(event.Item) {
					emit(Event{Kind: record.Kind, Text: record.Text, ID: a.itemID(event.OutputIndex), Final: true, Provisional: true})
				}
			}
		case "response.output_text.delta":
			emit(Event{Kind: "delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(Event{Kind: "reasoning_delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.refusal.delta":
			emit(Event{Kind: "refusal_delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.completed":
			a.histMu.Lock()
			a.lastRequest.Duration = time.Since(started)
			if u := event.Response.Usage; u != nil {
				n := u.InputTokens
				a.lastRequest.ContextInput = &n
				cached := int64(-1)
				if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
					cached = *u.InputTokensDetails.CachedTokens
				}
				a.lastRequest.Usage = &Usage{Input: n, Output: u.OutputTokens, Cached: cached}
			}
			a.histMu.Unlock()
			completed = append([]json.RawMessage{}, event.Response.Output...)
			return errStreamDone
		case "response.failed", "response.incomplete", "error":
			return streamFailure(event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, errIncompleteStream
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

func (a *Agent) ContextTokens() int64 {
	a.histMu.RLock()
	defer a.histMu.RUnlock()
	if a.lastRequest.ContextInput != nil {
		return *a.lastRequest.ContextInput
	}
	return estimateTokens(a.requestInput()) + int64(len(a.prompt))/tokenBytes
}

func (a *Agent) FinalText() string {
	var texts []string
	for i := len(a.History) - 1; i >= 0; i-- {
		var v struct {
			Role, Type string
		}
		if json.Unmarshal(a.History[i], &v) != nil || v.Role == "user" || v.Type == "function_call" || v.Type == "function_call_output" {
			break
		}
		for _, r := range RecordsForItem(a.History[i]) {
			if r.Kind == "assistant" && r.Text != "" {
				texts = append([]string{r.Text}, texts...)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func (a *Agent) SessionID() string { return a.id }

func bashParameters(defaultTimeout int) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("seconds, default %d, maximum %d", defaultTimeout, ShellMaxTimeout),
			},
		},
		"required":             []string{"command", "timeout"},
		"additionalProperties": false,
	}
}

func (a *Agent) setItemMeta(seq int, m Metadata) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	if a.itemMeta == nil {
		a.itemMeta = map[int]*Metadata{}
	}
	a.itemMeta[seq] = &m
}
