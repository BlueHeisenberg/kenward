// Package supervisor runs the household's units and is the only place that knows which
// mode kenward is in.
//
// A unit is one member's assistant, or the household group's. In simple mode every unit
// runs as a goroutine in this process; in isolated mode each runs as its own pod with
// its own key and its own bot token. The unit implementation is identical either way —
// that is the property the whole design rests on, and if a change to a unit ever needs
// to ask which mode it is in, that change is wrong.
//
// This package wires concrete implementations together and is the only package
// permitted to import all of them.
package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// State is what a unit is currently doing, as far as the supervisor can tell.
type State int

const (
	// StateUnknown is the zero value and means the supervisor has no information,
	// which is different from knowing the unit is down.
	StateUnknown State = iota
	// StateStarting means the unit has been asked to run but has not reported ready.
	StateStarting
	// StateReady means the unit is serving.
	StateReady
	// StateStopped means the unit exited or was stopped deliberately.
	StateStopped
	// StateFailed means the unit exited unexpectedly and, in isolated mode, is
	// subject to restart.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// UnitHealth reports one unit's condition. It is what `kenward doctor` renders and what
// the updater's health check consults, so it must describe the unit itself and never
// the reachability of anything external: household inference machines are legitimately
// powered off, and treating that as unhealthy would make a good update roll back
// forever.
type UnitHealth struct {
	// Member is empty for the household group's unit.
	Member domain.MemberID
	// Group is true for the household group's unit.
	Group bool
	State State
	// Since is when the unit entered its current state.
	Since time.Time
	// Restarts counts unexpected exits since the supervisor started.
	Restarts int
	// Err is the last unexpected failure, if any. It is retained after a successful
	// restart so an operator can see that something went wrong even once it is
	// working again.
	Err error
}

// Healthy reports whether the unit is serving.
func (u UnitHealth) Healthy() bool { return u.State == StateReady }

// ErrUnsupportedMode is returned when a mode cannot run on this host — in practice,
// isolated mode anywhere other than Linux, where the container runtime that provides
// the isolation does not exist. It is an error rather than a silent downgrade: a
// household that asked for sealed memory and quietly got shared memory would be worse
// off than one that was refused.
var ErrUnsupportedMode = errors.New("supervisor: mode not supported on this host")

// ErrNoUnits is returned when a configuration produces nothing to run.
var ErrNoUnits = errors.New("supervisor: no units to run")

// Supervisor starts, stops and reports on the household's units.
//
// Start blocks until ctx is cancelled or a fatal error occurs. Stop drains: it stops
// accepting new messages, lets in-flight turns finish, locks every session and returns.
// Health may be called at any time, including before Start and after Stop.
type Supervisor interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Health(ctx context.Context) ([]UnitHealth, error)
}
