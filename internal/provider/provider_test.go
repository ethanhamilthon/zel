package provider

import (
	"context"
	"errors"
	"fmt"
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

func TestRetryDelayIsBounded(t *testing.T) {
	if got := Delay(0, nil); got != time.Second {
		t.Fatalf("first pause %s", got)
	}
	if got := Delay(10, nil); got != RetryMax {
		t.Fatalf("capped pause %s", got)
	}
	long := &APIError{Status: 429, RetryAfter: "3600"}
	if got := Delay(0, long); got != RetryMax {
		t.Fatalf("Retry-After above the cap: %s", got)
	}
	short := &APIError{Status: 429, RetryAfter: "3"}
	if got := Delay(0, short); got != 3*time.Second {
		t.Fatalf("Retry-After 3: %s", got)
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
