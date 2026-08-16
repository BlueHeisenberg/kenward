package updater

import (
	"context"
	"errors"
	"time"

	keelupdate "github.com/BlueHeisenberg/keel/update"
)

// Run polls for updates every check interval — first check immediately — and
// applies eligible releases. It blocks until ctx is cancelled, returning
// ctx.Err(); until a swap completes with no Restart hook configured, returning
// keel's ErrRestartPending so the caller restarts the process; or, on a nil
// receiver or an off channel, immediately.
//
// `off` is genuinely off: Run logs that once and returns nil without
// constructing a request, resolving a URL, or touching the network. It is a
// fully supported permanent state, not a degraded one.
//
// Every failure — an unreachable manifest host, a bad signature, a refused
// preflight, a lost lock — is logged and retried on the next cycle. Run never
// stops because updating failed, and the household keeps working on the
// version it has; the one thing that ends the loop besides cancellation is a
// successful swap, which by design ends the process too.
//
// This is deliberately kenward's own loop rather than keel's Run, which since
// keel v0.4.0 covers most of what it once could not (a declined release is
// remembered, an unanswered one re-asked). Three things keep the local loop
// earning its keep. First, recovery: when the swap fails after a successful
// drain, keel's Run logs and waits out the interval while the household sits
// drained and silent; this loop restarts the process so it resumes serving
// immediately (see runOnce). Second, a release that requires consent when no
// consent path is wired is remembered here and warned about once, not every
// cycle forever. Third, testability: keel's Config.Now drives its policy
// clock but Run's tick still elapses in wall-clock time, so the scheduling
// behaviour this package promises is only testable against an injectable
// tick, which keel's Run cannot take. If keel ever grows all three, this loop
// should be deleted in favour of keel's.
//
// Processes sharing one install path (several pods running off one mounted
// binary) coordinate through keel's cross-process lock: exactly one applies,
// the siblings see ErrLocked or ErrUpdateInProgress and skip the cycle
// quietly. That serialisation is per install path, not per household — pods
// with private copies of the binary (each container's own image filesystem)
// never contend, and each updates independently on its own cycle. Sequencing
// isolated-mode pods "one member at a time" therefore cannot be expressed
// from inside this package at all; it belongs to whatever owns the pods'
// shared artifact — the image and the process that rolls it — and pretending
// otherwise here would be sequencing theatre. That owner is the host
// supervisor, and supervisor.Isolated does it: on the start after this binary
// was replaced, it finds the image it would now run differs from the one it
// recorded and rolls the pods onto it, one at a time. See
// docs/IMPLEMENTATION.md §9.
func (s *Scheduler) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.channel == keelupdate.ChannelOff {
		s.log.Info("updater: updates are off; kenward will never fetch, check or replace anything, and works indefinitely this way")
		return nil
	}
	delay := time.Duration(0) // first check immediately
	for {
		if err := s.wait(ctx, delay); err != nil {
			return err
		}
		delay = s.interval
		if err := s.runOnce(ctx); err != nil {
			return err
		}
	}
}

// runOnce performs one check-and-maybe-apply cycle. It returns nil to keep
// the loop going — including on every failure, which is logged and retried
// next cycle — and an error only when the loop must end: ErrRestartPending,
// whose meaning is "the binary on disk is new; restarting is the caller's
// job".
func (s *Scheduler) runOnce(ctx context.Context) error {
	st, err := s.up.Check(ctx)
	if err != nil {
		// Includes hostile-input refusals (bad signature, stale manifest) and
		// plain outages alike: nothing changed on disk, nothing stops, the
		// household keeps working, and the next cycle tries again.
		s.log.Warn("updater: check failed; will retry next cycle", "error", err)
		return nil
	}
	if !st.Available {
		s.log.Debug("updater: no eligible update", "current", st.Current, "latest", st.Latest, "reason", st.Reason)
		return nil
	}
	ver := st.Release.Version
	if why, ok := s.declined[ver]; ok {
		s.log.Debug("updater: release previously refused; not re-attempting", "version", ver, "reason", why)
		return nil
	}

	s.drained = false
	applyErr := s.up.Apply(ctx, *st.Release)
	switch {
	case applyErr == nil:
		// The Restart hook ran and returned; the journal keel wrote blocks a
		// second Apply until Resume finishes this one after the restart.
		s.log.Info("updater: update applied; restart under way", "from", st.Current, "to", ver)
		return nil
	case errors.Is(applyErr, keelupdate.ErrRestartPending):
		s.log.Info("updater: binary swapped; the caller owns the restart, and Resume finishes the update on next start", "from", st.Current, "to", ver)
		return applyErr
	case errors.Is(applyErr, keelupdate.ErrConsentDeclined):
		s.declined[ver] = "declined by the household"
		s.log.Info("updater: household declined; will not ask again for this version", "version", ver)
	case errors.Is(applyErr, keelupdate.ErrConsentRequired):
		// No Consenter was wired. That cannot change while this process runs,
		// so remember the version rather than logging the same warning every
		// six hours forever.
		s.declined[ver] = "requires consent but no consent path is wired"
		s.log.Warn("updater: release requires consent but no consent path is wired; it will not be applied", "version", ver)
	case errors.Is(applyErr, keelupdate.ErrConsentUnanswered):
		// Deliberately NOT remembered: silence is a refusal, not a decision.
		// The household is asked again next cycle.
		s.log.Info("updater: consent request went unanswered; treating as no and asking again next cycle", "version", ver)
	case errors.Is(applyErr, keelupdate.ErrLocked), errors.Is(applyErr, keelupdate.ErrUpdateInProgress):
		s.log.Debug("updater: a sibling process is handling this update; skipping the cycle", "version", ver)
	default:
		s.log.Warn("updater: apply failed; the household stays on the version it has", "version", ver, "error", applyErr)
	}

	// Since keel v0.4.0 the cross-process lock is taken BEFORE the drain, so
	// losing the lock no longer silences anyone. What remains is the swap
	// itself failing after a successful drain — disk trouble, a journal that
	// would not write — which leaves the household stopped for an update that
	// never happened. A drained household with no restart coming is silent
	// until a human notices, which violates "an update must never take the
	// assistant down" — so restart anyway: the process comes back on whatever
	// binary is at the target path (still the old one; a failed swap changes
	// nothing) and resumes serving.
	if s.drained && s.restart != nil {
		s.log.Warn("updater: household was drained for an update that did not complete; restarting so it resumes serving")
		if rerr := s.restart(ctx); rerr != nil {
			s.log.Warn("updater: restart after an incomplete update failed", "error", rerr)
		}
	}
	return nil
}

// Resume finishes whatever update was in flight when the process last
// stopped: it health-checks a freshly swapped binary and commits it, rolls
// back to the retained previous binary on failure, and repairs a crash
// mid-swap. Call it early on every startup, before serving traffic — it is
// the half of the automatic-rollback promise that runs in the NEW binary, and
// skipping it leaves a bad build installed with nothing ever deciding its
// fate.
//
// Resume works even when the channel is off: it fetches nothing and touches
// only local state, and an update already in flight when updates were turned
// off still deserves to be committed or rolled back rather than abandoned.
// keel's ErrLocked means a sibling process is resuming the same install path;
// retry shortly. ErrRestartPending after a rollback means the caller must
// restart onto the restored binary. A nil receiver reports OutcomeNone.
func (s *Scheduler) Resume(ctx context.Context) (keelupdate.ResumeReport, error) {
	if s == nil {
		return keelupdate.ResumeReport{Outcome: keelupdate.OutcomeNone}, nil
	}
	rep, err := s.up.Resume(ctx)
	switch rep.Outcome {
	case keelupdate.OutcomeCommitted:
		s.log.Info("updater: update committed", "from", rep.From, "to", rep.To)
	case keelupdate.OutcomeRolledBack:
		s.log.Warn("updater: update rolled back", "from", rep.From, "to", rep.To, "reason", rep.Reason)
	case keelupdate.OutcomeAborted:
		s.log.Warn("updater: update aborted", "from", rep.From, "to", rep.To, "reason", rep.Reason)
	}
	return rep, err
}

// sleepWait is the production waitFunc: a plain timer that yields to
// cancellation. A non-positive delay waits not at all (the first check is
// immediate) but still observes a cancelled context.
func sleepWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
