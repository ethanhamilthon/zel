package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"zel/internal/luart"
	"zel/internal/store"
)

func (a *Runtime) lockPath() string { return filepath.Join(a.home, "locks", a.id+".lock") }

func (a *Runtime) acquire() error {
	if err := os.MkdirAll(filepath.Dir(a.lockPath()), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.lockPath(), os.O_CREATE|os.O_RDWR, 0600)
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
	a.owner = file
	a.tasks = a.newTasks()
	return nil
}

// release stops the jobs, drops the lock and removes the lock file of an unsaved session.
// The database stays open: a session change hands it to the next session.
func (a *Runtime) release() {
	if a.vm != nil {
		a.vm.Close()
		a.vm = nil
	}
	if a.tasks != nil {
		a.tasks.close(nil)
		a.tasks = nil
	}
	if a.owner != nil {
		_ = syscall.Flock(int(a.owner.Fd()), syscall.LOCK_UN)
		_ = a.owner.Close()
		a.owner = nil
		if !a.stored {
			_ = os.Remove(a.lockPath())
		}
	}
}

func (a *Runtime) HasSavedSession() bool { return len(a.History) > 0 || a.stored }

func (a *Runtime) Close() {
	a.sessionEnd("quit")
	a.release()
	a.lua.set(nil) // closes the config VM, or once the last subagent that owes hooks has ended
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
}

// NewSession and Resume are called only when no turn is running.
func (a *Runtime) NewSession() error      { return a.changeSession("") }
func (a *Runtime) Resume(id string) error { return a.changeSession(id) }

func (a *Runtime) Reload() error {
	if err := a.loadConfig(a.Shell.Dir); err != nil {
		return err
	}
	a.reloadLua()
	if a.tasks != nil {
		a.tasks.cancelLua()
	}
	if a.vm != nil {
		a.vm.Reset("init.lua was reloaded")
	}
	previous, frozen := a.prompt, a.promptFrozen
	a.promptFrozen = false
	if _, err := a.instructions(context.Background()); err != nil {
		a.prompt, a.promptFrozen = previous, frozen
		return err
	}
	return nil
}

func (a *Runtime) changeSession(id string) error {
	if id != "" && id == a.SessionID() {
		if err := a.loadConfig(a.Shell.Dir); err != nil {
			return err
		}
		a.loadLua()
		_, err := a.instructions(context.Background())
		return err
	}
	next := &Runtime{opts: a.opts, home: a.home, db: a.db, project: a.project, Shell: a.Shell, tree: a.tree, parentRoot: a.parentRoot, depth: a.depth, link: a.link,
		histMu: &sync.RWMutex{}, slots: &imageSlots{}, queue: &Steering{wake: a.queue.wake}, lua: &luaHolder{notice: a.lua.notice}, acts: &activities{}}
	if err := next.loadConfig(a.Shell.Dir); err != nil {
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
	if a.HasSavedSession() {
		if err = a.Save(); err != nil {
			next.release()
			return err
		}
	}
	reason := "new"
	if id != "" {
		reason = "resume"
	}
	a.sessionEnd(reason)
	a.release()
	a.lua.set(nil)
	*a = *next
	if id != "" {
		a.vm = &luart.Runner{Env: luaEnv{a}}
		a.vm.Reset("the session was resumed")
	}
	a.sessionStart()
	return nil
}

// rootID is the root session of this runtime's agent tree: the session of the
// top-level runtime, which agents started below it report to.
func (a *Runtime) rootID() string {
	if a.parentRoot != "" {
		return a.parentRoot
	}
	return a.id
}

// shellEnv is exported to every sh() call. API keys and settings are never exported.
// ZEL_DEPTH is the agent nesting depth, so a nested `zel -p` still honors the limit.
func (a *Runtime) shellEnv() map[string]string {
	return map[string]string{
		"ZEL_SESSION_ID": a.SessionID(),
		"ZEL_DEPTH":      strconv.Itoa(a.depth),
	}
}

// RenameSession updates the title of a persisted session without opening it.
func RenameSession(cwd, id, title string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	clean := strings.Join(strings.Fields(title), " ")
	if clean == "" {
		return errors.New("session name cannot be empty")
	}
	home, err := ZelHome(cwd)
	if err != nil {
		return err
	}
	db, err := openDB(filepath.Join(home, "zel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.RenameSession(id, clean); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("session not found: %s", id)
		}
		return err
	}
	return nil
}
