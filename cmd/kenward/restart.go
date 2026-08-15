package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// restartSignal carries "this process must go down and come back" from the update
// scheduler to the serve loop.
//
// It exists because the two halves are built at different moments: the scheduler
// needs its Restart hook at construction, and the serve loop needs to know, after
// everything has drained, whether to exit in a way the service manager will restart.
//
// Why a Restart hook at all, rather than letting keel return ErrRestartPending and
// treating that as the signal: internal/updater drains before the swap and then
// restarts *even when the swap itself fails*, precisely so a household whose intake
// has already been stopped comes back up. ErrRestartPending is only returned on a
// successful swap, so relying on it would leave the one case that matters — drained,
// swap failed — with a live process that has stopped answering and no reason to exit.
// A household silently ignoring its members is worse than one that restarts.
type restartSignal struct {
	requested atomic.Bool
	once      sync.Once
	ch        chan struct{}
}

func newRestartSignal() *restartSignal {
	return &restartSignal{ch: make(chan struct{})}
}

// request records that the process should exit to be restarted. It is safe to call
// from any goroutine and more than once.
func (r *restartSignal) request() {
	r.requested.Store(true)
	r.once.Do(func() { close(r.ch) })
}

// wanted reports whether a restart was asked for.
func (r *restartSignal) wanted() bool { return r != nil && r.requested.Load() }

// waitCh is closed when a restart is requested.
func (r *restartSignal) waitCh() <-chan struct{} { return r.ch }

// hook is what the update scheduler is given as its Restart function.
func (r *restartSignal) hook() func(context.Context) error {
	return func(context.Context) error {
		r.request()
		return nil
	}
}
