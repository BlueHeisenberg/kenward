package updater

import (
	"context"
	"fmt"

	keelupdate "github.com/BlueHeisenberg/keel/update"
)

// Consenter carries an update decision to a human where they actually are.
//
// keel/update requires consent for a major version bump and for any release
// flagged securitySensitive. A scheduled, unattended check has no terminal,
// so the question has to reach the household as a message — the version
// pair, the release notes, and a yes/no — over kenward's own transport. This
// package deliberately does not import the transport: the caller implements
// this interface (transport.Ask over the household chat is the intended
// shape) and the package stays testable with a fake.
//
// The decision vocabulary is keel's own three-valued Decision, whose zero
// value is DecisionUnanswered on purpose: an implementation that returns
// without deciding causes a re-ask next cycle, never a silent approval and
// never a permanent suppression. DecisionDeclined is remembered per version;
// DecisionUnanswered is asked again, because silence is not a decision.
//
// Implementations owe four things. Taps from anyone other than a household
// member must not count as an answer. A timeout or an undeliverable message
// is DecisionUnanswered, never an approval. When req.SecuritySensitive is
// set, the message MUST say that the release changes security-relevant
// behaviour — that flag exists because a release may never silently move
// routing or privacy defaults, and a consent prompt that hides it would ask
// the household to approve exactly the thing the flag guards against.
// And errors must not embed secrets, because they are logged.
type Consenter interface {
	// RequestConsent asks whether the update described by req may be
	// applied, showing req.Notes verbatim and naming req.SecuritySensitive
	// when set. It blocks until a member answers, ctx expires, or delivery
	// fails.
	RequestConsent(ctx context.Context, req keelupdate.ConsentRequest) (keelupdate.Decision, error)
}

// Drainer blocks until no member has a turn in flight, so a restart never
// lands mid-conversation. It runs after the artifact is verified and
// immediately before the binary swap; returning an error aborts the update
// with nothing changed on disk.
//
// Implement it with the supervisor's own drain — its Stop, which halts
// intake, lets in-flight turns finish and locks every session — never with a
// second bookkeeping mechanism guessing at the same fact. The supervisor is
// the one component that actually knows whether a turn is in flight, and two
// sources of that truth would eventually disagree. Note the consequence: a
// completed drain has stopped intake, so whatever follows must end in a
// process restart; the Scheduler restarts even when the swap itself then
// fails, precisely so a drained-but-not-updated household comes back up.
//
// keel holds its cross-process lock across the drain, so implementations
// must bound the wait comfortably under the lock-staleness horizon (keel's
// default is ten minutes) or a sibling will presume the updater crashed and
// break the lock mid-drain. The supervisor's three-minute DefaultDrainTimeout
// fits with room to spare.
type Drainer interface {
	Drain(ctx context.Context) error
}

// DrainFunc adapts a plain function to the Drainer interface.
type DrainFunc func(ctx context.Context) error

// Drain calls f.
func (f DrainFunc) Drain(ctx context.Context) error { return f(ctx) }

// HealthProbes are the checks behind the post-restart health decision. Each
// probe closes over its own credentials — this package never sees a bot token
// or a key. A nil probe is skipped; supplying neither means health passes on
// "the process restarted and reached this code", which is the minimum.
type HealthProbes struct {
	// Lore reports whether this node's lore MCP responds — spawn (or reach)
	// the configured lore and complete a round trip.
	Lore func(ctx context.Context) error
	// Telegram reports whether the bot token authorises against the Telegram
	// API. It proves this process's own credential and configuration, not the
	// wider world: Telegram being reachable is a precondition of the product
	// doing anything at all, on old binary and new alike.
	Telegram func(ctx context.Context) error
}

// healthCheck builds the hook keel/update runs after a swap-and-restart to
// decide whether the new binary is kept or rolled back.
//
// Health tests only what this process itself controls: it started (executing
// this code is the proof), its configuration parsed (a Scheduler is only ever
// constructed from a loaded config, so a build that cannot parse the config
// never gets this far), lore's MCP responds, and Telegram authorises the
// token.
//
// Endpoint reachability is deliberately absent and must NEVER be added here.
// A household's inference machines are legitimately powered off much of the
// time — someone's gaming PC is asleep, the workstation is off for the
// weekend — and that is normal life, not ill health. A health check that
// required an endpoint would judge a perfectly good binary "unhealthy", roll
// the update back, re-apply it on the next check, roll it back again, and
// eventually wedge the installation. If you came here to "improve" health by
// probing the endpoints: that is the exact failure keel/update's
// documentation warns about, and TestHealthNeverConsultsEndpoints exists to
// fail the change. Health means "this binary works", not "the world is
// reachable".
func healthCheck(p HealthProbes) keelupdate.HealthCheck {
	return func(ctx context.Context) error {
		if p.Lore != nil {
			if err := p.Lore(ctx); err != nil {
				return fmt.Errorf("lore did not respond: %w", err)
			}
		}
		if p.Telegram != nil {
			if err := p.Telegram(ctx); err != nil {
				return fmt.Errorf("telegram did not authorise: %w", err)
			}
		}
		return nil
	}
}
