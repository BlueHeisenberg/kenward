package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// defaultPodImageRepo is the repository the isolated host supervisor starts pods
// from when --image is not given.
//
// internal/supervisor requires an image and offers no default, deliberately: there
// is no sensible default for the artifact a household's private conversations run
// inside. What is defensible is running pods of the same build as the host process,
// which is what this produces, and an operator who wants otherwise says so with
// --image. A `dev` build has no published tag, so it is refused rather than guessed
// at.
const defaultPodImageRepo = "ghcr.io/blueheisenberg/kenward"

// defaultSupervisor picks the supervisor from the configuration's mode and from
// whether this process was asked to be one unit.
//
// The three cases are distinct and none of them may quietly become another:
//
//   - simple, no unit selection: every unit as a goroutine in this process.
//   - isolated, no unit selection: the host supervisor, one pod per member. Linux
//     only; anywhere else it refuses rather than downgrading.
//   - isolated, one unit selected: this process is inside a pod and is that one
//     unit.
//
// simple with a unit selection is rejected before we get here, in cmdRun, because it
// is a usage error rather than a wiring decision.
func defaultSupervisor(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.Supervisor, error) {
	switch {
	case cfg.Mode == config.ModeIsolated && opts.selection.single():
		return newSingleUnitSupervisor(e, cfg, opts, logger)

	case cfg.Mode == config.ModeIsolated:
		image := opts.image
		if image == "" {
			v := version.Short()
			if v == "" || v == "dev" {
				return nil, fmt.Errorf("isolated mode needs a pod image and this is a %q build, "+
					"which has no published tag: pass --image REF", v)
			}
			image = defaultPodImageRepo + ":" + v
		}
		return supervisor.NewIsolated(cfg, supervisor.IsolatedOptions{
			Image: image,
			// The same resolver this command's own checks use, so a pod's token
			// is read from whichever source the configuration names rather than
			// from the environment alone.
			Secrets:   e.secrets(),
			Logger:    logger,
			LookupEnv: e.env(),
			Now:       e.now,
		})

	default:
		claimer, err := newClaimer(cfg)
		if err != nil {
			return nil, err
		}
		// The session manager is built here rather than left for the supervisor
		// to build, because it has to be unlocked before any unit runs. A manager
		// nothing ever unlocks holds no key, and a node holding no key answers
		// every direct message with the locked notice while its group chat works
		// perfectly — which is the shape of failure nobody diagnoses.
		sessions, err := startSessions(e, cfg, logger, cfg.DomainMembers())
		if err != nil {
			return nil, err
		}
		return supervisor.NewSimple(cfg, supervisor.SimpleOptions{
			// Enrol is supplied so claim codes work while the household runs:
			// without it a member who has been handed a code has nothing to
			// present it to until the operator restarts the node.
			Enrol:       claimer,
			Sessions:    sessions,
			TierWindows: tierWindows(cfg),
			Secrets:     e.secrets(),
			Logger:      logger,
			LookupEnv:   e.env(),
			Now:         e.now,
		})
	}
}

// startSessions reads the passphrase, provisions any of the given members who has no
// key yet, unlocks them, and hands back the manager the units will use.
//
// members is passed in rather than read from cfg because the two callers serve
// different sets: simple mode serves the whole household from one process, and a pod
// serves exactly one member. Handing a pod the household's member list would have it
// provision keys for people it must never hold.
func startSessions(e *env, cfg *config.Config, logger *slog.Logger, members []domain.Member) (session.Sessions, error) {
	pass, err := readPassphrase(e)
	if err != nil {
		if errors.Is(err, errNoPassphrase) {
			return nil, errNoPassphrase
		}
		return nil, err
	}
	defer pass.zero()

	store := session.NewFileStore(sessionStorePath(cfg))
	mgr, err := session.NewManager(sessionMode(cfg.Mode), store,
		session.WithIdleTimeout(cfg.Session.IdleTimeout.Duration()))
	if err != nil {
		return nil, fmt.Errorf("building the session manager: %w", err)
	}

	rep, err := unlockSessions(e.context(), mgr, store, members, pass)
	if err != nil {
		mgr.Close()
		// session.ErrBadPassphrase is deliberately indistinguishable from an
		// unknown member, so this cannot say which of the two it was — only that
		// the passphrase this node was given does not open what is on disk.
		if errors.Is(err, session.ErrBadPassphrase) {
			return nil, fmt.Errorf("the passphrase from %s does not unwrap the keys in %s.\n"+
				"kenward will not start with keys it cannot open: it would answer every private\n"+
				"message with the locked notice", pass.source, store.Path())
		}
		return nil, err
	}
	// Ids only. The passphrase appears in no log line, here or anywhere.
	logger.Info("kenward",
		"event", "sessions",
		"unlocked", len(rep.Unlocked),
		"provisioned", len(rep.Provisioned),
		"custody", sessionMode(cfg.Mode).String(),
		"source", pass.source)
	return mgr, nil
}

// newClaimer builds the enrolment claimer the running node uses to process claim
// codes from senders it does not yet serve.
func newClaimer(cfg *config.Config) (*enrol.Claimer, error) {
	binder, err := newBinder(cfg)
	if err != nil {
		return nil, err
	}
	c, err := enrol.New(inviteStore(cfg), binder)
	if err != nil {
		return nil, fmt.Errorf("building the enrolment claimer: %w", err)
	}
	return c, nil
}
