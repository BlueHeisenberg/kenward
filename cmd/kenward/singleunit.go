package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// errNoSingleUnitConstructor is what `run --member ID` and `run --group` produce
// until internal/supervisor exposes the in-pod runner.
//
// TODO(cmd/kenward): wire this to internal/supervisor's single-unit constructor.
// At the time of writing the package exports NewSimple and NewIsolated and nothing
// that runs exactly one unit in this process. That third thing is what an isolated
// pod needs: NewSimple refuses any configuration whose mode is not `simple`, and
// NewIsolated is the host side that starts the pods rather than anything that runs
// inside one.
//
// The obvious workaround — narrow the configuration to one member, rewrite its mode
// to `simple` and call NewSimple — is deliberately not taken. NewSimple builds its
// session manager in session.ModeSimple, which wraps every member's key under one
// operator-held passphrase; isolated mode's whole claim rests on session.ModeIsolated,
// where each member's key is wrapped under their own. A pod that ran with the simple
// key custody would tell its member their memory was sealed against the operator
// while it was not, which is precisely the substitution this product exists to refuse.
// Failing loudly is the only honest behaviour until the constructor exists.
//
// When it lands, this path must also call startSessions and inject the unlocked
// manager, exactly as the simple path does. A pod that starts without unlocking its
// member's key answers every message that member sends with the locked notice, and
// there is no group chat inside a member's pod to make the failure visible.
var errNoSingleUnitConstructor = errors.New(
	"running a single unit in this process is not wired yet: internal/supervisor exposes " +
		"NewSimple (every unit, simple mode) and NewIsolated (the host that starts pods), " +
		"but nothing that runs one unit inside a pod")

// newSingleUnitSupervisor runs exactly one unit in this process: what an isolated
// pod is.
func newSingleUnitSupervisor(_ *env, cfg *config.Config, opts runOptions, _ *slog.Logger) (supervisor.Supervisor, error) {
	if cfg.Mode != config.ModeIsolated {
		return nil, fmt.Errorf("a single unit runs only in isolated mode; this configuration says %q", cfg.Mode)
	}
	return nil, fmt.Errorf("cannot run %s: %w", opts.selection.label(), errNoSingleUnitConstructor)
}
