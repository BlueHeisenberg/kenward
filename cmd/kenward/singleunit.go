package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
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
		//
		// And the store it redeems against is this process's own, on this pod's own
		// volume — never the host's, which is a different file on a different
		// filesystem that nothing here can see. The codes the operator minted arrive
		// as the seed file --invites names, provisioned into this pod at create time
		// (supervisor.PodInvitesPath) or mounted there by the compose deployment, and
		// are imported here. Import rather than read: the mark that says a code is
		// spent is written to this pod's store, and the seed will never carry it.
		if err := importInvites(e, cfg, opts.invites, logger); err != nil {
			return supervisor.SingleOptions{}, err
		}
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

// importInvites merges the seed file at path into this unit's own invite store.
//
// A path that does not exist is no invites and not a failure: most pods have none
// outstanding, and a household that has never minted a code has no seed directory at
// all. A path that exists and cannot be read is a failure, because the alternative is
// a pod that comes up claim-only, waits for a code it will refuse, and says nothing —
// which is indistinguishable, from the member's side, from a bot that is ignoring them.
func importInvites(e *env, cfg *config.Config, path string, logger *slog.Logger) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil && info.IsDir():
		// Named on its own because of how it happens. A compose deployment bind-mounts
		// this path from the host, and a container runtime asked to mount a source
		// file that does not exist yet creates a directory there instead. The operator
		// brought the member's service up before minting their code; the fix is to
		// remove the directory and run `kenward invite` first.
		return fmt.Errorf("%s is a directory, not a file of outstanding invites: nothing has written one there\n"+
			"yet, and a container runtime asked to mount a file that does not exist creates a\n"+
			"directory in its place. Mint this member's code with `kenward invite` before starting\n"+
			"their container, and remove the directory", path)
	}
	n, err := copyInvites(e.context(), inviteStore(cfg), enrol.NewFileStore(path), nil)
	if err != nil {
		return fmt.Errorf("importing outstanding invites from %s: %w", path, err)
	}
	if n > 0 && logger != nil {
		// The count and nothing else. Which codes are outstanding is the fact the
		// digests are kept 0600 to withhold, and a log line is the least private
		// place in a household.
		logger.Info("kenward", "event", "enrolment", "detail", "outstanding claim codes imported", "count", n)
	}
	return nil
}

// applyRevocation clears this unit's own binding when the host has recorded that its
// member was revoked, and reports nothing to do otherwise.
//
// This is the far end of the only channel a revocation has in isolated mode. The claim
// was redeemed here, in this pod, and the binding was written to the state file on this
// pod's own volume; `kenward revoke` on the host cannot reach that file and must not try,
// because a host that can write into a running member's volume is one edit from reading
// it back and that volume holds the member's wrapped key and their lore. So the host
// records the fact, the supervisor or the compose mount carries it in, and the clearing
// happens here — by the process that owns the file, in the one moment it is safe: before
// it starts serving anybody.
//
// The timestamp is what keeps it from being a standing order. A member revoked, invited
// again and claimed again holds a binding newer than the record, and this pod is
// recreated on every rolling update; undoing that second claim every time would make
// re-enrolment impossible for exactly the people who have been through this once.
//
// The configuration is corrected in memory as well as on disk, because it was loaded
// with the state file already merged into it and the caller is about to decide from it
// whether this pod has a member to serve.
func applyRevocation(e *env, cfg *config.Config, sel unitSelection, path string) error {
	if path == "" || sel.group || sel.member == "" {
		return nil
	}
	rec, ok, err := readRevocation(path)
	if err != nil {
		return fmt.Errorf("reading the revocation record at %s: %w", path, err)
	}
	if !ok {
		return nil
	}
	id := domain.MemberID(sel.member)
	if rec.MemberID != id {
		// The same refusal a pod makes when its configuration names a member it does
		// not serve, for the same reason: acting on this would unbind whoever this pod
		// is actually for, on the strength of a mount pointing at the wrong file.
		return fmt.Errorf("%s revokes member %q and this pod serves member %q; refusing to unbind\n"+
			"somebody the record does not name", path, rec.MemberID, id)
	}

	m, held := cfg.MemberByID(id)
	if !held || !m.Enrolled() || m.EnrolledAt.After(rec.RevokedAt) {
		// Nothing bound, or bound by a claim made after the revocation was recorded.
		return nil
	}
	binder, err := newBinder(cfg)
	if err != nil {
		return err
	}
	if _, err := binder.Unbind(e.context(), id); err != nil {
		return fmt.Errorf("applying the revocation recorded at %s: %w", path, err)
	}
	for i := range cfg.Members {
		if domain.MemberID(cfg.Members[i].ID) == id {
			cfg.Members[i].TelegramID = 0
			cfg.Members[i].EnrolledAt = time.Time{}
		}
	}
	return nil
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
