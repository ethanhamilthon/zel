package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	retryAttempts = 3
	retryBase     = time.Second
	RetryMax      = 8 * time.Second
)

func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var api *APIError
	if errors.As(err, &api) {
		switch api.Status {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return api.Status >= 500
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	for _, target := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, io.ErrUnexpectedEOF, io.EOF, ErrIncompleteStream} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

var ErrIncompleteStream = errors.New("provider stream ended without a completed response")

func Delay(attempt int, err error) time.Duration {
	wait := min(RetryMax, retryBase<<attempt)
	var api *APIError
	if errors.As(err, &api) {
		if seconds, e := strconv.Atoi(api.RetryAfter); e == nil && seconds > 0 {
			seconds = min(seconds, int(RetryMax/time.Second))
			wait = max(wait, time.Duration(seconds)*time.Second)
		}
	}
	return wait
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
