package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Defaults for SimpleOptions and SingleOptions. Exported so `kenward doctor` can
// say what silence buys.
const (
	// DefaultDrainTimeout bounds how long a shutdown triggered by context
	// cancellation waits for in-flight turns before cutting them. It comfortably
	// covers one completion at the default endpoint timeout.
	DefaultDrainTimeout = 3 * time.Minute
	// DefaultRestartBackoff is the first delay before a failed unit is restarted.
	DefaultRestartBackoff = time.Second
	// DefaultMaxRestartBackoff caps the doubling restart delay.
	DefaultMaxRestartBackoff = 5 * time.Minute
	// simpleSessionsFile is where the default session manager persists wrapped
	// keys, under the configuration's data directory.
	simpleSessionsFile = "sessions.json"
	// defaultCancelGrace is how long a drain whose patience has already run out
	// waits for the goroutines it just cancelled to notice, before it stops
	// waiting on them at all.
	defaultCancelGrace = 5 * time.Second
)

// SimpleOptions configures a Simple supervisor. Every dependency left nil is built
// from the configuration; tests inject fakes, production wiring injects nothing.
type SimpleOptions struct {
	// Transport is the household's one bot. Nil builds a Telegram transport from
	// the token named by telegram.bot_token_env.
	Transport transport.Transport
	// Memory is the shared lore client. Nil builds one from memory.lore_command.
	// One lore instance for the whole household is this mode's arrangement: the
	// spaces separate members, the address space does not.
	Memory memory.Memory
	// Router walks tier chains. Nil builds a routing.Pool over the configured
	// endpoints.
	Router routing.Router
	// Sessions holds unwrapped member keys. Nil builds a session.Manager in
	// simple mode over a file store in the data directory.
	Sessions session.Sessions
	// UnlockOnEnrol gives a member who claims mid-run the key their unit needs.
	// Nil means their first private message is answered with the locked notice
	// until the node restarts; see the type.
	UnlockOnEnrol UnlockOnEnrol
	// Enrol, when set, receives every direct message from a sender no unit
	// serves, so claim codes work while the household runs. Nil means strangers
	// are dropped by the mux and enrolment happens elsewhere. The supervisor
	// never mints codes; it only carries messages to the Claimer and sends back
	// whatever onboarding a successful claim produced.
	Enrol *enrol.Claimer
	// Unit seeds every unit's options. HouseholdName and SearchLimit are filled
	// from the configuration when zero, and so are ContextBudget and MaxTokens —
	// from the endpoints each unit's own tier chain reaches, which is where they
	// belong. Leave both zero unless every conversation in the household
	// genuinely shares one window: a member on a local-only chain and a group on
	// a cloud chain legitimately get different budgets, and one household-wide
	// number gets one of them wrong.
	Unit assistant.Options
	// Logger receives lifecycle events and per-message failures. Nil discards.
	Logger *slog.Logger
	// Secrets resolves the bot token from whichever source the configuration
	// states — a file, an environment variable, or a systemd credential. Nil
	// builds a resolver over LookupEnv, the real filesystem and the
	// CREDENTIALS_DIRECTORY systemd supplies.
	Secrets *config.Secrets
	// LookupEnv resolves endpoint API keys at call time and seeds the default
	// Secrets resolver. Nil means os.LookupEnv.
	LookupEnv config.LookupEnvFunc
	// DrainTimeout bounds the drain when shutdown is triggered by Start's context
	// rather than by Stop, whose own context sets the bound. Defaults to
	// DefaultDrainTimeout.
	DrainTimeout time.Duration
	// RestartBackoff and MaxRestartBackoff schedule goroutine restarts after a
	// unit panics. Defaults: DefaultRestartBackoff, DefaultMaxRestartBackoff.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// Now supplies timestamps for health reporting. Nil means time.Now.
	Now func() time.Time
}

// Simple runs every unit as a goroutine in this process, behind one bot token.
//
// One transport, one Mux fanning its updates out by (UserID, ChatID), one Unit per
// enrolled member plus one for the household group. Everything shares an address
// space — that is the mode's stated limitation, said out loud in the privacy
// statement, not a defect to paper over. The units themselves are the same
// assistant.Unit the isolated mode runs; nothing they receive tells them which
// mode this is.
//
// A Simple is single-use: construct, Start once, Stop once.
type Simple struct {
	run *runner
}

// NewSimple wires a Simple supervisor over cfg. It builds one unit per enrolled
// member and one for the household group when a group chat is configured; members
// who have not enrolled get a health record and nothing else. It returns ErrNoUnits
// when the configuration yields nothing to run and no Claimer could change that.
func NewSimple(cfg *config.Config, opts SimpleOptions) (*Simple, error) {
	if cfg == nil {
		return nil, errors.New("supervisor: nil configuration")
	}
	if cfg.Mode != config.ModeSimple {
		return nil, fmt.Errorf("supervisor: simple supervisor given mode %q", cfg.Mode)
	}

	secrets := opts.Secrets
	if secrets == nil {
		secrets = config.NewSecrets(config.SecretOptions{LookupEnv: opts.LookupEnv})
	}
	remindOpts, err := remindOptions(cfg)
	if err != nil {
		return nil, err
	}
	rc := runnerConfig{
		// One bot for the whole household, and it is the household's. Under one
		// agent — the only identity this mode can deliver, see
		// Config.AgentPerMember — that bot is also every member's own assistant,
		// which is what makes a direct message here their own conversation.
		bot:           "",
		transport:     opts.Transport,
		memory:        opts.Memory,
		router:        opts.Router,
		sessions:      opts.Sessions,
		claimer:       opts.Enrol,
		unlockOnEnrol: opts.UnlockOnEnrol,
		// The household bot's token, resolved through the Secrets API at the
		// moment the transport is built — file first, then environment, then
		// credential, exactly as the configuration states it.
		botToken:          func() (config.Secret, error) { return cfg.BotToken(secrets) },
		endpointKey:       endpointKeyFunc(cfg, secrets),
		sessionMode:       session.ModeSimple,
		lookupEnv:         opts.LookupEnv,
		remindOpts:        remindOpts,
		unitOpts:          opts.Unit,
		logger:            opts.Logger,
		drainTimeout:      opts.DrainTimeout,
		restartBackoff:    opts.RestartBackoff,
		maxRestartBackoff: opts.MaxRestartBackoff,
		now:               opts.Now,
		privacyMode:       privacy.ModeSimple,
		group:             cfg.Household.GroupChatID != 0,
	}
	rc.normalize()
	for _, m := range cfg.DomainMembers() {
		if m.Enrolled() {
			rc.members = append(rc.members, m)
		} else {
			rc.unenrolled = append(rc.unenrolled, m.ID)
		}
	}

	run, err := newRunner(cfg, rc)
	if err != nil {
		return nil, err
	}
	return &Simple{run: run}, nil
}

// normalize fills a runnerConfig's zero-valued knobs with the package defaults.
func (rc *runnerConfig) normalize() {
	if rc.logger == nil {
		rc.logger = slog.New(slog.DiscardHandler)
	}
	if rc.lookupEnv == nil {
		rc.lookupEnv = os.LookupEnv
	}
	if rc.drainTimeout <= 0 {
		rc.drainTimeout = DefaultDrainTimeout
	}
	if rc.restartBackoff <= 0 {
		rc.restartBackoff = DefaultRestartBackoff
	}
	if rc.maxRestartBackoff <= 0 {
		rc.maxRestartBackoff = DefaultMaxRestartBackoff
	}
	if rc.healthyReset <= 0 {
		rc.healthyReset = DefaultHealthyReset
	}
	if rc.cancelGrace <= 0 {
		rc.cancelGrace = defaultCancelGrace
	}
	if rc.now == nil {
		rc.now = time.Now
	}
}

// Start launches every unit's goroutine, begins fanning updates out, and blocks
// until ctx is cancelled, Stop is called, or the transport's update stream ends
// underneath it — the one failure this mode cannot ride out, because with the
// stream gone no unit can receive anything.
//
// On cancellation Start drains exactly as Stop does, bounded by DrainTimeout, and
// returns ctx.Err().
func (s *Simple) Start(ctx context.Context) error { return s.run.start(ctx) }

// Stop drains and shuts down: intake stops immediately, the turn each unit is in
// finishes, every session is locked, and only then does Stop return. ctx bounds
// the wait for in-flight turns; when it expires they are cancelled, which is the
// one case a turn can be cut mid-write, and the ctx error is returned so the
// caller knows the drain was not clean. Stop is idempotent and may be called
// before Start, in which case there is nothing to drain and it only locks
// sessions and releases what the constructor built.
func (s *Simple) Stop(ctx context.Context) error { return s.run.stop(ctx) }

// Health reports every unit's condition from the supervisor's own records. It
// never touches the transport, the router, lore or anything else external — a
// household's inference machines being asleep is normal life, not ill health —
// and is callable before Start and after Stop. Members who have not enrolled are
// present with StateNotEnrolled: they have no unit, which is a known situation,
// not a failure and not an absence of information.
func (s *Simple) Health(_ context.Context) ([]UnitHealth, error) {
	return s.run.health(), nil
}

// Simple implements Supervisor.
var _ Supervisor = (*Simple)(nil)
