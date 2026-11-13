package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"zel/internal/entities"
)

type fakeHost struct {
	log      []string
	requests int
	steer    bool
	cancel   context.CancelFunc
}

func (h *fakeHost) AcceptSteering() bool {
	h.log = append(h.log, "steer")
	if h.requests == 1 && h.steer {
		h.steer = false
		return true
	}
	return false
}
func (h *fakeHost) TurnEnd(context.Context) bool { return false }
func (h *fakeHost) Save() error                  { h.log = append(h.log, "save"); return nil }
func (h *fakeHost) Compact() error               { h.log = append(h.log, "compact"); return nil }
func (h *fakeHost) Request(context.Context) ([]json.RawMessage, error) {
	h.log = append(h.log, "request")
	h.requests++
	if h.requests == 1 {
		return []json.RawMessage{json.RawMessage(`{"type":"function_call","name":"bash","call_id":"c1","arguments":"{}"}`)}, nil
	}
	return []json.RawMessage{json.RawMessage(`{"type":"message","content":[]}`)}, nil
}
func (h *fakeHost) CommitResponse([]json.RawMessage) error {
	h.log = append(h.log, "commit")
	if h.cancel != nil {
		h.cancel()
	}
	return nil
}
func (h *fakeHost) Execute(_ context.Context, call entities.ToolCall) error {
	h.log = append(h.log, "execute:"+call.CallID)
	return nil
}

func TestRunCommitsCallsBeforeExecution(t *testing.T) {
	h := &fakeHost{}
	if err := Run(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	want := []string{"steer", "save", "request", "commit", "execute:c1", "compact", "steer", "steer", "save", "request", "commit", "steer"}
	if !reflect.DeepEqual(h.log, want) {
		t.Fatalf("log=%v", h.log)
	}
}
func TestRunDoesNotExecuteToolsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &fakeHost{cancel: cancel}
	if err := Run(ctx, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	for _, event := range h.log {
		if event == "execute:c1" {
			t.Fatal("tool ran after cancellation")
		}
	}
}
