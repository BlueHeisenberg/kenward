package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// runnerConfig is everything the in-process machinery needs to run some set of
// units over one bot. Simple fills it with every enrolled member plus the group;
// Single fills it with exactly one unit. The unit construction below is the same
// code path for both — which is the point: there is one way to build a unit in
// this process, and the mode only decides how many are built.
type runnerConfig struct {
	// Which units to run.
	members []domain.Member
	group   bool
	// unenrolled members are reported in Health with ErrNotEnrolled and get
	// nothing else.
	unenrolled []domain.MemberID

	// Injected seams; any left nil is built from the configuration.
	transport transport.Transport
	memory    memory.Memory
	router    routing.Router
	sessions  session.Sessions
	// claimer, when set, receives direct messages from senders no unit serves.
	claimer *enrol.Claimer

	// How to build the nil seams.
	botTokenEnv string
	sessionMode session.Mode
	loreHome    string
	lookupEnv   config.LookupEnvFunc

	unitOpts          assistant.Options
	logger            *slog.Logger
	drainTimeout      time.Duration
	restartBackoff    time.Duration
	maxRestartBackoff time.Duration
	now               func() time.Time
	privacyMode       privacy.Mode
}

// runner runs units as goroutines in this process: one transport, one Mux fanning
// its updates out by (UserID, ChatID), one goroutine per unit. It is the shared
// engine behind Simple (all units) and Single (exactly one, inside a pod or on a
// member's own machine). A runner is single-use: construct, start once, stop once.
type runner struct {
	rc       runnerConfig
	logger   *slog.Logger
	tracker  *tracker
	mux      *transport.Mux
	memory   memory.Memory
	router   routing.Router
	sessions session.Sessions
	// owned are the dependencies the runner constructed itself and therefore
	// closes on stop. Injected dependencies stay their owner's to close.
	owned []io.Closer

	// cfg is this runner's private snapshot of the configuration; resolve reads
	// it and enrolled swaps it copy-on-write, so no unit ever observes a
	// half-updated member set. The runner never mutates the caller's Config.
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

// pendingUnit is a constructed unit waiting for start to give it a goroutine.
type pendingUnit struct {
	key  unitKey
	unit *assistant.Unit
	view transport.Transport
}

// newRunner wires the machinery over cfg. It returns ErrNoUnits when rc names
// nothing to run and no Claimer could change that.
func newRunner(cfg *config.Config, rc runnerConfig) (*runner, error) {
	turnCtx, turnCancel := context.WithCancel(context.Background())
	r := &runner{
		rc:         rc,
		logger:     rc.logger,
		tracker:    newTracker(rc.now),
		cfg:        snapshotConfig(cfg),
		turnCtx:    turnCtx,
		turnCancel: turnCancel,
		draining:   make(chan struct{}),
		allDone:    make(chan struct{}),
		stoppedCh:  make(chan struct{}),
		served:     make(map[int64]struct{}),
	}

	if err := r.buildDeps(); err != nil {
		turnCancel()
		r.closeOwned()
		return nil, err
	}
	r.mux = transport.NewMux(r.rc.transport)

	for _, id := range rc.unenrolled {
		r.tracker.addNotEnrolled(id)
	}
	for _, m := range rc.members {
		if err := r.buildMemberUnit(m); err != nil {
			turnCancel()
			r.closeOwned()
			return nil, err
		}
	}
	if rc.group {
		if err := r.buildGroupUnit(); err != nil {
			turnCancel()
			r.closeOwned()
			return nil, err
		}
	}
	if len(r.pending) == 0 && rc.claimer == nil {
		turnCancel()
		r.closeOwned()
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return r, nil
}

func (r *runner) buildDeps() error {
	if r.rc.transport == nil {
		env := r.rc.botTokenEnv
		token, ok := r.rc.lookupEnv(env)
		if env == "" || !ok || token == "" {
			return fmt.Errorf("supervisor: bot token environment variable %q is not set", env)
		}
		t, err := transport.NewTelegram(token, transport.WithLogger(r.logger))
		if err != nil {
			return fmt.Errorf("supervisor: building telegram transport: %w", err)
		}
		r.rc.transport = t
		r.owned = append(r.owned, t)
	}
	if r.rc.memory == nil {
		cmd := r.cfg.Memory.LoreCommand
		if len(cmd) == 0 {
			return errors.New("supervisor: memory.lore_command is empty")
		}
		c, err := memory.NewClient(memory.Config{
			Command:  cmd[0],
			Args:     cmd[1:],
			LoreHome: r.rc.loreHome,
			Logger:   r.logger,
		})
		if err != nil {
			return fmt.Errorf("supervisor: building lore client: %w", err)
		}
		r.rc.memory = c
		r.owned = append(r.owned, c)
	}
	if r.rc.router == nil {
		r.rc.router = routing.NewPool(r.cfg.RoutingEndpoints(),
			routing.NewHTTPCompleter(nil, r.rc.lookupEnv))
	}
	if r.rc.sessions == nil {
		store := session.NewFileStore(filepath.Join(r.cfg.DataDir, simpleSessionsFile))
		m, err := session.NewManager(r.rc.sessionMode, store,
			session.WithIdleTimeout(r.cfg.Session.IdleTimeout.Duration()))
		if err != nil {
			return fmt.Errorf("supervisor: building session manager: %w", err)
		}
		r.rc.sessions = m
		r.owned = append(r.owned, closerFunc(func() error { m.Close(); return nil }))
	}
	r.memory = r.rc.memory
	r.router = r.rc.router
	r.sessions = r.rc.sessions
	return nil
}

// resolve is every unit's ResolveFunc. It reads the current configuration snapshot,
// which enrolled swaps atomically, so a member who claims their invite mid-run is
// recognised by the group unit's next message without any unit being restarted.
func (r *runner) resolve(in transport.Inbound) (domain.Scope, error) {
	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()
	return scope.Resolve(cfg, in)
}

// buildMemberUnit constructs one member's unit over a mux view scoped to their
// direct messages. Each unit gets its own capture engine and its own view; the
// only things units share are the seams shared by design in one process — the
// bot, the lore client, the router and the session manager.
func (r *runner) buildMemberUnit(m domain.Member) error {
	telegramID := m.TelegramID
	view := r.mux.View(func(in transport.Inbound) bool {
		return !in.IsGroup && in.UserID == telegramID
	})
	u, err := r.buildUnit(view, "member:"+string(m.ID))
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = append(r.pending, pendingUnit{key: unitKey{member: m.ID}, unit: u, view: view})
	r.served[telegramID] = struct{}{}
	r.mu.Unlock()
	r.tracker.add(unitKey{member: m.ID})
	return nil
}

// buildGroupUnit constructs the household group's unit over a view scoped to the
// configured group chat and nothing else.
func (r *runner) buildGroupUnit() error {
	groupChatID := r.cfg.Household.GroupChatID
	view := r.mux.View(func(in transport.Inbound) bool {
		return in.IsGroup && in.ChatID == groupChatID
	})
	u, err := r.buildUnit(view, "group")
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = append(r.pending, pendingUnit{key: unitKey{group: true}, unit: u, view: view})
	r.mu.Unlock()
	r.tracker.add(unitKey{group: true})
	return nil
}

func (r *runner) buildUnit(view transport.Transport, name string) (*assistant.Unit, error) {
	engine := capture.New(r.memory, view, capture.Options{
		MaxProposalsPerTurn: r.cfg.Capture.MaxProposalsPerTurn,
		Shared:              domain.SpaceID(r.cfg.Household.SharedSpace),
	})
	unitOpts := r.rc.unitOpts
	if unitOpts.HouseholdName == "" {
		unitOpts.HouseholdName = r.cfg.Household.Name
	}
	if unitOpts.SearchLimit == 0 {
		unitOpts.SearchLimit = r.cfg.Memory.SearchLimit
	}
	u, err := assistant.New(assistant.Deps{
		Resolve:   r.resolve,
		Memory:    r.memory,
		Router:    r.router,
		Transport: view,
		Sessions:  r.sessions,
		Capture:   engine,
		Logger:    r.logger.With("unit", name),
	}, unitOpts)
	if err != nil {
		return nil, fmt.Errorf("supervisor: building unit %s: %w", name, err)
	}
	return u, nil
}

// start launches every unit's goroutine, begins fanning updates out, and blocks
// until ctx is cancelled, stop is called, or the transport's update stream ends
// underneath it — the one failure a single-bot process cannot ride out, because
// with the stream gone no unit can receive anything.
//
// On cancellation start drains exactly as stop does, bounded by drainTimeout, and
// returns ctx.Err().
func (r *runner) start(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("supervisor: already started")
	}
	if r.stopping {
		r.mu.Unlock()
		return errors.New("supervisor: already stopped")
	}
	r.started = true
	r.startCtx = ctx
	units := r.pending
	r.pending = nil
	r.mu.Unlock()

	for _, pu := range units {
		if err := r.launch(ctx, pu); err != nil {
			r.shutdown(r.drainContext())
			return err
		}
	}
	if r.rc.claimer != nil {
		if err := r.launchEnrol(ctx); err != nil {
			r.shutdown(r.drainContext())
			return err
		}
	}
	r.mu.Lock()
	r.launched = true
	idle := r.active == 0
	r.mu.Unlock()
	if idle {
		// Possible only in pathological wiring; close so waiters don't hang.
		r.allDoneOnce.Do(func() { close(r.allDone) })
	}

	if err := r.mux.Start(ctx); err != nil {
		r.shutdown(r.drainContext())
		return fmt.Errorf("supervisor: starting mux: %w", err)
	}

	r.logStartup()

	select {
	case <-ctx.Done():
		r.shutdown(r.drainContext())
		return ctx.Err()
	case <-r.allDone:
		r.mu.Lock()
		stopping := r.stopping
		r.mu.Unlock()
		if stopping {
			<-r.stoppedCh
			return nil
		}
		// Every pump exited and nobody asked them to: the bot's update stream
		// ended. Drain what little is left and report it.
		r.shutdown(r.drainContext())
		return errors.New("supervisor: transport update stream ended")
	case <-r.stoppedCh:
		return nil
	}
}

// logStartup emits the one summary an operator should be able to read and know
// what this process will and will not do: the units served, each conversation's
// tier chain, and the mode's privacy posture, worded once, in the privacy package.
func (r *runner) logStartup() {
	for _, m := range r.rc.members {
		r.logger.Info("supervisor: serving member",
			"member", string(m.ID), "tiers", m.Tiers)
	}
	for _, id := range r.rc.unenrolled {
		r.logger.Info("supervisor: member not enrolled, no unit", "member", string(id))
	}
	if r.rc.group {
		r.cfgMu.RLock()
		tiers := r.cfg.Household.Tiers
		r.cfgMu.RUnlock()
		r.logger.Info("supervisor: serving household group", "tiers", tiers)
	}
	r.logger.Info("supervisor: started",
		"mode", r.rc.privacyMode.String(),
		"privacy", privacy.Statement(r.rc.privacyMode))
}

// launch opens a unit's update stream and gives it a goroutine.
func (r *runner) launch(ctx context.Context, pu pendingUnit) error {
	ch, err := pu.view.Updates(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: opening updates for unit: %w", err)
	}
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return errors.New("supervisor: stopping")
	}
	r.active++
	r.mu.Unlock()
	r.tracker.set(pu.key, StateStarting)
	go r.runUnit(pu.key, pu.unit, ch)
	return nil
}

// runUnit supervises one unit's goroutine. unitLoop returning an error means the
// unit panicked mid-turn; restarting it here means exactly what the package doc
// promises — run the loop again after a backoff — because a Unit holds no state a
// crash can corrupt beyond the turn that crashed.
func (r *runner) runUnit(k unitKey, u *assistant.Unit, ch <-chan transport.Inbound) {
	defer r.workerDone()
	r.tracker.set(k, StateReady)
	bo := newBackoff(r.rc.restartBackoff, r.rc.maxRestartBackoff)
	for {
		err := r.unitLoop(u, ch)
		if err == nil {
			r.tracker.set(k, StateStopped)
			return
		}
		r.logger.Error("supervisor: unit crashed, restarting", "unit", k.member, "group", k.group, "error", err)
		r.tracker.fail(k, err)
		if !r.sleep(bo.next()) {
			r.tracker.set(k, StateStopped)
			return
		}
		r.tracker.set(k, StateReady)
	}
}

// unitLoop feeds one unit until its stream closes or a drain begins. It returns
// nil for both of those and an error only when a turn panicked.
func (r *runner) unitLoop(u *assistant.Unit, ch <-chan transport.Inbound) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("supervisor: unit panicked: %v", rec)
		}
	}()
	for {
		// Prefer the drain signal over queued backlog: drained means "stop
		// accepting", and a message accepted but not yet started is not a turn
		// the household is owed.
		select {
		case <-r.draining:
			return nil
		default:
		}
		select {
		case <-r.draining:
			return nil
		case in, ok := <-ch:
			if !ok {
				return nil
			}
			r.handleErr(u.Handle(r.turnCtx, in), in)
		}
	}
}

// handleErr triages one turn's error. Not-enrolled is answered with the silence it
// asks for; cancellation is the shutdown path; everything else is logged and the
// unit keeps serving.
func (r *runner) handleErr(err error, in transport.Inbound) {
	switch {
	case err == nil:
	case errors.Is(err, scope.ErrNotEnrolled):
		r.logger.Debug("supervisor: dropped message from unrecognised sender", "chat", in.ChatID)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		r.logger.Debug("supervisor: turn abandoned during shutdown", "chat", in.ChatID)
	default:
		r.logger.Warn("supervisor: turn failed", "chat", in.ChatID, "error", err)
	}
}

// launchEnrol starts the pump that carries messages from strangers to the Claimer.
// Its view accepts direct messages from any sender no unit serves, so it never
// shadows a member — including one whose unit was created after this view was.
func (r *runner) launchEnrol(ctx context.Context) error {
	view := r.mux.View(func(in transport.Inbound) bool {
		return !in.IsGroup && !r.servesUser(in.UserID)
	})
	ch, err := view.Updates(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: opening updates for enrolment: %w", err)
	}
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return errors.New("supervisor: stopping")
	}
	r.active++
	r.mu.Unlock()
	go r.runEnrol(view, ch)
	return nil
}

func (r *runner) servesUser(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.served[id]
	return ok
}

// runEnrol hands each stranger's message to the Claimer and sends back exactly the
// messages it returned — which is nothing, on every path but a successful claim.
// Errors are for the log and never for the sender; turning one into a reply would
// confirm the bot exists, which is the fact silence protects.
func (r *runner) runEnrol(view transport.Transport, ch <-chan transport.Inbound) {
	defer r.workerDone()
	for {
		select {
		case <-r.draining:
			return
		case in, ok := <-ch:
			if !ok {
				return
			}
			res, err := r.rc.claimer.Handle(r.turnCtx, in)
			if err != nil {
				r.logger.Warn("supervisor: enrolment attempt failed (sender got silence)", "error", err)
			}
			for _, out := range res.Messages {
				if err := view.Send(r.turnCtx, out); err != nil {
					r.logger.Warn("supervisor: sending onboarding failed", "error", err)
					break
				}
			}
			if res.Enrolled {
				r.enrolled(res.Member)
			}
		}
	}
}

// enrolled brings a freshly claimed member into service without a restart: fold
// their binding into a new configuration snapshot, swap it in, and give them a
// unit. The swap is copy-on-write, so no unit ever reads a half-updated member set.
func (r *runner) enrolled(m domain.Member) {
	r.cfgMu.Lock()
	r.cfg = configWithBinding(r.cfg, m)
	r.cfgMu.Unlock()
	r.tracker.promote(m.ID)

	if err := r.buildMemberUnit(m); err != nil {
		r.logger.Error("supervisor: building unit for new member", "member", string(m.ID), "error", err)
		return
	}
	r.mu.Lock()
	var pu pendingUnit
	if n := len(r.pending); n > 0 {
		pu = r.pending[n-1]
		r.pending = r.pending[:n-1]
	}
	ctx := r.startCtx
	r.mu.Unlock()
	if pu.unit == nil || ctx == nil {
		return
	}
	if err := r.launch(ctx, pu); err != nil {
		r.logger.Error("supervisor: launching unit for new member", "member", string(m.ID), "error", err)
	}
	r.logger.Info("supervisor: member enrolled and serving", "member", string(m.ID))
}

func (r *runner) workerDone() {
	r.mu.Lock()
	r.active--
	last := r.active == 0 && r.launched
	r.mu.Unlock()
	if last {
		r.allDoneOnce.Do(func() { close(r.allDone) })
	}
}

// sleep waits d unless a drain begins first.
func (r *runner) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.draining:
		return false
	}
}

// drainContext bounds a shutdown that has no caller-supplied deadline.
func (r *runner) drainContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), r.rc.drainTimeout)
	// The context lives for the length of the drain; shutdown runs once, so tie
	// the cancel to its completion.
	go func() {
		<-r.stoppedCh
		cancel()
	}()
	return ctx
}

// stop drains and shuts down: intake stops immediately, the turn each unit is in
// finishes, every session is locked, and only then does stop return. ctx bounds
// the wait for in-flight turns; when it expires they are cancelled, which is the
// one case a turn can be cut mid-write, and the ctx error is returned so the
// caller knows the drain was not clean. Idempotent; callable before start.
func (r *runner) stop(ctx context.Context) error {
	return r.shutdown(ctx)
}

func (r *runner) shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.stopping = true
		started := r.started
		idle := r.active == 0
		r.mu.Unlock()

		// Stop intake first: every pump sees draining and returns after the turn
		// it is in. Backlogged messages that never started a turn are dropped —
		// drained means no new work, and only the turns already speaking to a
		// member are owed a finish.
		close(r.draining)

		if started && !idle {
			select {
			case <-r.allDone:
			case <-ctx.Done():
				r.stopErr = ctx.Err()
				// The grace ran out. Cut the remaining turns and give them a
				// moment to observe it rather than abandoning their goroutines.
				r.turnCancel()
				select {
				case <-r.allDone:
				case <-time.After(5 * time.Second):
					r.logger.Error("supervisor: turns did not exit after cancellation")
				}
			}
		}

		// Only now close the mux. The order matters: a closed view refuses Send,
		// so closing it while a turn was still finishing would eat the reply of
		// the very turn the drain existed to protect.
		_ = r.mux.Close()

		// Sessions are locked only after the in-flight turns are done, because a
		// turn cut off from its key mid-write is how a member loses a capture
		// they confirmed.
		r.turnCancel()
		r.sessions.LockAll()
		r.tracker.stopAll()
		r.closeOwned()
		close(r.stoppedCh)
	})
	<-r.stoppedCh
	return r.stopErr
}

func (r *runner) closeOwned() {
	for _, c := range r.owned {
		if err := c.Close(); err != nil {
			r.logger.Warn("supervisor: closing dependency", "error", err)
		}
	}
	r.owned = nil
}

// health reports every unit's condition from the runner's own records. It never
// touches the transport, the router, lore or anything else external, and is
// callable before start and after stop.
func (r *runner) health() []UnitHealth {
	return r.tracker.snapshot()
}

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
