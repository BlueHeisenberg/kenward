// Package updater schedules kenward's self-update checks and wires the
// household's real behaviour into keel/update's hooks.
//
// Threat model, in one paragraph: an auto-updater inside a process holding
// household keys is a remote code execution channel built on purpose, so the
// only authority that can put new code on a household's machine is an Ed25519
// release signature verified against keys compiled into this binary — never
// the update host, never the network path to it, never this scheduler.
// Everything past signature verification is availability engineering: a failed
// update must leave a working household on the version it already has, and
// nothing in this package may prevent kenward from starting or serving.
// keel/update's package documentation states the full model — what a hostile
// update host can and cannot do, replay bounds, downgrade refusal, key custody
// — and this package defers to it entirely. What is added here is kenward's
// side of the bargain: when checks happen, what health, drain and consent mean
// for a household, and one bound keel leaves off by default, the manifest age
// limit (DefaultMaxManifestAge).
//
// A Scheduler that cannot be constructed is a warning and a household that
// does not auto-update; it is never a refusal to start. Run and Resume on a
// nil *Scheduler are no-ops for exactly that reason: a caller may log the
// construction error and go on serving without guarding every call site.
package updater

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	keelupdate "github.com/BlueHeisenberg/keel/update"

	"github.com/BlueHeisenberg/kenward/internal/version"
)

// Defaults applied by New when the corresponding Options field is zero. They
// are exported so `kenward doctor` can say what silence buys.
const (
	// DefaultMaxManifestAge bounds replay of an old, still-correctly-signed
	// manifest. keel/update leaves this off by default, which means a
	// compromised or frozen update host could serve a stale manifest forever
	// and hide newer releases indefinitely (it can never downgrade anyone —
	// keel refuses that regardless — but a hidden security fix is bad enough).
	//
	// The value must comfortably exceed the slowest expected release cadence:
	// a manifest older than the limit is refused as a suspected replay even
	// when it is merely the latest release being old, so a limit shorter than
	// a normal quiet stretch between releases would make every long-lived
	// installation refuse a perfectly honest manifest. kenward is maintained
	// by one person and months can legitimately pass between releases; half a
	// year is longer than any normal gap while still guaranteeing a frozen
	// manifest is eventually detected rather than trusted forever.
	DefaultMaxManifestAge = 182 * 24 * time.Hour

	// DefaultStableDelay is how long a release must have been published
	// before the stable channel applies it. The maintainer's own household
	// runs edge and takes releases immediately; three days is long enough for
	// that household to live with a release — through a weekend of real
	// conversations — before everyone else's takes it.
	DefaultStableDelay = 72 * time.Hour

	// DefaultConsentTimeout bounds one consent request. The household is
	// asked over its own transport and people answer when they see it; an
	// hour is generous without holding the check cycle hostage. An unanswered
	// request is a refusal, and the household is simply asked again on the
	// next cycle — see keel's DecisionUnanswered.
	DefaultConsentTimeout = time.Hour
)

// defaultCheckInterval mirrors config.DefaultCheckInterval and keel's own
// Config.CheckInterval default. This package deliberately does not import
// internal/config — the caller maps kenward.yaml's update section onto
// Options — so the value is restated here rather than referenced.
const defaultCheckInterval = 6 * time.Hour

// Options wires a Scheduler. ManifestURL and Keys are required unless the
// channel is off; every hook is optional, with the consequences each field's
// comment states. The caller maps kenward.yaml's update section onto Channel
// and CheckInterval (config.UpdateChannel's values are keel's channel values
// verbatim); this package takes the plain values so it depends on keel and
// nothing else in kenward but version.
type Options struct {
	// Channel is stable, edge, or off — cfg.Update.Channel. Empty defaults to
	// stable, exactly as config.Load and keel both would.
	Channel keelupdate.Channel
	// CheckInterval is how often the node looks for a new release —
	// cfg.Update.CheckInterval. Zero or negative defaults to six hours.
	// Ignored when the channel is off.
	CheckInterval time.Duration

	// ManifestURL serves the signed manifest envelope. Required unless the
	// channel is off.
	ManifestURL string
	// Keys are the trusted Ed25519 release public keys compiled into this
	// binary. Required unless the channel is off: a scheduler with no trusted
	// key cannot verify a signature, and an unverified update is remote code
	// execution with extra steps.
	Keys []ed25519.PublicKey
	// CurrentVersion is the running build's version. Empty means
	// version.Version, which is what production wants; tests set it.
	CurrentVersion string

	// Consent carries the decision on a major version bump, or on any release
	// flagged securitySensitive, to a human. Nil is allowed and means such
	// releases are never applied by this scheduler — logged once per version,
	// then left alone — while patch and minor releases keep applying. See
	// Consenter for what an implementation owes.
	Consent Consenter
	// Drain blocks until no member has a turn in flight; it runs after the
	// artifact is verified and immediately before the swap. Nil skips the
	// wait, which is only acceptable where nothing is serving (a CLI
	// invocation). See Drainer for what an implementation owes.
	Drain Drainer
	// Restart restarts the process after a swap or a rollback — in
	// production, exit and let the supervisor (systemd, compose) bring the
	// process back up. Nil means Run returns keel's ErrRestartPending after a
	// successful swap and the caller owns the restart; the update is then
	// finished by Resume on the next start.
	Restart func(ctx context.Context) error
	// Health supplies the probes behind the post-restart health check. The
	// zero value passes vacuously — "the process restarted and reached this
	// code" — which is the minimum; production supplies both probes. See
	// HealthProbes for what may NEVER appear here.
	Health HealthProbes

	// MaxManifestAge bounds signed-manifest replay. Zero means
	// DefaultMaxManifestAge; negative disables the bound, which is keel's own
	// default and not recommended.
	MaxManifestAge time.Duration
	// StableDelay is how long a release must have been published before the
	// stable channel applies it. Zero means DefaultStableDelay; negative
	// disables the delay. Ignored on other channels.
	StableDelay time.Duration
	// ConsentTimeout bounds one consent request. Zero means
	// DefaultConsentTimeout.
	ConsentTimeout time.Duration

	// HTTPClient overrides the client used for manifest and artifact fetches.
	// Nil means http.DefaultClient.
	HTTPClient *http.Client
	// Logger receives every check, apply, refusal and failure. Nil discards.
	// Nothing logged here ever includes a token or a key: the probes and the
	// Consenter close over their own credentials and this package never sees
	// them.
	Logger *slog.Logger

	// Unexported test seams, settable only from this package's tests.
	//
	// targetPath overrides the binary to replace (keel resolves the running
	// executable when empty). skipPreflight disables executing the staged
	// binary before the swap — production never sets it: tests swap text
	// fixtures, which cannot be executed. now feeds keel's Config.Now, the
	// published clock seam behind every time-based policy decision (stable
	// delay, manifest freshness, lock staleness). wait overrides the
	// inter-check sleep: keel's Now deliberately does not drive Run's tick —
	// that interval elapses in wall-clock time — so scheduling tests need
	// this seam even with Now available.
	targetPath    string
	skipPreflight bool
	now           func() time.Time
	wait          waitFunc
}

// waitFunc sleeps for d or until ctx is done, returning ctx.Err() on
// cancellation and nil on a completed wait. It is the scheduler's clock seam.
type waitFunc func(ctx context.Context, d time.Duration) error

// Scheduler owns the periodic update check: it polls keel/update on the
// configured interval, applies eligible releases under the household's own
// consent, drain and health behaviour, and retries every failure on the next
// cycle. It never stops on failure and never takes the assistant down: an
// update that cannot be applied leaves the household working on the version
// it has.
//
// A Scheduler is single-use: construct, optionally Resume, then Run once.
type Scheduler struct {
	up       *keelupdate.Updater
	channel  keelupdate.Channel
	interval time.Duration
	log      *slog.Logger
	wait     waitFunc
	restart  func(ctx context.Context) error
	health   keelupdate.HealthCheck
	// target is the binary keel would replace, resolved the same way keel resolves
	// it. Held so Resume can ask whether there is any update state beside it before
	// keel reaches for the cross-process lock — see pendingUpdate.
	target string

	// declined remembers, per release version, why it will not be
	// re-attempted: an explicit "no" from the household, or a
	// consent-requiring release with no consent path wired. An unanswered
	// consent request is deliberately NOT recorded here — silence is not a
	// decision, so the household is asked again next cycle.
	declined map[string]string

	// drained is set by the drain hook within one Apply attempt. If Apply
	// then fails — the cross-process lock lost, the swap refused — the
	// household has been drained for an update that never happened, so
	// runOnce restarts the process to bring it back to serving rather than
	// leaving it silent until someone notices.
	drained bool
}

// New builds a Scheduler over the household's configuration.
//
// A construction error is a reason to run without auto-update, never a reason
// to refuse to start: callers log it as a warning and go on. The nil
// *Scheduler they are left with is safe to Run and Resume.
func New(opts Options) (*Scheduler, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	channel := opts.Channel
	if channel == "" {
		channel = keelupdate.ChannelStable
	}
	interval := opts.CheckInterval
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	current := opts.CurrentVersion
	if current == "" {
		current = version.Version
	}
	maxAge := opts.MaxManifestAge
	switch {
	case maxAge == 0:
		maxAge = DefaultMaxManifestAge
	case maxAge < 0:
		maxAge = 0
	}
	stableDelay := opts.StableDelay
	switch {
	case stableDelay == 0:
		stableDelay = DefaultStableDelay
	case stableDelay < 0:
		stableDelay = 0
	}
	consentTimeout := opts.ConsentTimeout
	if consentTimeout <= 0 {
		consentTimeout = DefaultConsentTimeout
	}
	wait := opts.wait
	if wait == nil {
		wait = sleepWait
	}

	s := &Scheduler{
		channel:  channel,
		interval: interval,
		log:      log,
		wait:     wait,
		restart:  opts.Restart,
		declined: make(map[string]string),
		target:   resolveTarget(opts.targetPath),
	}
	s.health = healthCheck(opts.Health)

	cfg := keelupdate.Config{
		ManifestURL:    opts.ManifestURL,
		Keys:           opts.Keys,
		Channel:        channel,
		Current:        current,
		TargetPath:     opts.targetPath,
		StableDelay:    stableDelay,
		CheckInterval:  interval,
		MaxManifestAge: maxAge,
		// The staged binary is executed with these before the swap and must
		// exit 0. `version` touches nothing: no configuration, no lore, no
		// network — see docs/RELEASING.md's pre-publish checklist, which runs
		// exactly this on every artifact.
		PreflightArgs: []string{"version"},
		SkipPreflight: opts.skipPreflight,
		Now:           opts.now,
		HTTPClient:    opts.HTTPClient,
		Logger:        log,
		Health:        s.health,
		Drain:         s.drainHook(opts.Drain),
		Restart:       opts.Restart,
	}
	if opts.Consent != nil {
		cfg.Consent = consentHook(opts.Consent, consentTimeout)
	}
	up, err := keelupdate.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("updater: %w", err)
	}
	s.up = up
	return s, nil
}

// resolveTarget is the binary keel/update will replace: the configured path, or the
// running executable with its symlinks followed, exactly as keel resolves it. An
// unresolvable executable is not an error here — keel tolerates one when the channel is
// off, and the empty string simply means there is nothing beside it to inspect.
func resolveTarget(configured string) string {
	if configured != "" {
		return configured
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		return resolved
	}
	return exe
}

// drainHook wraps the household's drain so runOnce can tell whether a failed
// Apply left the household drained. It records first: a drain that errors may
// still have stopped intake (the supervisor's drain cuts turns when its
// context expires), and the recovery restart must cover that case too.
func (s *Scheduler) drainHook(d Drainer) keelupdate.Drain {
	if d == nil {
		return nil
	}
	return func(ctx context.Context) error {
		s.drained = true
		return d.Drain(ctx)
	}
}

// consentHook adapts a Consenter to keel's Consent contract, which since
// keel v0.4.0 speaks the same three-valued Decision this package needs:
// approved applies, declined is remembered per version, unanswered fails
// closed for the cycle and is asked again on the next one. The adapter adds
// only two things — a timeout, so a consent request left hanging cannot hold
// the check loop forever, and the rule that a timeout is silence rather than
// a fault: the household not looking at Telegram for an hour is normal life,
// so it maps to DecisionUnanswered with no error, while a genuine delivery
// failure passes through as an error for the caller's transport to explain.
func consentHook(c Consenter, timeout time.Duration) keelupdate.Consent {
	return func(ctx context.Context, req keelupdate.ConsentRequest) (keelupdate.Decision, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		decision, err := c.RequestConsent(cctx, req)
		if err != nil {
			if cctx.Err() != nil {
				// The bound above expired while the question was pending.
				// Nobody answered; nothing is wrong.
				return keelupdate.DecisionUnanswered, nil
			}
			return keelupdate.DecisionUnanswered, err
		}
		return decision, nil
	}
}
