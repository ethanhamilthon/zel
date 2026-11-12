package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	if err := a.listenNotifications(); err != nil {
		a.release()
		return err
	}
	env := a.shellEnv()
	a.tasks.mu.Lock()
	a.tasks.env = env
	a.tasks.mu.Unlock()
	return nil
}

// release closes the socket, drops the lock and removes the lock file of an unsaved session.
// The database stays open: a session change hands it to the next session.
func (a *Runtime) release() {
	a.StopNotifications()
	if a.tasks != nil {
		a.tasks.close()
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

func (a *Runtime) StopNotifications() bool {
	if a.sock == nil {
		return false
	}
	a.sock.retire()
	_ = os.Remove(a.sock.path)
	a.sock = nil
	return true
}

func (a *Runtime) HasSavedSession() bool { return len(a.History) > 0 || a.stored }

func (a *Runtime) Close() {
	if a.hooks != nil && a.db != nil {
		a.fire("session_close", nil)
		a.fire("app_exit", nil)
	}
	a.release()
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
		_, err := a.instructions(context.Background())
		return err
	}
	next := &Runtime{opts: a.opts, home: a.home, db: a.db, project: a.project, Shell: a.Shell,
		histMu: &sync.RWMutex{}, slots: &imageSlots{}, queue: &Steering{wake: a.queue.wake}, hooks: a.hooks}
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
	if a.HasSavedSession() {
		if err = a.Save(); err != nil {
			next.release()
			return err
		}
	}
	a.fire("session_close", nil)
	a.takePasted() // Do not carry output from the previous session.
	a.release()
	*a = *next
	a.fire("session_start", nil)
	return nil
}

// ParentSession returns the root session ID of the whole tree if running under
// another zel instance (via ZEL_PARENT), or empty if root.
func ParentSession() string {
	return strings.TrimSpace(os.Getenv("ZEL_PARENT"))
}

func (a *Runtime) rootID() string {
	if root := ParentSession(); root != "" {
		return root
	}
	return a.id
}

// shellEnv is exported to every bash tool call. API keys and settings are never exported.
// ZEL_PARENT holds the root session ID of the whole tree.
func (a *Runtime) shellEnv() map[string]string {
	env := map[string]string{
		"ZEL_SESSION_ID": a.SessionID(),
		"ZEL_PARENT":     a.rootID(),
	}
	if a.sock != nil {
		env["ZEL_ENDPOINT"] = a.sock.path
	}
	return env
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
