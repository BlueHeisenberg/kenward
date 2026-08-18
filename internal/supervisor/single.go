package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// SingleOptions configures a Single supervisor. Exactly one of Member and Group
// selects the unit; every dependency left nil is built from the configuration.
type SingleOptions struct {
	// Member names the one member this process serves. Mutually exclusive with
	// Group; exactly one must be set.
	Member domain.MemberID
	// Group selects the household group's unit instead of a member's.
	Group bool

	// Transport is this unit's own bot. Nil builds a Telegram transport from the
	// member's own bot_token_env — never the household's — or, for the group,
	// from telegram.bot_token_env.
	Transport transport.Transport
	// Memory is this unit's lore client. Nil opens the store at the LORE_HOME this
	// process was given, which is the pod's own.
	Memory memory.Memory
	// Router walks tier chains. Nil builds a routing.Pool over the configured
	// endpoints.
	Router routing.Router
	// Sessions holds this member's key and nobody else's. Nil builds a
	// session.Manager in isolated mode over a file store in the data directory,
	// which inside a pod holds exactly one wrapped key.
	Sessions session.Sessions
	// UnlockOnEnrol provisions and unlocks this member's key when their claim
	// binds, so a claim-only pod serves the moment it reports serving. It is
	// called for this pod's own member and for nobody else — see runner.enrolled
	// — and the passphrase it closes over is that member's own. Nil leaves a
	// freshly claimed member locked until the pod restarts.
	UnlockOnEnrol UnlockOnEnrol
	// Enrol receives direct messages while the selected member has not yet
	// claimed their invite. A member's bot exists before they claim, and the
	// claim happens in a conversation with that bot — never the household's —
	// so an unenrolled member's pod starts in a claim-only state: no unit, no
	// session, nothing touching lore, just this claimer waiting for the code.
	// When the claim binds, the member's unit starts serving in place, without
	// a restart. Required to select a member who has not enrolled; ignored for
	// one who has, whose claim window is over.
	Enrol *enrol.Claimer
	// Unit seeds the unit's options. HouseholdName and SearchLimit are filled
	// from the configuration when zero, and so are ContextBudget and MaxTokens —
	// from the endpoints this unit's own tier chain reaches; see the same field
	// on SimpleOptions.
	Unit assistant.Options
	// Logger receives lifecycle events and per-message failures. Nil discards.
	Logger *slog.Logger
	// Secrets resolves the bot token from whichever source the configuration
	// states — a file, an environment variable, or a systemd credential. A pod
	// is exactly where the environment is the wrong place for a token, so a
	// 0600 file or a credential works here without any environment variable
	// existing at all. Nil builds a resolver over LookupEnv, the real
	// filesystem and the CREDENTIALS_DIRECTORY this process was given.
	Secrets *config.Secrets
	// LookupEnv resolves endpoint API keys at call time, locates LORE_HOME,
	// and seeds the default Secrets resolver. Nil means os.LookupEnv.
	LookupEnv config.LookupEnvFunc
	// DrainTimeout bounds the drain when shutdown is triggered by Start's context
	// rather than by Stop. Defaults to DefaultDrainTimeout.
	DrainTimeout time.Duration
	// RestartBackoff and MaxRestartBackoff schedule goroutine restarts after the
	// unit panics. Defaults: DefaultRestartBackoff, DefaultMaxRestartBackoff.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// Now supplies timestamps for health reporting. Nil means time.Now.
	Now func() time.Time
}

// Single runs exactly one unit in this process: one member's assistant, or the
// household group's. It is what runs inside each pod the isolated supervisor
// starts — `kenward run --member david` or `kenward run --group` — and it is
// mode-blind in the way the design demands: the unit it builds is the same
// assistant.Unit, built by the same code path, that Simple builds a houseful of.
//
// It deliberately holds one unit's credentials and nothing more: the selected
// unit's own bot token, that member's key, this process's own lore. Handing a pod
// a second member's token or key would undo the entire mode, so there is no way
// to express that here.
//
// Unlike the isolated supervisor, which needs Linux for the pod boundary, a
// Single runs anywhere: the thing inside the pod is just a process, and nothing
// stops an operator running one member per machine with it.
//
// A Single is single-use: construct, Start once, Stop once.
type Single struct {
	run *runner
}

// NewSingle wires a Single supervisor over cfg for the unit opts selects.
//
// Selecting a member the configuration does not carry is an error. Selecting one
// who has not claimed their invite starts the pod in a claim-only state — see
// SingleOptions.Enrol — and is an error only when no claimer was supplied, since
// such a pod could never do anything at all. Selecting the group requires a
// configured group chat for the same reason.
func NewSingle(cfg *config.Config, opts SingleOptions) (*Single, error) {
	if cfg == nil {
		return nil, errors.New("supervisor: nil configuration")
	}
	if cfg.Mode != config.ModeIsolated {
		return nil, fmt.Errorf("supervisor: single-unit supervisor given mode %q; it serves one pod of an isolated deployment", cfg.Mode)
	}
	if opts.Group == (opts.Member != "") {
		return nil, errors.New("supervisor: select exactly one of Member and Group")
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
		transport:         opts.Transport,
		memory:            opts.Memory,
		router:            opts.Router,
		sessions:          opts.Sessions,
		unlockOnEnrol:     opts.UnlockOnEnrol,
		sessionMode:       session.ModeIsolated,
		endpointKey:       endpointKeyFunc(cfg, secrets),
		lookupEnv:         opts.LookupEnv,
		remindOpts:        remindOpts,
		unitOpts:          opts.Unit,
		logger:            opts.Logger,
		drainTimeout:      opts.DrainTimeout,
		restartBackoff:    opts.RestartBackoff,
		maxRestartBackoff: opts.MaxRestartBackoff,
		now:               opts.Now,
		privacyMode:       privacy.ModeIsolated,
	}
	rc.normalize()

	// This process's lore is the one LORE_HOME names — inside a pod, the pod's
	// own instance. Pinning it explicitly on the subprocess keeps that true even
	// if lore's own default ever moves.
	if home, ok := rc.lookupEnv(EnvLoreHome); ok {
		rc.loreHome = home
	}

	switch {
	case opts.Group:
		if cfg.Household.GroupChatID == 0 {
			return nil, errors.New("supervisor: group unit selected but no group chat is configured")
		}
		rc.group = true
		// The household's bot, and so the empty bot identity: under one agent each
		// this is what makes a private message here kenward's conversation rather
		// than the sender's own.
		rc.bot = ""
		rc.botToken = func() (config.Secret, error) { return cfg.BotToken(secrets) }
	default:
		m, ok := cfg.MemberByID(opts.Member)
		if !ok {
			return nil, fmt.Errorf("supervisor: no member %q in this household", opts.Member)
		}
		// The member's own token, never the household's, resolved through the
		// Secrets API from whichever source this member's row states. A pod
		// holding another unit's credentials would undo the mode.
		mc, ok := memberConfigByID(cfg, opts.Member)
		if !ok {
			return nil, fmt.Errorf("supervisor: no member %q in this household", opts.Member)
		}
		rc.botToken = func() (config.Secret, error) { return mc.BotToken(secrets) }
		// This pod's bot is this member's own agent. Resolution needs to know, so
		// that a message from anyone else arriving on it is refused by the boundary
		// rather than only by there being no unit to hand it to.
		rc.bot = m.ID
		// A claimed member gets their unit; one who has not claimed yet gets a
		// claim-only process — their bot exists before they enrol, and the
		// claim conversation happens on it, so the pod's job in that window is
		// to wait for the code and nothing else. Serving would be impossible
		// anyway, and refusing to start would leave nowhere for the code to go.
		if m.Enrolled() {
			rc.members = []domain.Member{m}
		} else {
			if opts.Enrol == nil {
				return nil, fmt.Errorf("supervisor: member %q has not claimed their invite and no claimer was supplied: %w",
					opts.Member, ErrNotEnrolled)
			}
			rc.unenrolled = []domain.MemberID{m.ID}
			rc.claimer = opts.Enrol
		}
		// Whether the claim happens now or happened already, this process
		// serves exactly one member. A claim for anyone else that lands on this
		// bot binds them, but their unit belongs to their own pod.
		selected := m.ID
		rc.serveOnEnrol = func(em domain.Member) bool { return em.ID == selected }
	}

	run, err := newRunner(cfg, rc)
	if err != nil {
		return nil, err
	}
	return &Single{run: run}, nil
}

// Start launches the unit's goroutine and blocks until ctx is cancelled, Stop is
// called, or the bot's update stream ends underneath it. On cancellation Start
// drains exactly as Stop does, bounded by DrainTimeout, and returns ctx.Err().
func (s *Single) Start(ctx context.Context) error { return s.run.start(ctx) }

// Stop drains and shuts down: intake stops immediately, the turn in flight
// finishes, the session is locked, and only then does Stop return. Inside a pod
// this is what answers the SIGTERM a graceful pod stop delivers. ctx bounds the
// wait for the in-flight turn; on expiry it is cancelled and the ctx error is
// returned so the caller knows the drain was not clean. Stop is idempotent and
// may be called before Start.
func (s *Single) Stop(ctx context.Context) error { return s.run.stop(ctx) }

// Health reports the one unit's condition from the supervisor's own records,
// never from anything external, and is callable before Start and after Stop. In
// the claim-only window the member reports StateNotEnrolled — a known situation,
// never a failure — and moves to StateReady when their claim mints the unit.
func (s *Single) Health(_ context.Context) ([]UnitHealth, error) {
	return s.run.health(), nil
}

// Single implements Supervisor.
var _ Supervisor = (*Single)(nil)

// memberConfigByID finds a member's configuration row, which carries the secret
// references the domain type deliberately does not.
func memberConfigByID(cfg *config.Config, id domain.MemberID) (config.MemberConfig, bool) {
	for _, mc := range cfg.Members {
		if domain.MemberID(mc.ID) == id {
			return mc, true
		}
	}
	return config.MemberConfig{}, false
}
