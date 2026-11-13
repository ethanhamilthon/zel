package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base, key, api string
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

func New(baseURL, key, api string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: key, api: api}
}

type APIError struct {
	Status     int
	Message    string
	RetryAfter string
}

func (e *APIError) Error() string {
	text := fmt.Sprintf("provider returned %d %s", e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error json.RawMessage
		}
		_ = json.Unmarshal(data, &e)
		var message struct{ Message string }
		if json.Unmarshal(e.Error, &message) != nil {
			_ = json.Unmarshal(e.Error, &message.Message) // some providers send a plain string
		}
		return nil, &APIError{Status: resp.StatusCode, Message: message.Message, RetryAfter: resp.Header.Get("Retry-After")}
	}
	return resp, nil
}

type ModelInfo struct {
	ID            string
	ContextLength int64 `json:"context_length"`
	ContextWindow int64 `json:"context_window"`
}

func (m ModelInfo) Window() int64 {
	if m.ContextLength > 0 {
		return m.ContextLength
	}
	return max(m.ContextWindow, 0)
}

func (c *Client) Models(ctx context.Context) (models []ModelInfo, err error) {
	defer func() { err = c.safeError(err) }()
	var ids []ModelInfo
	seen := map[string]bool{}
	for after, pages := "", 0; pages < maxModelPages; pages++ {
		path := "/models"
		if after != "" {
			path += "?after=" + url.QueryEscape(after)
		}
		resp, err := c.send(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data    []ModelInfo
			HasMore bool `json:"has_more"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
		fresh := 0
		for _, m := range page.Data {
			if !seen[m.ID] {
				seen[m.ID] = true
				ids = append(ids, m)
				fresh++
			}
		}
		if !page.HasMore || fresh == 0 {
			return ids, nil
		}
		after = page.Data[len(page.Data)-1].ID
	}
	return ids, nil
}

const maxModelPages = 100

type StreamEvent struct {
	Type        string                          `json:"type"`
	OutputIndex int64                           `json:"output_index"`
	Delta       string                          `json:"delta"`
	Arguments   string                          `json:"arguments"`
	Input       string                          `json:"input"`
	Item        json.RawMessage                 `json:"item"`
	Code        string                          `json:"code"`
	Message     string                          `json:"message"`
	Error       *struct{ Code, Message string } `json:"error"`
	Response    struct {
		Output []json.RawMessage `json:"output"`
		Usage  *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// ErrStreamDone, returned by a Stream callback, ends the Stream successfully: a
// provider may keep the connection open after its terminal event.
var ErrStreamDone = errors.New("stream done")

const (
	MaxStreamEvent  = 8 << 20
	MaxStreamBytes  = 64 << 20
	MaxStreamEvents = 100000
)

// sse posts body and passes the data of each server-sent event to fn.
func (c *Client) sse(ctx context.Context, path string, body map[string]any, fn func(data string) error) error {
	resp, err := c.send(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	budget := &io.LimitedReader{R: resp.Body, N: MaxStreamBytes + 1}
	scanner := bufio.NewScanner(budget)
	scanner.Buffer(make([]byte, 64<<10), MaxStreamEvent)
	var data strings.Builder
	events := 0
	for scanner.Scan() {
		if budget.N == 0 {
			return errors.New("provider stream: response byte limit exceeded")
		}
		line := scanner.Text()
		if line != "" {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				if data.Len()+len(rest) > MaxStreamEvent {
					return errors.New("provider stream: event byte limit exceeded")
				}
				data.WriteString(strings.TrimPrefix(rest, " "))
			}
			continue
		}
		// Count empty events too so comment/heartbeat floods have a budget.
		events++
		if events > MaxStreamEvents {
			return errors.New("provider stream: event count limit exceeded")
		}
		if data.Len() == 0 || data.String() == "[DONE]" {
			data.Reset()
			continue
		}
		event := data.String()
		data.Reset()
		if err := fn(event); errors.Is(err, ErrStreamDone) {
			return nil
		} else if err != nil {
			return err
		}
	}
	if budget.N == 0 {
		return errors.New("provider stream: response byte limit exceeded")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("provider stream: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if data.Len() > 0 && data.String() != "[DONE]" {
		return fn(data.String())
	}
	return nil
}

const (
	APIResponses = "responses"
	APIChat      = "chat"
)

// Stream normalizes API failures and retries only before exposing response events.
func (c *Client) Stream(ctx context.Context, request Request, fn func(StreamEvent) error) (err error) {
	defer func() { err = c.safeError(err) }()
	custom := c.FreeformTools()
	freeform := freeformFields(request.Tools)
	body := request.payload(custom)
	for attempt := 0; ; attempt++ {
		delivered, completed := false, false
		inputs := map[int64]string{}
		err := c.streamOnce(ctx, body, func(event StreamEvent) error {
			if custom {
				var emit []StreamEvent
				emit, event = normalizeCustom(event, freeform, inputs)
				for _, e := range emit {
					delivered = true
					if err := fn(e); err != nil {
						return err
					}
				}
				if event.Type == "" {
					return nil
				}
			}
			switch event.Type {
			case "response.failed", "response.incomplete", "error":
				return StreamFailure(event)
			case "response.completed":
				completed = true
			}
			delivered = true
			return fn(event)
		})
		if err == nil && !completed {
			err = ErrIncompleteStream
		}
		if err == nil || delivered || attempt >= retryAttempts-1 || !Retryable(err) {
			return err
		}
		err = c.safeError(err)
		pause := Delay(attempt, err)
		if e := fn(StreamEvent{Type: "retry", Message: fmt.Sprintf("Retrying in %s (attempt %d/%d): %s", pause.Round(time.Second), attempt+2, retryAttempts, err)}); e != nil {
			return e
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
