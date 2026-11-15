package luart

import (
	"context"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// callBudget is the deadline of one Lua call that can be paused: waiting for
// the user (choice) does not use up the call's time.
type callBudget struct {
	mu      sync.Mutex
	timer   *time.Timer
	left    time.Duration
	started time.Time
	expired bool
	cancel  context.CancelFunc
}

type budgetKey struct{}

// budgetContext is a context that ends with context.DeadlineExceeded when its
// budget runs out and with the parent's error otherwise.
type budgetContext struct {
	context.Context
	b *callBudget
}

func (c budgetContext) Err() error {
	c.b.mu.Lock()
	expired := c.b.expired
	c.b.mu.Unlock()
	if err := c.Context.Err(); err != nil && expired {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func (c budgetContext) Value(key any) any {
	if key == (budgetKey{}) {
		return c.b
	}
	return c.Context.Value(key)
}

// withCallBudget derives a context that ends after d of running time.
func withCallBudget(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	inner, cancel := context.WithCancel(parent)
	b := &callBudget{left: d, cancel: cancel}
	b.arm()
	return budgetContext{inner, b}, func() {
		b.mu.Lock()
		if b.timer != nil {
			b.timer.Stop()
		}
		b.mu.Unlock()
		cancel()
	}
}

func (b *callBudget) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = time.Now()
	b.timer = time.AfterFunc(b.left, func() {
		b.mu.Lock()
		b.expired = true
		b.mu.Unlock()
		b.cancel()
	})
}

// pauseBudget stops the clock of the call in ctx until the returned function
// runs; it does nothing for a context without a budget.
func pauseBudget(ctx context.Context) (resume func()) {
	b, ok := ctx.Value(budgetKey{}).(*callBudget)
	if !ok {
		return func() {}
	}
	b.mu.Lock()
	if b.timer == nil || !b.timer.Stop() {
		b.mu.Unlock()
		return func() {}
	}
	b.left = max(0, b.left-time.Since(b.started))
	b.timer = nil
	b.mu.Unlock()
	return b.arm
}

// installChoice installs choice(name, options): the user picks one option in the UI.
func (v *vm) installChoice() {
	v.L.SetGlobal("choice", v.hostFn("choice", v.choice))
}

func (v *vm) choice(L *lua.LState) int {
	title := L.CheckString(1)
	list := L.CheckTable(2)
	var options []string
	for i := 1; i <= list.Len(); i++ {
		s, ok := list.RawGetInt(i).(lua.LString)
		if !ok || s == "" {
			raise(L, "choice", EINVAL, "option #%d is not a non-empty string", i)
		}
		options = append(options, string(s))
	}
	if len(options) == 0 {
		raise(L, "choice", EINVAL, "no options")
	}
	if mode := v.env.Info().Mode; mode != "main" || v.role == roleTask {
		raise(L, "choice", EDENIED, "there is no user to ask here (mode %s%s); decide yourself or report back", mode, map[bool]string{true: ", task", false: ""}[v.role == roleTask])
	}
	defer pauseBudget(v.ctx)()
	picked, ok, err := v.env.Choose(v.ctx, title, options)
	if v.ctx.Err() != nil {
		v.ctxErr("choice")
	}
	if err != nil {
		raise(L, "choice", EFAIL, "%v", err)
	}
	if !ok {
		L.Push(lua.LNil)
	} else {
		L.Push(lua.LString(picked))
	}
	return 1
}
