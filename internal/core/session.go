package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"zel/internal/config"
	"zel/internal/ipc"
)

// MaxDepth is how deep zel -p children nest.
const MaxDepth = 3

func (s *session) sessionDir() string { return s.db.Dir(s.project, s.id) }

func (s *session) lockPath() string { return filepath.Join(s.sessionDir(), "lock") }

func (s *session) socketPath() string {
	return ipc.Path(filepath.Join(s.home, ".sessions", "run"), s.id)
}

// acquire takes the session lock, opens the session socket and starts the tasks.
func (s *session) acquire() error {
	if err := os.MkdirAll(s.sessionDir(), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		file.Close()
		return fmt.Errorf("session is in use by another zel process")
	}
	if err != nil {
		file.Close()
		return err
	}
	s.owner = file
	s.tasks = s.newTasks()
	if s.sock, err = ipc.Listen(s.socketPath(), s.token, s.serveSocket); err != nil {
		s.release()
		return fmt.Errorf("session socket: %w", err)
	}
	if s.opts.Mode != "headless" {
		s.startWatch()
	}
	return nil
}

// release stops the jobs, closes the socket, drops the lock and removes the
// folder of a session that was never saved.
func (s *session) release() {
	if s.stopWatch != nil {
		s.stopWatch()
		s.stopWatch = nil
	}
	if s.tasks != nil {
		s.tasks.close(nil)
	}
	if s.sock != nil {
		s.sock.Close()
		s.sock = nil
	}
	if s.owner != nil {
		_ = syscall.Flock(int(s.owner.Fd()), syscall.LOCK_UN)
		_ = s.owner.Close()
		s.owner = nil
		s.histMu.RLock()
		stored := s.stored
		s.histMu.RUnlock()
		if !stored {
			_ = os.RemoveAll(s.sessionDir())
		}
	}
}

func (s *session) shutdown(reason string) {
	s.sessionClosed(reason)
	s.hookMu.Lock()
	s.ended.Store(true)
	if s.stopHooks != nil {
		s.stopHooks()
	}
	s.hookMu.Unlock()
	s.hookWG.Wait()
	if s.queue != nil {
		s.queue.OnPush(nil)
	}
	s.ask.drop(s)
	s.conf.setNotice(nil)
	s.release()
	s.leaveParent()
}

func (a *Runtime) HasSavedSession() bool { return a.session().HasSavedSession() }

func (a *Runtime) closeDB() {
	a.closeOnce.Do(func() {
		if a.db != nil {
			a.db.Close()
		}
	})
}

func (a *Runtime) Close() {
	if s := a.session(); s != nil {
		s.shutdown("quit")
	}
	a.closeDB()
}

// NewSession and Resume are called only when no turn is running.
func (a *Runtime) NewSession() error      { return a.changeSession("") }
func (a *Runtime) Resume(id string) error { return a.changeSession(id) }

func (a *Runtime) changeSession(id string) error {
	old := a.session()
	if id != "" && id == old.id {
		return old.Reload()
	}
	next := a.newSessionState(old.dir, old.queue.wakeHandler(), old.conf.noticeHandler())
	next.project = old.project
	var err error
	if id == "" {
		err = next.createSession()
	} else {
		err = next.openSession(id)
	}
	if err != nil {
		next.release()
		return err
	}
	if old.HasSavedSession() {
		if err = old.Save(); err != nil {
			next.release()
			return err
		}
	}
	reason := "new"
	if id != "" {
		reason = "resume"
	}
	old.shutdown(reason)
	a.mu.Lock()
	a.cur = next
	a.mu.Unlock()
	if id == "" {
		next.sessionCreated()
	} else {
		next.fireHooks("session_resumed", map[string]any{})
	}
	a.changes.notify()
	return nil
}

// OnConfigChanged registers fn to run when the config file of the session changed
// (hot reload); without it the session reloads at once. fn must not block.
func (a *Runtime) OnConfigChanged(fn func()) { a.reloads.set(fn) }

// startWatch follows the config file of the session for hot reload.
func (s *session) startWatch() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopWatch = cancel
	go config.Watch(ctx, s.path, func() {
		if s.ended.Load() {
			return
		}
		if !s.reloads.call() {
			if err := s.Reload(); err != nil {
				s.notify(err.Error())
			}
		}
	})
}

// baseEnv is the environment of everything the session runs: the bash calls,
// tasks, hooks and ${{ }}. API keys are never exported.
func (s *session) baseEnv() map[string]string {
	env := map[string]string{
		"ZEL_SESSION_ID": s.id,
		"ZEL_MODE":       s.mode(),
		"ZEL_DEPTH":      strconv.Itoa(s.depth),
		"ZEL_SOCKET":     s.socketPath(),
		"ZEL_TOKEN":      s.token,
		"ZEL_CONFIG":     s.path,
	}
	if s.parent != nil {
		env["ZEL_PARENT"] = s.parent.session
	}
	if s.cfg != nil {
		st := s.Settings()
		env["ZEL_PROVIDER"], env["ZEL_MODEL"], env["ZEL_EFFORT"] = st.Provider, st.Model, st.Effort
	}
	return env
}

// shellEnv is baseEnv with the [env] of the config.
func (s *session) shellEnv() map[string]string {
	env := map[string]string{}
	if s.cfg != nil {
		for k, v := range s.Settings().Env {
			env[k] = v
		}
	}
	for k, v := range s.baseEnv() {
		env[k] = v
	}
	return env
}

// mode is how the session runs: "main" (the TUI) or "headless" (zel -p).
func (s *session) mode() string { return cmp.Or(s.opts.Mode, "main") }
