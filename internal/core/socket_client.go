package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"zel/internal/ipc"
)

const socketTokenFile = "socket-token"

// SessionArgs are the arguments shared by the CLI and the session socket.
type SessionArgs struct {
	Text, ID, Name string
	Files, Options []string
	Done           bool
}

// CallSession sends a helper request to the explicit or inherited session ID.
func CallSession(ctx context.Context, id, op string, args SessionArgs) (out string, code int, err error) {
	if id == "" {
		id = os.Getenv("ZEL_SESSION_ID")
		if id == "" {
			return "", 1, fmt.Errorf("EFAIL: no session (set ZEL_SESSION_ID or use -s ID)")
		}
	}
	if !validSessionID(id) || id == "." || id == ".." {
		return "", 2, fmt.Errorf("EINVAL: invalid session ID %q", id)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", 1, err
	}
	path, token, err := sessionSocket(cwd, id)
	if err != nil {
		return "", 1, fmt.Errorf("EFAIL: %w", err)
	}
	r, err := ipc.Call(ctx, path, ipc.Request{Op: op, Token: token, Cwd: cwd, Args: raw(args)})
	if err != nil {
		return "", 1, err
	}
	if !r.OK {
		return "", r.Code, fmt.Errorf("%s", r.Error)
	}
	if op == "session" {
		return string(r.Data), 0, nil
	}
	return r.Out, 0, nil
}

// sessionSocket locates the credentials for a live session across projects.
func sessionSocket(cwd, id string) (socket, token string, err error) {
	state, err := StateHome(cwd)
	if err != nil {
		return "", "", err
	}
	root := filepath.Join(state, "sessions")
	projects, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	for _, p := range projects {
		if !p.IsDir() || p.Name() == "run" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, p.Name(), id, socketTokenFile))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return ipc.Path(filepath.Join(root, "run"), id), strings.TrimSpace(string(data)), nil
	}
	return "", "", fmt.Errorf("zel session %s is not running", id)
}

// Publish credentials only when the session is ready to serve CLI requests.
func (s *session) publishSocketToken() error {
	if err := s.saveSocketToken(); err != nil {
		return fmt.Errorf("session socket token: %w", err)
	}
	return nil
}

func (s *session) saveSocketToken() error {
	file, err := os.CreateTemp(s.sessionDir(), ".socket-token-*") // private, even on resume
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(s.token)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(s.sessionDir(), socketTokenFile))
}
