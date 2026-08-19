package main

import (
	"context"
	"log/slog"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/link"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// startLink runs this pod's half of the household link handshake for as long as it
// serves, and returns the function that stops it.
//
// It is the last manual step in isolated mode, removed. The group's pod owns the
// household's shared space and each member's pod has to be admitted to it; that
// used to be `lore space invite` in one container and `lore join` in the other,
// typed by a person, once per member. Now the group's pod runs a desk and each
// member's pod asks it, both authenticated with `household.link_key`. Nobody runs
// anything and no command is executed inside any pod — see internal/link, which
// makes the argument for why kenward may do this and lore may not.
//
// # Which units get one
//
// The same rule as the sync daemon, for the same reason: an isolated pod and
// nothing else. A simple-mode node has one lore home holding every space, so there
// is no second store to admit and nothing to link. The isolated host supervisor
// holds no lore home at all.
//
// A member's pod stops as soon as it holds the space, which after the first boot
// is one local lookup. The group's pod serves for the life of the process, because
// a member added to a running household is a pod that turns up later and has to
// find a desk.
//
// # Failure is never fatal
//
// Unchanged from the daemon's rule and for the same reason: private memory works
// without any of this, and refusing a household its assistant over a desk that
// would not bind trades a partial outage for a total one. What tells an operator
// is `kenward doctor`, which reports whether this pod holds the shared space.
//
// A household with no `household.link_key` gets no handshake and no error. That is
// every configuration written before the key existed, and those households are
// already linked by the manual recipe; there is nothing for this to do in one and
// nothing to complain about. `doctor` says so where it matters.
func startLink(e *env, cfg *config.Config, sel unitSelection, mem memory.Memory, logger *slog.Logger) func() {
	noop := func() {}
	client, ok := mem.(*memory.Client)
	switch {
	case cfg.Mode != config.ModeIsolated || !sel.single() || !ok || client == nil:
		return noop
	case cfg.Household.SharedSpace == "":
		return noop
	}
	key, err := cfg.LinkKey(e.secrets())
	switch {
	case err != nil:
		logger.Warn("kenward", "event", "link",
			"detail", "the household link key could not be read, so this pod cannot be linked into the household's shared space automatically",
			"err", err.Error())
		return noop
	case !key.IsSet():
		logger.Info("kenward", "event", "link",
			"detail", "no household.link_key is configured; membership of the household's shared space stays a manual step here")
		return noop
	}

	opts := linkOptions(cfg, client, logger, key.Value())
	run := link.Join
	if sel.scope().Group {
		// The group's pod created the space, so it has nothing to join and
		// everything to answer.
		run = link.Serve
	}

	ctx, cancel := context.WithCancel(e.context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := run(ctx, opts); err != nil {
			logger.Error("kenward", "event", "link",
				"detail", "the household link handshake did not start; this household's shared memory will only reach "+
					"the pods already in it, and `kenward doctor` reports which those are",
				"err", err.Error())
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// linkOptions is what a pod asks internal/link for, split out from startLink so it
// can be asserted without binding anything.
//
// The decision it holds is LoopbackOnly and NoDiscovery both staying false. A pod's
// siblings are separate network namespaces on the container runtime's bridge, not
// on its loopback, and a pod's address is not knowable when its spec is built — so
// a desk on loopback would answer nobody and a desk that did not advertise could
// not be found. The tests in internal/link set both, because two halves in one
// process are on each other's loopback already and a test binary must not advertise
// a household on the developer's LAN. Turning either off here would silently strand
// every real pod while every test still passed, which is what
// TestLinkAsksForTheLAN is for.
func linkOptions(cfg *config.Config, mem link.Memory, logger *slog.Logger, key string) link.Options {
	return link.Options{
		Space:  domain.SpaceID(cfg.Household.SharedSpace),
		Key:    []byte(key),
		Memory: mem,
		Logger: logger,
	}
}
