package main

import (
	"fmt"
	"log/slog"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// newSingleUnitSupervisor runs exactly one unit in this process: what an isolated
// pod is.
//
// supervisor.Single holds one unit's credentials and nothing more — the selected
// member's own bot token, that member's key, this process's lore — and fixes session
// custody at session.ModeIsolated, where each member's key is wrapped under their own
// passphrase. That is the property the mode's whole claim rests on, so nothing here
// may hand it a second member's anything.
func newSingleUnitSupervisor(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.Supervisor, error) {
	sel := opts.selection
	single := supervisor.SingleOptions{
		Group: sel.group,
		// The same resolver doctor and the health check use: a pod whose token is
		// a file or a systemd credential must start, not just validate.
		Secrets:   e.secrets(),
		Logger:    logger,
		LookupEnv: e.env(),
		Now:       e.now,
		// TierWindows is deliberately not set; see tierWindows.
		TierWindows: tierWindows(cfg),
	}

	if !sel.group {
		m, ok := cfg.MemberByID(domain.MemberID(sel.member))
		if !ok {
			return nil, fmt.Errorf("no member %q in this household", sel.member)
		}
		single.Member = m.ID

		// Exactly this member, and nobody else. unlockSessions provisions and
		// unlocks whoever it is given, so handing it the whole household here
		// would have a pod create wrapped keys for other members under its own
		// passphrase — which is the isolation failure the mode exists to
		// prevent, arriving through the back door of a convenience.
		sessions, err := startSessions(e, cfg, logger, []domain.Member{m})
		if err != nil {
			return nil, err
		}
		single.Sessions = sessions
	}
	// The group pod deliberately gets no session manager of its own making: it
	// serves the shared space and holds no member key, so demanding a passphrase
	// for it would be asking for a secret that unwraps nothing.

	return supervisor.NewSingle(cfg, single)
}

// tierWindows would name the smallest context window, in the assistant's estimated
// tokens, of any endpoint tagged with each tier — the per-conversation context budget
// is derived from it as the minimum across that conversation's own chain.
//
// It returns nil, because there is nothing to build it from. kenward.yaml's
// endpoints carry a name, a base URL, a model, tags and a timeout, and no context
// window; internal/config has no field for one and nothing else in the module knows
// a model's window either. Guessing from the model string would be worse than the
// fallback: too high silently truncates the oldest turns of every conversation on
// that tier, and too low wastes a machine somebody bought for the size of its
// window. Both are invisible until somebody notices the assistant has become
// forgetful.
//
// Left nil, every unit takes assistant.DefaultContextBudget, which is the documented
// fallback. Giving this a real answer means adding a context_window to
// config.EndpointConfig — that is internal/config's decision, not this layer's.
func tierWindows(*config.Config) map[string]int { return nil }
