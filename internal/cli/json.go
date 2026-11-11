package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zel/internal/agent"
	"zel/internal/control"
	"zel/internal/shell"
)

// JSON mode is newline-delimited JSON over stdin/stdout; see docs/JSON.md.
// Every request gets exactly one result; controller events follow in order.

const maxRequestBytes = 4 * 1024 * 1024

type request struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Text     string   `json:"text,omitempty"`
	Images   []string `json:"images,omitempty"`
	Files    []string `json:"files,omitempty"`
	Session  string   `json:"session,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Model    string   `json:"model,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	Task     string   `json:"task,omitempty"`
}

type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jsonMeta struct {
	DurationMS    int64  `json:"duration_ms,omitempty"`
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
	CachedTokens  *int64 `json:"cached_tokens,omitempty"`
	ContextTokens *int64 `json:"context_tokens,omitempty"`
}

type jsonRecord struct {
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	ID      string    `json:"id,omitempty"`
	Final   bool      `json:"final,omitempty"`
	Preview bool      `json:"preview,omitempty"`
	Meta    *jsonMeta `json:"meta,omitempty"`
}

func toMeta(m *agent.Metadata) *jsonMeta {
	if m == nil {
		return nil
	}
	out := &jsonMeta{DurationMS: m.Duration.Milliseconds(), ContextTokens: m.ContextInput}
	if u := m.Usage; u != nil {
		out.InputTokens, out.OutputTokens = &u.Input, &u.Output
		if u.Cached >= 0 {
			out.CachedTokens = &u.Cached
		}
	}
	return out
}

func toRecords(records []agent.Record) []jsonRecord {
	out := make([]jsonRecord, 0, len(records))
	for _, r := range records {
		out = append(out, jsonRecord{Kind: r.Kind, Text: r.Text, ID: r.ID, Preview: r.Preview, Meta: toMeta(r.Meta)})
	}
	return out
}

func toSession(s control.Session) map[string]any {
	return map[string]any{"id": s.ID, "title": s.Title, "context_tokens": s.Context, "records": toRecords(s.Records)}
}

// outbox serializes results and events to stdout in order without blocking the controller.
type outbox struct {
	mu      sync.Mutex
	pending []any
	wake    chan struct{}
}

func (o *outbox) push(v any) {
	o.mu.Lock()
	o.pending = append(o.pending, v)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// run writes queued lines until ctx ends and the queue is empty.
func (o *outbox) run(ctx context.Context, out io.Writer) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		o.mu.Lock()
		batch := o.pending
		o.pending = nil
		o.mu.Unlock()
		for _, v := range batch {
			_ = enc.Encode(v)
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-o.wake:
		case <-ctx.Done():
			o.mu.Lock()
			empty := len(o.pending) == 0
			o.mu.Unlock()
			if empty {
				return
			}
		}
	}
}

func eventLine(e control.Event) map[string]any {
	line := map[string]any{"type": "event", "event": e.Kind}
	switch e.Kind {
	case "agent":
		a := e.Agent
		line["agent"] = jsonRecord{Kind: a.Kind, Text: a.Text, ID: a.ID, Final: a.Final, Preview: a.Provisional, Meta: toMeta(a.Meta)}
	case "started", "notice", "error":
		line["text"] = e.Text
	case "done":
		line["status"], line["text"], line["context_tokens"] = e.Status, e.Text, e.Context
		if e.Error != "" {
			line["error"] = e.Error
		}
		if len(e.Unsent) > 0 {
			unsent := make([]string, 0, len(e.Unsent))
			for _, in := range e.Unsent {
				unsent = append(unsent, in.Text)
			}
			line["unsent"] = unsent
		}
	case "session":
		line["session"] = toSession(*e.Session)
	}
	if e.Title != "" {
		line["title"] = e.Title
	}
	return line
}

func jsonCommand(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	fs := flagSet("json", func(*flag.FlagSet) {})
	if err := parseWithConfig(fs, args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: zel json [--config FILE]")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	a, err := agent.New(cwd, agent.Options{Mode: "json"})
	if err != nil {
		return err
	}
	defer a.Close()
	home, err := agent.ZelHome(cwd)
	if err != nil {
		return err
	}
	box := &outbox{wake: make(chan struct{}, 1)}
	writing, stopWriting := context.WithCancel(context.Background())
	written := make(chan struct{})
	go func() { box.run(writing, out); close(written) }()
	defer func() { stopWriting(); <-written }()

	ctl := control.New(a, func(e control.Event) { box.push(eventLine(e)) })
	defer ctl.Close()
	s := &jsonServer{ctl: ctl, box: box, attachments: filepath.Join(home, "attachments"), cwd: cwd}
	if session, err := ctl.Session(); err == nil {
		box.push(map[string]any{"type": "event", "event": "session", "session": toSession(session)})
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), maxRequestBytes)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
		readErr <- scanner.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return cancelled
		case <-ctl.Closed():
			return nil
		case err := <-readErr:
			if err != nil {
				fmt.Fprintf(diagnostics, "zel json: %v\n", err)
			}
			// End of input finishes the work in flight, then quits.
			for ctl.Status().Busy {
				select {
				case <-ctx.Done():
					return cancelled
				case <-time.After(20 * time.Millisecond):
				}
			}
			if _, err := ctl.Quit(); err != nil {
				return err
			}
			<-ctl.Closed()
			return nil
		case line := <-lines:
			if strings.TrimSpace(string(line)) != "" {
				s.handle(line)
			}
		}
	}
}

type jsonServer struct {
	ctl         *control.Controller
	box         *outbox
	attachments string
	cwd         string
}

func (s *jsonServer) handle(line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(request{}, nil, &control.Error{Code: control.CodeInvalid, Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.ID == "" {
		s.reply(req, nil, &control.Error{Code: control.CodeInvalid, Message: "request id is required"})
		return
	}
	if req.Type == "models" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			models, failures, err := s.ctl.Models(ctx)
			s.reply(req, map[string]any{"models": modelList(models), "failures": failures}, err)
		}()
		return
	}
	data, err := s.dispatch(req)
	s.reply(req, data, err)
}

func (s *jsonServer) reply(req request, data any, err error) {
	line := map[string]any{"type": "result", "id": req.ID, "ok": err == nil}
	if err != nil {
		e := jsonError{Code: control.CodeFailed, Message: err.Error()}
		if ce := (*control.Error)(nil); errors.As(err, &ce) {
			e.Code = ce.Code
		}
		line["error"] = e
	} else if data != nil {
		line["data"] = data
	}
	s.box.push(line)
}

func invalid(format string, args ...any) error {
	return &control.Error{Code: control.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func (s *jsonServer) dispatch(req request) (any, error) {
	switch req.Type {
	case "input":
		input, err := s.input(req)
		if err != nil {
			return nil, err
		}
		return nil, s.ctl.Input(input)
	case "stop":
		s.ctl.Stop()
		return nil, nil
	case "recall":
		input, err := s.ctl.Recall()
		if err != nil {
			return nil, err
		}
		return map[string]string{"text": input.Text}, nil
	case "quit":
		closed, err := s.ctl.Quit()
		return map[string]bool{"closed": closed}, err
	case "new":
		return nil, s.ctl.NewSession()
	case "resume":
		return nil, s.ctl.Resume(req.Session)
	case "sessions":
		sessions, err := s.ctl.Sessions()
		list := make([]map[string]any, 0, len(sessions))
		for _, ss := range sessions {
			list = append(list, map[string]any{"id": ss.ID, "title": ss.Title, "updated": ss.Updated})
		}
		return map[string]any{"sessions": list}, err
	case "rename":
		return nil, s.ctl.Rename(req.Text)
	case "reload":
		s.ctl.Reload()
		return nil, nil
	case "history":
		session, err := s.ctl.Session()
		if err != nil {
			return nil, err
		}
		return toSession(session), nil
	case "select_model":
		return nil, s.ctl.SelectModel(req.Provider, req.Model, req.Effort)
	case "config":
		return configView(s.ctl.Settings()), nil
	case "info":
		info, err := s.ctl.Info()
		if err != nil {
			return nil, err
		}
		usage := map[string]any{"input_tokens": info.Usage.Input, "output_tokens": info.Usage.Output}
		if info.Usage.Cached >= 0 {
			usage["cached_tokens"] = info.Usage.Cached
		}
		return map[string]any{"session": info.Session, "title": info.Title, "project": info.Project, "db_path": info.DB,
			"name": info.Name, "description": info.Description, "color": info.Color,
			"provider": info.Provider, "model": info.Model, "effort": info.Effort, "configs": info.Configs,
			"messages": info.Messages, "requests": info.Requests, "tasks": info.Tasks,
			"context_tokens": info.Context, "usage": usage}, nil
	case "tasks":
		tasks, err := s.ctl.Tasks()
		list := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			item := map[string]any{"id": t.ID, "command": t.Command, "status": t.Status, "started": t.Started}
			if t.ExitCode != nil {
				item["exit_code"] = *t.ExitCode
			}
			list = append(list, item)
		}
		return map[string]any{"tasks": list}, err
	case "show_task":
		output, err := s.ctl.TaskOutput(req.Task)
		return map[string]string{"output": output}, err
	case "stop_task":
		return nil, s.ctl.StopTask(req.Task)
	case "status":
		st := s.ctl.Status()
		queued := make([]string, 0, len(st.Queued))
		for _, in := range st.Queued {
			queued = append(queued, in.Text)
		}
		data := map[string]any{"busy": st.Busy, "cancelling": st.Cancelling, "queued": queued, "tasks": st.Tasks}
		if st.Busy {
			data["started"] = st.Started
		}
		return data, nil
	}
	return nil, invalid("unknown request type: %q", req.Type)
}

// input attaches images directly and mentions files as @path lines.
func (s *jsonServer) input(req request) (agent.Input, error) {
	input := agent.Input{Text: req.Text}
	for _, path := range req.Files {
		input.Text += "\n@" + path
	}
	for _, path := range req.Images {
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.cwd, path)
		}
		image, err := shell.CaptureImageCopy(path, s.attachments)
		if err != nil {
			return input, invalid("%s: %v", path, err)
		}
		input.Images = append(input.Images, image)
	}
	if strings.TrimSpace(input.Text) == "" && len(input.Images) > 0 {
		input.Text = "See the attached images."
	}
	return input, nil
}

func configView(s agent.Settings) map[string]any {
	providers := map[string]any{}
	for name, p := range s.Providers {
		providers[name] = map[string]string{"base_url": p.BaseURL}
	}
	return map[string]any{"provider": s.Provider, "model": s.Model, "effort": s.Effort,
		"reasoning_summary": s.ReasoningSummary, "shell_output": s.ShellOutput, "providers": providers}
}

// modelList is the JSON shape of a model listing, shared by zel models --json and the models request.
func modelList(choices []agent.ModelChoice) []map[string]string {
	list := make([]map[string]string, 0, len(choices))
	for _, c := range choices {
		list = append(list, map[string]string{"provider": c.Provider, "model": c.Model})
	}
	return list
}
