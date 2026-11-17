package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamBudgets(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		repeat int
		want   string
	}{
		{"line", "data: " + strings.Repeat("x", MaxStreamEvent) + "\n", 1, "token too long"},
		{"multiline event", "data: " + strings.Repeat("x", MaxStreamEvent/2) + "\n", 3, "event byte limit"},
		{"response", ":" + strings.Repeat("x", (1<<20)-2) + "\n", 65, "response byte limit"},
		{"event count", "\n", MaxStreamEvents + 1, "event count limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < tc.repeat; i++ {
					if _, err := fmt.Fprint(w, tc.line); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			c := New(server.URL, "", "")
			err := c.Stream(context.Background(), Request{}, func(StreamEvent) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v want %s", err, tc.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	jitter = func(d time.Duration) time.Duration { return d }
	defer func() { jitter = defaultJitter }()
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, RetryMax, RetryMax} {
		if got, ok := RetryDelay(attempt, nil); !ok || got != want {
			t.Fatalf("pause %d = %s, want %s", attempt, got, want)
		}
	}
	if got, ok := RetryDelay(0, &APIError{Status: 429, RetryAfter: "7"}); !ok || got != 7*time.Second {
		t.Fatalf("Retry-After 7: %s %v", got, ok)
	}
	if got, ok := RetryDelay(3, &APIError{Status: 429, RetryAfter: "2"}); !ok || got != 8*time.Second {
		t.Fatalf("a short Retry-After does not shorten the backoff: %s", got)
	}
	date := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if got, ok := RetryDelay(0, &APIError{Status: 503, RetryAfter: date}); !ok || got < 40*time.Second || got > 46*time.Second {
		t.Fatalf("Retry-After as a date: %s %v", got, ok)
	}
	if _, ok := RetryDelay(0, &APIError{Status: 429, RetryAfter: "3600"}); ok {
		t.Fatal("a server that asks for an hour is not waited for")
	}
	for range 100 {
		if got, _ := RetryDelay(2, nil); got < 3200*time.Millisecond || got > 4800*time.Millisecond {
			t.Fatalf("jitter out of bounds: %s", got)
		}
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 429}, true},
		{&APIError{Status: 529}, true},
		{&APIError{Status: 503}, true},
		{&APIError{Status: 429, NoQuota: true}, false},
		{&APIError{Status: 501}, false},
		{&APIError{Status: 400}, false},
		{&APIError{Status: 401}, false},
		{errors.New("http2: server sent GOAWAY and closed the connection"), true},
		{io.ErrUnexpectedEOF, true},
		{context.Canceled, false},
		{errors.New("invalid request"), false},
	} {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A failure after the start of the response, before any content, is retried; one after content is not.
func TestRetryAfterTheStartOfAResponseOnly(t *testing.T) {
	jitter = func(time.Duration) time.Duration { return time.Millisecond }
	defer func() { jitter = defaultJitter }()
	for _, content := range []bool{false, true} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
			if content {
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
			}
			if calls == 1 {
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
		}))
		err := New(server.URL, "", APIResponses).Stream(context.Background(), Request{}, func(StreamEvent) error { return nil })
		server.Close()
		if content && (err == nil || calls != 1) || !content && (err != nil || calls != 2) {
			t.Fatalf("content %v: %d calls, err %v", content, calls, err)
		}
	}
}

func TestChatStreamCountsNonzeroUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"hello"}}]}`+"\n\ndata: "+`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"prompt_tokens_details":{"cached_tokens":67}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	client := New(server.URL, "", APIChat)
	var input, output, cached int64
	err := client.Stream(context.Background(), Request{Model: "m"}, func(e StreamEvent) error {
		if e.Response.Usage != nil {
			input, output = e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens
			cached = *e.Response.Usage.InputTokensDetails.CachedTokens
		}
		return nil
	})
	if err != nil || input != 123 || output != 45 || cached != 67 {
		t.Fatalf("usage=%d/%d/%d err=%v", input, output, cached, err)
	}
}

func TestRetryCancellationAndRedaction(t *testing.T) {
	const key = "secret-provider-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, key)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(server.URL, key, APIResponses)
	notices := 0
	err := client.Stream(ctx, Request{}, func(e StreamEvent) error {
		if strings.Contains(e.Message, key) {
			t.Fatal("credential leaked through retry event")
		}
		if e.Type == "retry" {
			notices++
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || notices != 1 {
		t.Fatalf("err=%v notices=%d", err, notices)
	}
}

func TestFailureRetainsStructuredStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid sk-private"}}`)
	}))
	defer server.Close()
	err := New(server.URL, "sk-private", APIResponses).Stream(context.Background(), Request{}, func(StreamEvent) error { return nil })
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 401 || failure.Retryable || strings.Contains(failure.Message, "sk-private") {
		t.Fatalf("failure=%+v", failure)
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != 401 {
		t.Fatalf("original cause unavailable: %v", err)
	}
}
