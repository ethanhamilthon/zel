package ipc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCall(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "session-1")
	cancelled := make(chan struct{})
	s, err := Listen(path, "tok", func(ctx context.Context, req Request) Response {
		if req.Op == "wait" {
			<-ctx.Done()
			close(cancelled)
			return Response{}
		}
		return Response{OK: true, Out: req.Op + ":" + string(req.Args)}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := Call(context.Background(), path, Request{Token: "tok", Op: "echo", Args: []byte(`{"a":1}`)})
	if err != nil || !r.OK || r.Out != `echo:{"a":1}` {
		t.Fatalf("call = %+v, %v", r, err)
	}
	r, _ = Call(context.Background(), path, Request{Token: "bad", Op: "echo"})
	if r.Code != 6 || !strings.Contains(r.Error, "EDENIED") {
		t.Fatalf("bad token = %+v", r)
	}
	// A client that goes away cancels its call.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Call(ctx, path, Request{Token: "tok", Op: "wait"}); err == nil {
		t.Fatal("wait returned without error")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server call was not cancelled")
	}
	if _, err := Call(context.Background(), Path(dir, "other"), Request{}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("missing socket: %v", err)
	}
}
