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
	single, err := singleUnitOptions(e, cfg, opts, logger)
	if err != nil {
		return nil, err
	}
	return supervisor.NewSingle(cfg, single)
}

// singleUnitOptions is the wiring newSingleUnitSupervisor hands to
// supervisor.NewSingle, separated from the construction so a test can put fakes on
// the three edges — Telegram, lore, the endpoints — and still exercise the wiring
// this file decides. Everything the pod's behaviour depends on is decided here.
func singleUnitOptions(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.SingleOptions, error) {
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
			return supervisor.SingleOptions{}, fmt.Errorf("no member %q in this household", sel.member)
		}
		single.Member = m.ID

		// Exactly this member, and nobody else. unlockSessions provisions and
		// unlocks whoever it is given, so handing it the whole household here
		// would have a pod create wrapped keys for other members under its own
		// passphrase — which is the isolation failure the mode exists to
		// prevent, arriving through the back door of a convenience.
		sessions, onEnrol, err := startSessions(e, cfg, logger, []domain.Member{m})
		if err != nil {
			return supervisor.SingleOptions{}, err
		}
		single.Sessions = sessions
		// The same rule, extended past startup. A member selected here who has
		// not claimed yet has no key to unlock — there is nothing to provision
		// for somebody who may never arrive — so the provisioning happens when
		// their claim binds, in their own pod, under their own passphrase. The
		// supervisor calls this for this pod's member and for nobody else; a
		// claim for anyone else that lands on this bot is bound and left to
		// their own pod, keyless here.
		single.UnlockOnEnrol = onEnrol

		// And the claim that hook completes has to be able to arrive. D-023: this
		// member's bot exists before they claim, and the claim happens in a
		// conversation with it rather than the household's, so the pod of a member
		// who has not claimed comes up claim-only and waits for the code. Without a
		// claimer it refuses to start instead, and the code has nowhere to go.
		//
		// It is the same claimer the simple-mode node builds, over the same invite
		// store `kenward invite` mints into — one enrolment path, not a second one
		// for pods. NewSingle ignores it for a member who has already claimed, and
		// a claim for anybody else that lands on this bot is bound and left keyless
		// and unitless here, for their own pod to serve.
		claimer, err := newClaimer(cfg)
		if err != nil {
			return supervisor.SingleOptions{}, err
		}
		single.Enrol = claimer
	}
	// The group pod deliberately gets no session manager of its own making: it
	// serves the shared space and holds no member key, so demanding a passphrase
	// for it would be asking for a secret that unwraps nothing. It gets no claimer
	// either: D-023 puts the claim conversation on the member's own bot, and a
	// household bot that accepted codes would carry the start of exactly the
	// relationship isolated mode exists to keep off it.

	return single, nil
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
