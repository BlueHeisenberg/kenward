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
	// Memory is this unit's lore client. Nil builds one from memory.lore_command
	// against the LORE_HOME this process was given, which is the pod's own.
	Memory memory.Memory
	// Router walks tier chains. Nil builds a routing.Pool over the configured
	// endpoints.
	Router routing.Router
	// Sessions holds this member's key and nobody else's. Nil builds a
	// session.Manager in isolated mode over a file store in the data directory,
	// which inside a pod holds exactly one wrapped key.
	Sessions session.Sessions
	// Unit seeds the unit's options. HouseholdName and SearchLimit are filled
	// from the configuration when zero.
	Unit assistant.Options
	// Logger receives lifecycle events and per-message failures. Nil discards.
	Logger *slog.Logger
	// LookupEnv resolves the bot token and API keys. Nil means os.LookupEnv.
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
// Selecting a member the configuration does not carry, or one who has not claimed
// their invite, is an error rather than an empty run: a pod that starts cleanly
// and serves nobody would look healthy while a member goes unanswered. Selecting
// the group requires a configured group chat for the same reason.
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

	rc := runnerConfig{
		transport:         opts.Transport,
		memory:            opts.Memory,
		router:            opts.Router,
		sessions:          opts.Sessions,
		sessionMode:       session.ModeIsolated,
		lookupEnv:         opts.LookupEnv,
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
		rc.botTokenEnv = cfg.Telegram.BotTokenEnv
	default:
		m, ok := cfg.MemberByID(opts.Member)
		if !ok {
			return nil, fmt.Errorf("supervisor: no member %q in this household", opts.Member)
		}
		if !m.Enrolled() {
			// An error, not an empty run: a pod serving nobody must not sit
			// there looking healthy.
			return nil, fmt.Errorf("supervisor: member %q: %w", opts.Member, ErrNotEnrolled)
		}
		rc.members = []domain.Member{m}
		// The member's own token, never the household's. A pod holding another
		// unit's credentials would undo the mode.
		rc.botTokenEnv = m.BotTokenEnv
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
// never from anything external, and is callable before Start and after Stop.
func (s *Single) Health(_ context.Context) ([]UnitHealth, error) {
	return s.run.health(), nil
}

// Single implements Supervisor.
var _ Supervisor = (*Single)(nil)
