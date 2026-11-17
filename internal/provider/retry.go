package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	retryAttempts = 6                // the first try and five retries
	RetryMax      = 30 * time.Second // the longest backoff pause
	retryAfterMax = 2 * time.Minute  // a server that asks for longer is not waited for
)

// RetryBase is the pause before the first retry; it doubles. Tests make it small.
var RetryBase = time.Second

// jitter spreads the pauses of many clients; tests replace it.
var jitter = defaultJitter

func defaultJitter(d time.Duration) time.Duration {
	return d - d/5 + rand.N(d*2/5+1)
}

// Retryable says if a failed request may work when sent again.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var api *APIError
	if errors.As(err, &api) {
		switch {
		case api.NoQuota:
			return false
		case api.Status == http.StatusRequestTimeout, api.Status == http.StatusConflict, api.Status == http.StatusTooManyRequests:
			return true
		}
		return api.Status >= 500 && api.Status != http.StatusNotImplemented && api.Status != http.StatusHTTPVersionNotSupported
	}
	var stream *StreamError
	if errors.As(err, &stream) {
		// The provider says the failure is on its side or temporary.
		code := strings.ToLower(stream.Code)
		return slices.ContainsFunc([]string{"server_error", "overloaded", "rate_limit", "unavailable", "timeout"}, func(m string) bool { return strings.Contains(code, m) })
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	for _, target := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, io.ErrUnexpectedEOF, io.EOF, ErrIncompleteStream} {
		if errors.Is(err, target) {
			return true
		}
	}
	// Errors of the HTTP/2 and TLS layers have no types to match.
	text := err.Error()
	for _, marker := range []string{"GOAWAY", "stream error", "connection reset", "unexpected EOF", "client connection lost"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

var ErrIncompleteStream = errors.New("provider stream ended without a completed response")

// RetryDelay is the pause before retry number attempt+1: an exponential
// backoff with jitter, or the Retry-After of the server if that is longer. It
// is false when the server asks for more than retryAfterMax.
func RetryDelay(attempt int, err error) (time.Duration, bool) {
	wait := jitter(min(RetryMax, RetryBase<<attempt))
	var api *APIError
	if errors.As(err, &api) && api.RetryAfter != "" {
		ask, ok := parseRetryAfter(api.RetryAfter, time.Now())
		if ask > retryAfterMax {
			return 0, false
		}
		if ok {
			wait = max(wait, ask)
		}
	}
	return wait, true
}

// parseRetryAfter reads the header: a number of seconds or an HTTP date.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, at.Sub(now)), true
	}
	return 0, false
}

func StreamFailure(event StreamEvent) error {
	fields := []string{event.Code, event.Message}
	switch {
	case event.Error != nil:
		fields = []string{event.Error.Code, event.Error.Message}
	case event.Type == "response.failed" && event.Response.Error != nil:
		fields = []string{event.Response.Error.Code, event.Response.Error.Message}
	case event.Type == "response.incomplete" && event.Response.IncompleteDetails != nil:
		fields = []string{event.Response.IncompleteDetails.Reason}
	}
	detail := strings.TrimSpace(strings.Join(fields, " "))
	if detail == "" {
		detail = "provider reported a failure without details"
	}
	return &StreamError{Type: event.Type, Code: fields[0], Message: detail}
}

type StreamError struct{ Type, Code, Message string }

func (e *StreamError) Error() string { return fmt.Sprintf("provider stream %s: %s", e.Type, e.Message) }

// Failure preserves the original structured error while hiding credentials.
type Failure struct {
	Status           int
	Code, RetryAfter string
	Retryable        bool
	Message          string
	cause            error
}

func (e *Failure) Error() string { return e.Message }
func (e *Failure) Unwrap() error { return e.cause }
func (c *Client) safeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Failure); ok {
		return err
	}
	message := err.Error()
	if c.key != "" {
		message = strings.ReplaceAll(message, c.key, "[redacted]")
	}
	failure := &Failure{Message: message, cause: err, Retryable: Retryable(err)}
	var api *APIError
	if errors.As(err, &api) {
		failure.Status, failure.RetryAfter = api.Status, api.RetryAfter
	}
	var stream *StreamError
	if errors.As(err, &stream) {
		failure.Code = stream.Code
	}
	return failure
}
