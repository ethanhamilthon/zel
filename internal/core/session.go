package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"zel/internal/luart"
)

func (s *session) lockPath() string { return filepath.Join(s.home, "locks", s.id+".lock") }

func (s *session) acquire() error {
	if err := os.MkdirAll(filepath.Dir(s.lockPath()), 0700); err != nil {
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
	return nil
}

// release stops the jobs, drops the lock and removes the lock file of an unsaved session.
// The database stays open: a session change hands it to the next session.
// s.tasks is retained (already closed) so surviving environment work never races on nil.
func (s *session) release() {
	if s.vm != nil {
		s.vm.Close()
		s.vm = nil
	}
	if s.tasks != nil {
		s.tasks.close(nil)
	}
	if s.owner != nil {
		_ = syscall.Flock(int(s.owner.Fd()), syscall.LOCK_UN)
		_ = s.owner.Close()
		s.owner = nil
		s.histMu.RLock()
		stored := s.stored
		s.histMu.RUnlock()
		if !stored {
			_ = os.Remove(s.lockPath())
		}
	}
}

func (s *session) shutdown(reason string) {
	s.sessionEnd(reason)
	s.ended.Store(true)
	if s.queue != nil {
		s.queue.OnPush(nil)
	}
	if s.lua != nil {
		s.lua.setNotice(nil)
	}
	s.release()
	if s.lua != nil {
		s.lua.set(nil) // closes the config VM, or once the last subagent that owes hooks has ended
	}
}

func (a *Runtime) release() { a.session().release() }

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

func (a *Runtime) Reload() error { return a.session().Reload() }

func (s *session) Reload() error {
	if err := s.loadConfig(s.dir); err != nil {
		return err
	}
	s.reloadLua()
	if s.tasks != nil {
		s.tasks.cancelLua()
	}
	if s.vm != nil {
		s.vm.Reset("init.lua was reloaded")
	}
	previous, frozen := s.prompt, s.promptFrozen
	s.promptFrozen = false
	if _, err := s.instructions(context.Background()); err != nil {
		s.prompt, s.promptFrozen = previous, frozen
		return err
	}
	return nil
}

func (a *Runtime) changeSession(id string) error {
	old := a.session()
	if id != "" && id == old.id {
		if err := old.loadConfig(old.dir); err != nil {
			return err
		}
		old.loadLua()
		_, err := old.instructions(context.Background())
		return err
	}
	next := a.newSessionState(old.dir, old.queue.wakeHandler(), old.lua.noticeHandler())
	if err := next.loadConfig(next.dir); err != nil {
		return err
	}
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
	next.loadLua()
	if old.HasSavedSession() {
		if err = old.Save(); err != nil {
			next.release()
			next.lua.set(nil)
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
	if id != "" {
		next.vm = &luart.Runner{Env: luaEnv{next}}
		next.vm.Reset("the session was resumed")
	}
	next.sessionStart()
	return nil
}

// rootID is the root session of this session's agent tree: the session of the
// top-level runtime, which agents started below it report to.
func (s *session) rootID() string {
	if s.parentRoot != "" {
		return s.parentRoot
	}
	return s.id
}

func (a *Runtime) shellEnv() map[string]string { return a.session().shellEnv() }

// shellEnv is exported to every sh() call. API keys and settings are never exported.
// ZEL_DEPTH is the agent nesting depth, so a nested `zel -p` still honors the limit.
func (s *session) shellEnv() map[string]string {
	return map[string]string{
		"ZEL_SESSION_ID": s.id,
		"ZEL_DEPTH":      strconv.Itoa(s.depth),
	}
}
