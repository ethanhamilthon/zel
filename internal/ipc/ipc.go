// Package ipc is the unix socket of a session: a child zel -p started by its
// sh() sends one JSON request per connection and reads one JSON response.
package ipc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Version of the request format.
const Version = 1

// Request is one call. Task is ZEL_TASK of the caller, empty outside a task.
type Request struct {
	V     int             `json:"v"`
	Token string          `json:"token"`
	Op    string          `json:"op"`
	Task  string          `json:"task,omitempty"`
	Cwd   string          `json:"cwd,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
}

// Response is the answer: Out is printed to stdout, Error to stderr, and the
// client exits with Code.
type Response struct {
	OK    bool            `json:"ok"`
	Out   string          `json:"out,omitempty"`
	Code  int             `json:"code"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Handler serves one request; ctx ends when the client goes away or the server closes.
type Handler func(ctx context.Context, req Request) Response

// Path is the socket path of a session under runDir, short enough for the
// 104-byte limit of macOS; very long homes fall back to the temp directory.
func Path(runDir, session string) string {
	sum := sha256.Sum256([]byte(session))
	name := hex.EncodeToString(sum[:6]) + ".sock"
	if p := filepath.Join(runDir, name); len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("zel-%d-%s", os.Getuid(), name))
}

// Token is a new random key for a session socket.
func Token() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Server listens on a session socket.
type Server struct {
	path     string
	token    string
	handler  Handler
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Listen opens the socket at path (0600), replacing a stale file. The caller
// holds the session lock, so no live server owns that path.
func Listen(path, token string, handler Handler) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0600)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{path: path, token: token, handler: handler, listener: l, ctx: ctx, cancel: cancel}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *Server) Path() string { return s.path }

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

const maxRequest = 16 << 20

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(io.LimitReader(conn, maxRequest))
	var req Request
	if err := dec.Decode(&req); err != nil {
		writeResponse(conn, Response{Code: 2, Error: "EINVAL: bad request: " + err.Error()})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		writeResponse(conn, Response{Code: 6, Error: "EDENIED: wrong session token"})
		return
	}
	if req.V != Version {
		writeResponse(conn, Response{Code: 2, Error: fmt.Sprintf("EINVAL: request version %d, this zel speaks %d", req.V, Version)})
		return
	}
	// The call is cancelled when the client hangs up (its bash call was killed).
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()
	writeResponse(conn, s.handler(ctx, req))
}

func writeResponse(conn net.Conn, r Response) {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(r)
}

// Close stops accepting, cancels the calls in flight, waits for them and removes the socket.
func (s *Server) Close() {
	s.cancel()
	_ = s.listener.Close()
	s.wg.Wait()
	_ = os.Remove(s.path)
}

// Call sends one request to the socket at path and waits for the answer.
func Call(ctx context.Context, path string, req Request) (Response, error) {
	req.V = Version
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("EFAIL: zel is not running (%v)", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("EFAIL: send: %v", err)
	}
	var r Response
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("EFAIL: zel closed the connection (%v)", err)
	}
	return r, nil
}
