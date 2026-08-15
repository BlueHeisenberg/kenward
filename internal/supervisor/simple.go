package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Defaults for SimpleOptions. Exported so `kenward doctor` can say what silence buys.
const (
	// DefaultDrainTimeout bounds how long a shutdown triggered by context
	// cancellation waits for in-flight turns before cutting them. It comfortably
	// covers one completion at the default endpoint timeout.
	DefaultDrainTimeout = 3 * time.Minute
	// DefaultRestartBackoff is the first delay before a panicked unit's goroutine
	// is restarted.
	DefaultRestartBackoff = time.Second
	// DefaultMaxRestartBackoff caps the doubling restart delay.
	DefaultMaxRestartBackoff = 5 * time.Minute
	// simpleSessionsFile is where the default session manager persists wrapped
	// keys, under the configuration's data directory.
	simpleSessionsFile = "sessions.json"
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
	// Enrol, when set, receives every direct message from a sender no unit
	// serves, so claim codes work while the household runs. Nil means strangers
	// are dropped by the mux and enrolment happens elsewhere. The supervisor
	// never mints codes; it only carries messages to the Claimer and sends back
	// whatever onboarding a successful claim produced.
	Enrol *enrol.Claimer
	// Unit seeds every unit's options. HouseholdName and SearchLimit are filled
	// from the configuration when zero.
	Unit assistant.Options
	// Logger receives lifecycle events and per-message failures. Nil discards.
	Logger *slog.Logger
	// LookupEnv resolves bot tokens and API keys. Nil means os.LookupEnv.
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

func (o SimpleOptions) normalized() SimpleOptions {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	if o.RestartBackoff <= 0 {
		o.RestartBackoff = DefaultRestartBackoff
	}
	if o.MaxRestartBackoff <= 0 {
		o.MaxRestartBackoff = DefaultMaxRestartBackoff
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
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
	opts     SimpleOptions
	logger   *slog.Logger
	tracker  *tracker
	mux      *transport.Mux
	memory   memory.Memory
	router   routing.Router
	sessions session.Sessions
	// owned are the dependencies this supervisor constructed itself and therefore
	// closes on Stop. Injected dependencies stay their owner's to close.
	owned []io.Closer

	// cfg is this supervisor's private snapshot of the configuration; resolve
	// reads it and enrolled swaps it copy-on-write, so no unit ever observes a
	// half-updated member set. The supervisor never mutates the caller's Config.
	cfgMu sync.RWMutex
	cfg   *config.Config

	// turnCtx is what in-flight turns run under. It is deliberately not Start's
	// context: a drain must let the running turn finish after new intake has
	// stopped, and cancelling turns is the drain's last resort, not its first act.
	turnCtx    context.Context
	turnCancel context.CancelFunc
	// draining tells unit pumps to stop after the turn they are in.
	draining chan struct{}
	// allDone closes when the last pump goroutine exits.
	allDone     chan struct{}
	allDoneOnce sync.Once
	// stoppedCh closes when shutdown has fully completed.
	stoppedCh chan struct{}
	stopOnce  sync.Once
	stopErr   error

	mu       sync.Mutex
	started  bool
	stopping bool
	launched bool
	active   int
	startCtx context.Context
	pending  []pendingUnit
	served   map[int64]struct{}
}

// pendingUnit is a constructed unit waiting for Start to give it a goroutine.
type pendingUnit struct {
	key  unitKey
	unit *assistant.Unit
	view transport.Transport
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
	opts = opts.normalized()

	turnCtx, turnCancel := context.WithCancel(context.Background())
	s := &Simple{
		opts:       opts,
		logger:     opts.Logger,
		tracker:    newTracker(opts.Now),
		cfg:        snapshotConfig(cfg),
		turnCtx:    turnCtx,
		turnCancel: turnCancel,
		draining:   make(chan struct{}),
		allDone:    make(chan struct{}),
		stoppedCh:  make(chan struct{}),
		served:     make(map[int64]struct{}),
	}

	if err := s.buildDeps(); err != nil {
		turnCancel()
		s.closeOwned()
		return nil, err
	}
	s.mux = transport.NewMux(s.opts.Transport)

	for _, m := range s.cfg.DomainMembers() {
		if !m.Enrolled() {
			s.tracker.addNotEnrolled(m.ID)
			continue
		}
		if err := s.buildMemberUnit(m); err != nil {
			turnCancel()
			s.closeOwned()
			return nil, err
		}
	}
	if s.cfg.Household.GroupChatID != 0 {
		if err := s.buildGroupUnit(); err != nil {
			turnCancel()
			s.closeOwned()
			return nil, err
		}
	}
	if len(s.pending) == 0 && opts.Enrol == nil {
		turnCancel()
		s.closeOwned()
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return s, nil
}

func (s *Simple) buildDeps() error {
	if s.opts.Transport == nil {
		env := s.cfg.Telegram.BotTokenEnv
		token, ok := s.opts.LookupEnv(env)
		if !ok || token == "" {
			return fmt.Errorf("supervisor: bot token environment variable %q is not set", env)
		}
		t, err := transport.NewTelegram(token, transport.WithLogger(s.logger))
		if err != nil {
			return fmt.Errorf("supervisor: building telegram transport: %w", err)
		}
		s.opts.Transport = t
		s.owned = append(s.owned, t)
	}
	if s.opts.Memory == nil {
		cmd := s.cfg.Memory.LoreCommand
		if len(cmd) == 0 {
			return errors.New("supervisor: memory.lore_command is empty")
		}
		c, err := memory.NewClient(memory.Config{
			Command: cmd[0],
			Args:    cmd[1:],
			Logger:  s.logger,
		})
		if err != nil {
			return fmt.Errorf("supervisor: building lore client: %w", err)
		}
		s.opts.Memory = c
		s.owned = append(s.owned, c)
	}
	if s.opts.Router == nil {
		s.opts.Router = routing.NewPool(s.cfg.RoutingEndpoints(),
			routing.NewHTTPCompleter(nil, s.opts.LookupEnv))
	}
	if s.opts.Sessions == nil {
		store := session.NewFileStore(filepath.Join(s.cfg.DataDir, simpleSessionsFile))
		m, err := session.NewManager(session.ModeSimple, store,
			session.WithIdleTimeout(s.cfg.Session.IdleTimeout.Duration()))
		if err != nil {
			return fmt.Errorf("supervisor: building session manager: %w", err)
		}
		s.opts.Sessions = m
		s.owned = append(s.owned, closerFunc(func() error { m.Close(); return nil }))
	}
	s.memory = s.opts.Memory
	s.router = s.opts.Router
	s.sessions = s.opts.Sessions
	return nil
}

// resolve is every unit's ResolveFunc. It reads the current configuration snapshot,
// which enrolled swaps atomically, so a member who claims their invite mid-run is
// recognised by the group unit's next message without any unit being restarted.
func (s *Simple) resolve(in transport.Inbound) (domain.Scope, error) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	return scope.Resolve(cfg, in)
}

// buildMemberUnit constructs one member's unit over a mux view scoped to their
// direct messages. Each unit gets its own capture engine and its own view; the
// only things units share are the seams that are shared by design in this mode —
// the bot, the lore client, the router and the session manager.
func (s *Simple) buildMemberUnit(m domain.Member) error {
	telegramID := m.TelegramID
	view := s.mux.View(func(in transport.Inbound) bool {
		return !in.IsGroup && in.UserID == telegramID
	})
	u, err := s.buildUnit(view, "member:"+string(m.ID))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.pending = append(s.pending, pendingUnit{key: unitKey{member: m.ID}, unit: u, view: view})
	s.served[telegramID] = struct{}{}
	s.mu.Unlock()
	s.tracker.add(unitKey{member: m.ID})
	return nil
}

// buildGroupUnit constructs the household group's unit over a view scoped to the
// configured group chat and nothing else.
func (s *Simple) buildGroupUnit() error {
	groupChatID := s.cfg.Household.GroupChatID
	view := s.mux.View(func(in transport.Inbound) bool {
		return in.IsGroup && in.ChatID == groupChatID
	})
	u, err := s.buildUnit(view, "group")
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.pending = append(s.pending, pendingUnit{key: unitKey{group: true}, unit: u, view: view})
	s.mu.Unlock()
	s.tracker.add(unitKey{group: true})
	return nil
}

func (s *Simple) buildUnit(view transport.Transport, name string) (*assistant.Unit, error) {
	engine := capture.New(s.memory, view, capture.Options{
		MaxProposalsPerTurn: s.cfg.Capture.MaxProposalsPerTurn,
		Shared:              domain.SpaceID(s.cfg.Household.SharedSpace),
	})
	unitOpts := s.opts.Unit
	if unitOpts.HouseholdName == "" {
		unitOpts.HouseholdName = s.cfg.Household.Name
	}
	if unitOpts.SearchLimit == 0 {
		unitOpts.SearchLimit = s.cfg.Memory.SearchLimit
	}
	u, err := assistant.New(assistant.Deps{
		Resolve:   s.resolve,
		Memory:    s.memory,
		Router:    s.router,
		Transport: view,
		Sessions:  s.sessions,
		Capture:   engine,
		Logger:    s.logger.With("unit", name),
	}, unitOpts)
	if err != nil {
		return nil, fmt.Errorf("supervisor: building unit %s: %w", name, err)
	}
	return u, nil
}

// Start launches every unit's goroutine, begins fanning updates out, and blocks
// until ctx is cancelled, Stop is called, or the transport's update stream ends
// underneath it — the one failure this mode cannot ride out, because with the
// stream gone no unit can receive anything.
//
// On cancellation Start drains exactly as Stop does, bounded by DrainTimeout, and
// returns ctx.Err().
func (s *Simple) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("supervisor: already started")
	}
	if s.stopping {
		s.mu.Unlock()
		return errors.New("supervisor: already stopped")
	}
	s.started = true
	s.startCtx = ctx
	units := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, pu := range units {
		if err := s.launch(ctx, pu); err != nil {
			s.shutdown(s.drainContext())
			return err
		}
	}
	if s.opts.Enrol != nil {
		if err := s.launchEnrol(ctx); err != nil {
			s.shutdown(s.drainContext())
			return err
		}
	}
	s.mu.Lock()
	s.launched = true
	idle := s.active == 0
	s.mu.Unlock()
	if idle {
		// Possible only in pathological wiring; close so waiters don't hang.
		s.allDoneOnce.Do(func() { close(s.allDone) })
	}

	if err := s.mux.Start(ctx); err != nil {
		s.shutdown(s.drainContext())
		return fmt.Errorf("supervisor: starting mux: %w", err)
	}

	s.logStartup()

	select {
	case <-ctx.Done():
		s.shutdown(s.drainContext())
		return ctx.Err()
	case <-s.allDone:
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			<-s.stoppedCh
			return nil
		}
		// Every pump exited and nobody asked them to: the bot's update stream
		// ended. Drain what little is left and report it.
		s.shutdown(s.drainContext())
		return errors.New("supervisor: transport update stream ended")
	case <-s.stoppedCh:
		return nil
	}
}

// logStartup emits the one summary an operator should be able to read and know what
// this process will and will not do: the mode, the units served, each conversation's
// tier chain, and the mode's privacy posture, worded once, in the privacy package.
func (s *Simple) logStartup() {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	for _, m := range cfg.DomainMembers() {
		if m.Enrolled() {
			s.logger.Info("supervisor: serving member",
				"member", string(m.ID), "tiers", m.Tiers)
		} else {
			s.logger.Info("supervisor: member not enrolled, no unit",
				"member", string(m.ID))
		}
	}
	if cfg.Household.GroupChatID != 0 {
		s.logger.Info("supervisor: serving household group",
			"tiers", cfg.Household.Tiers)
	}
	s.logger.Info("supervisor: started",
		"mode", privacy.ModeSimple.String(),
		"privacy", privacy.Statement(privacy.ModeSimple))
}

// launch opens a unit's update stream and gives it a goroutine.
func (s *Simple) launch(ctx context.Context, pu pendingUnit) error {
	ch, err := pu.view.Updates(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: opening updates for unit: %w", err)
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return errors.New("supervisor: stopping")
	}
	s.active++
	s.mu.Unlock()
	s.tracker.set(pu.key, StateStarting)
	go s.runUnit(pu.key, pu.unit, ch)
	return nil
}

// runUnit supervises one unit's goroutine. unitLoop returning an error means the
// unit panicked mid-turn; restarting it here means exactly what the package doc
// promises — run the loop again after a backoff — because a Unit holds no state a
// crash can corrupt beyond the turn that crashed.
func (s *Simple) runUnit(k unitKey, u *assistant.Unit, ch <-chan transport.Inbound) {
	defer s.workerDone()
	s.tracker.set(k, StateReady)
	bo := newBackoff(s.opts.RestartBackoff, s.opts.MaxRestartBackoff)
	for {
		err := s.unitLoop(u, ch)
		if err == nil {
			s.tracker.set(k, StateStopped)
			return
		}
		s.logger.Error("supervisor: unit crashed, restarting", "unit", k.member, "group", k.group, "error", err)
		s.tracker.fail(k, err)
		if !s.sleep(bo.next()) {
			s.tracker.set(k, StateStopped)
			return
		}
		s.tracker.set(k, StateReady)
	}
}

// unitLoop feeds one unit until its stream closes or a drain begins. It returns
// nil for both of those and an error only when a turn panicked.
func (s *Simple) unitLoop(u *assistant.Unit, ch <-chan transport.Inbound) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("supervisor: unit panicked: %v", r)
		}
	}()
	for {
		// Prefer the drain signal over queued backlog: drained means "stop
		// accepting", and a message accepted but not yet started is not a turn
		// the household is owed.
		select {
		case <-s.draining:
			return nil
		default:
		}
		select {
		case <-s.draining:
			return nil
		case in, ok := <-ch:
			if !ok {
				return nil
			}
			s.handleErr(u.Handle(s.turnCtx, in), in)
		}
	}
}

// handleErr triages one turn's error. Not-enrolled is answered with the silence it
// asks for; cancellation is the shutdown path; everything else is logged and the
// unit keeps serving.
func (s *Simple) handleErr(err error, in transport.Inbound) {
	switch {
	case err == nil:
	case errors.Is(err, scope.ErrNotEnrolled):
		s.logger.Debug("supervisor: dropped message from unrecognised sender", "chat", in.ChatID)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.logger.Debug("supervisor: turn abandoned during shutdown", "chat", in.ChatID)
	default:
		s.logger.Warn("supervisor: turn failed", "chat", in.ChatID, "error", err)
	}
}

// launchEnrol starts the pump that carries messages from strangers to the Claimer.
// Its view accepts direct messages from any sender no unit serves, so it never
// shadows a member — including one whose unit was created after this view was.
func (s *Simple) launchEnrol(ctx context.Context) error {
	view := s.mux.View(func(in transport.Inbound) bool {
		return !in.IsGroup && !s.servesUser(in.UserID)
	})
	ch, err := view.Updates(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: opening updates for enrolment: %w", err)
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return errors.New("supervisor: stopping")
	}
	s.active++
	s.mu.Unlock()
	go s.runEnrol(view, ch)
	return nil
}

func (s *Simple) servesUser(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.served[id]
	return ok
}

// runEnrol hands each stranger's message to the Claimer and sends back exactly the
// messages it returned — which is nothing, on every path but a successful claim.
// Errors are for the log and never for the sender; turning one into a reply would
// confirm the bot exists, which is the fact silence protects.
func (s *Simple) runEnrol(view transport.Transport, ch <-chan transport.Inbound) {
	defer s.workerDone()
	for {
		select {
		case <-s.draining:
			return
		case in, ok := <-ch:
			if !ok {
				return
			}
			res, err := s.opts.Enrol.Handle(s.turnCtx, in)
			if err != nil {
				s.logger.Warn("supervisor: enrolment attempt failed (sender got silence)", "error", err)
			}
			for _, out := range res.Messages {
				if err := view.Send(s.turnCtx, out); err != nil {
					s.logger.Warn("supervisor: sending onboarding failed", "error", err)
					break
				}
			}
			if res.Enrolled {
				s.enrolled(res.Member)
			}
		}
	}
}

// enrolled brings a freshly claimed member into service without a restart: fold
// their binding into a new configuration snapshot, swap it in, and give them a
// unit. The swap is copy-on-write, so no unit ever reads a half-updated member set.
func (s *Simple) enrolled(m domain.Member) {
	s.cfgMu.Lock()
	s.cfg = configWithBinding(s.cfg, m)
	s.cfgMu.Unlock()
	s.tracker.promote(m.ID)

	if err := s.buildMemberUnit(m); err != nil {
		s.logger.Error("supervisor: building unit for new member", "member", string(m.ID), "error", err)
		return
	}
	s.mu.Lock()
	var pu pendingUnit
	if n := len(s.pending); n > 0 {
		pu = s.pending[n-1]
		s.pending = s.pending[:n-1]
	}
	ctx := s.startCtx
	s.mu.Unlock()
	if pu.unit == nil || ctx == nil {
		return
	}
	if err := s.launch(ctx, pu); err != nil {
		s.logger.Error("supervisor: launching unit for new member", "member", string(m.ID), "error", err)
	}
	s.logger.Info("supervisor: member enrolled and serving", "member", string(m.ID))
}

func (s *Simple) workerDone() {
	s.mu.Lock()
	s.active--
	last := s.active == 0 && s.launched
	s.mu.Unlock()
	if last {
		s.allDoneOnce.Do(func() { close(s.allDone) })
	}
}

// sleep waits d unless a drain begins first.
func (s *Simple) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.draining:
		return false
	}
}

// drainContext bounds a shutdown that has no caller-supplied deadline.
func (s *Simple) drainContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.DrainTimeout)
	// The context lives for the length of the drain; shutdown runs once, so tie
	// the cancel to its completion.
	go func() {
		<-s.stoppedCh
		cancel()
	}()
	return ctx
}

// Stop drains and shuts down: intake stops immediately, the turn each unit is in
// finishes, every session is locked, and only then does Stop return. ctx bounds
// the wait for in-flight turns; when it expires they are cancelled, which is the
// one case a turn can be cut mid-write, and the ctx error is returned so the
// caller knows the drain was not clean. Stop is idempotent and may be called
// before Start, in which case there is nothing to drain and it only locks
// sessions and releases what the constructor built.
func (s *Simple) Stop(ctx context.Context) error {
	return s.shutdown(ctx)
}

func (s *Simple) shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		started := s.started
		idle := s.active == 0
		s.mu.Unlock()

		// Stop intake: pumps see draining and finish their current turn; the mux
		// stops dispatching and closes every view. Backlogged messages that never
		// started a turn are dropped — drained means no new work, and only the
		// turns already speaking to a member are owed a finish.
		close(s.draining)
		_ = s.mux.Close()

		if started && !idle {
			select {
			case <-s.allDone:
			case <-ctx.Done():
				s.stopErr = ctx.Err()
				// The grace ran out. Cut the remaining turns and give them a
				// moment to observe it rather than abandoning their goroutines.
				s.turnCancel()
				select {
				case <-s.allDone:
				case <-time.After(5 * time.Second):
					s.logger.Error("supervisor: turns did not exit after cancellation")
				}
			}
		}

		// The order is deliberate: sessions are locked only after the in-flight
		// turns are done, because a turn cut off from its key mid-write is how a
		// member loses a capture they confirmed.
		s.turnCancel()
		s.sessions.LockAll()
		s.tracker.stopAll()
		s.closeOwned()
		close(s.stoppedCh)
	})
	<-s.stoppedCh
	return s.stopErr
}

func (s *Simple) closeOwned() {
	for _, c := range s.owned {
		if err := c.Close(); err != nil {
			s.logger.Warn("supervisor: closing dependency", "error", err)
		}
	}
	s.owned = nil
}

// Health reports every unit's condition from the supervisor's own records. It
// never touches the transport, the router, lore or anything else external — a
// household's inference machines being asleep is normal life, not ill health —
// and is callable before Start and after Stop. Members who have not enrolled are
// present with StateUnknown and ErrNotEnrolled: they have no unit, which is a
// fact, not a failure.
func (s *Simple) Health(_ context.Context) ([]UnitHealth, error) {
	return s.tracker.snapshot(), nil
}

// Simple implements Supervisor.
var _ Supervisor = (*Simple)(nil)

// closerFunc adapts a function to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// snapshotConfig copies the parts of a configuration the supervisor reads
// concurrently, so nothing outside this package can mutate what scope resolution
// sees mid-run.
func snapshotConfig(c *config.Config) *config.Config {
	out := *c
	out.Members = append([]config.MemberConfig(nil), c.Members...)
	out.Endpoints = append([]config.EndpointConfig(nil), c.Endpoints...)
	out.Household.Tiers = append([]string(nil), c.Household.Tiers...)
	return &out
}

// configWithBinding returns a new snapshot carrying m's enrolment. The member row
// is updated in place when the configuration knows them and appended when the
// Binder created them from an invited name.
func configWithBinding(c *config.Config, m domain.Member) *config.Config {
	out := snapshotConfig(c)
	for i := range out.Members {
		if domain.MemberID(out.Members[i].ID) == m.ID {
			out.Members[i].TelegramID = m.TelegramID
			out.Members[i].EnrolledAt = m.EnrolledAt
			return out
		}
	}
	out.Members = append(out.Members, config.MemberConfig{
		ID:           string(m.ID),
		Name:         m.Name,
		TelegramID:   m.TelegramID,
		PrivateSpace: string(m.Private),
		Tiers:        append([]string(nil), m.Tiers...),
		BotTokenEnv:  m.BotTokenEnv,
		EnrolledAt:   m.EnrolledAt,
	})
	return out
}
