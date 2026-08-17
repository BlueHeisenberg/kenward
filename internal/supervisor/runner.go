package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/remind"
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
	// bot names which of the household's bots this process polls: a member's id for
	// their own agent's bot, empty for the household's own. It is the same fact
	// botToken resolves a token for, and scope.Resolve needs it because Telegram
	// does not put it on the wire — a private chat's id is the member's account id
	// and is the same whichever bot they are talking to.
	bot domain.MemberID
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
	// serveOnEnrol says whether a freshly claimed member gets a unit in this
	// process. Nil serves everyone, which is simple mode; a single-unit process
	// serves exactly its own member and leaves everyone else to their own pods.
	serveOnEnrol func(domain.Member) bool
	// unlockOnEnrol gives that member the key their unit needs. It is called only
	// for a member serveOnEnrol has already admitted.
	unlockOnEnrol UnlockOnEnrol

	// How to build the nil seams.
	//
	// botToken resolves this process's bot token through config's Secrets API —
	// file, environment variable or systemd credential, whichever the
	// configuration states — at the moment the transport is built. It is a
	// closure so the resolved value never sits on any struct a logger could
	// print.
	botToken func() (config.Secret, error)
	// endpointKey resolves one endpoint's API key at call time, for the same
	// reason botToken is a closure: resolution stays owned by internal/config
	// and the value rests nowhere.
	endpointKey routing.KeyFunc
	sessionMode session.Mode
	loreHome    string
	lookupEnv   config.LookupEnvFunc

	// remindOpts tunes every unit's reminder store. It is one set of numbers for the
	// household because the cap is a household policy, but each unit gets its own
	// Store over its own file: the limits are shared, the state never is.
	remindOpts        remind.Options
	unitOpts          assistant.Options
	logger            *slog.Logger
	drainTimeout      time.Duration
	restartBackoff    time.Duration
	maxRestartBackoff time.Duration
	// healthyReset is how long a unit must serve after a panic before its
	// backoff schedule returns to base.
	healthyReset time.Duration
	// cancelGrace is how long a drain that has already run out of patience waits
	// for cancelled goroutines to observe it before giving up on them.
	cancelGrace time.Duration
	now         func() time.Time
	privacyMode privacy.Mode
}

// UnlockOnEnrol provisions and unlocks a member's key at the moment they claim
// their invite, in the process that is about to serve them.
//
// It exists because enrolment must be complete when it reports complete. Keys were
// provisioned and unlocked at startup only, so a member who claimed while the node
// was running got their unit and their onboarding — and then the locked notice on
// their first private message, until an operator restarted the node. That is an
// operator's remedy for a fault only the member can see, on the first thing a
// household ever does with kenward.
//
// It is a closure rather than a passphrase field for two reasons: the value never
// rests on a struct a logger could print, exactly as botToken does not; and custody
// stays with whoever read the passphrase. Which passphrase that is differs by mode
// and must — in simple mode it is the operator's node passphrase, which by that
// mode's own definition wraps every member's key; in a pod it is that member's own,
// and the runner calls this only for a member the process is entitled to serve. See
// runner.enrolled, where the order of those two decisions is the whole safeguard.
type UnlockOnEnrol func(context.Context, domain.Member) error

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
	// clockCtx is what the reminder clocks run under. It is cancelled the moment a
	// drain begins — before in-flight turns are waited for — because a drain is
	// exactly the wrong time to start sending a household messages it did not ask
	// for, and unlike a turn there is nobody waiting on one to finish.
	clockCtx    context.Context
	clockCancel context.CancelFunc
	// clockWg tracks the reminder clocks. They are kept off the active-worker count
	// on purpose; see launchClock.
	clockWg sync.WaitGroup
	// draining tells unit pumps to stop after the turn they are in.
	draining chan struct{}
	// turnWg tracks in-flight turns. Turns run on their own goroutines — the
	// Unit serialises them itself, and calling Handle concurrently is what lets
	// a member who ignores a capture question's buttons still get their next
	// message answered — so the drain waits on this after the pumps have stopped
	// dispatching.
	turnWg sync.WaitGroup
	// allDone closes when the last pump goroutine exits.
	allDone     chan struct{}
	allDoneOnce sync.Once
	// stoppedCh closes when shutdown has fully completed.
	stoppedCh chan struct{}
	stopOnce  sync.Once
	stopErr   error

	// testHookBackoff observes each restart delay before it is slept, so tests
	// can assert the schedule without measuring wall time. Never set in
	// production; set before start and read-only after.
	testHookBackoff func(unitKey, time.Duration)

	mu       sync.Mutex
	started  bool
	stopping bool
	launched bool
	active   int
	startCtx context.Context
	pending  []pendingUnit
	served   map[int64]struct{}
	units    map[unitKey]struct{}
	// tutorials maps a chat with an onboarding in flight to that onboarding.
	//
	// It is what makes the tutorial multi-turn without a second reader of the update
	// stream. Between a claim and the end of the tutorial the member is served by no
	// unit, so their messages keep arriving at the enrolment pump — the same pump —
	// which hands them across rather than putting them to the Claimer again.
	tutorials map[int64]*tutorial
}

// tutorial is one onboarding in flight, as the enrolment pump sees it.
type tutorial struct {
	// answers is where a typed message goes. The send onto it is non-blocking, so a
	// message is delivered only while the tutorial is actually waiting for one.
	answers chan transport.Inbound
	// nudge is what to say to a member who typed while a button question was up, or
	// nil when there is nothing to say. The tutorial arms it as each question goes up
	// and disarms it as the question comes down — it is the only thing that knows
	// which language to say it in, since the member may have chosen one at question
	// one — and the pump takes it with Swap, so exactly one message per question
	// reaches the member however much they type.
	nudge atomic.Pointer[string]
}

// pendingUnit is a constructed unit waiting for start to give it a goroutine.
type pendingUnit struct {
	key  unitKey
	unit *assistant.Unit
	view transport.Transport
	// clock delivers this unit's reminders. It gets a goroutine of its own beside
	// the unit's pump, and it sends through the unit's own view — so a reminder can
	// only ever reach the conversation the unit already serves.
	clock *remind.Clock
}

// newRunner wires the machinery over cfg. It returns ErrNoUnits when rc names
// nothing to run and no Claimer could change that.
func newRunner(cfg *config.Config, rc runnerConfig) (*runner, error) {
	turnCtx, turnCancel := context.WithCancel(context.Background())
	clockCtx, clockCancel := context.WithCancel(context.Background())
	r := &runner{
		rc:          rc,
		logger:      rc.logger,
		tracker:     newTracker(rc.now),
		cfg:         snapshotConfig(cfg),
		turnCtx:     turnCtx,
		turnCancel:  turnCancel,
		clockCtx:    clockCtx,
		clockCancel: clockCancel,
		draining:    make(chan struct{}),
		allDone:     make(chan struct{}),
		stoppedCh:   make(chan struct{}),
		served:      make(map[int64]struct{}),
		units:       make(map[unitKey]struct{}),
	}

	if err := r.buildDeps(); err != nil {
		turnCancel()
		clockCancel()
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
		// Under one agent each, the process holding the household's bot also holds
		// every member's private conversation with kenward. One unit each, not one
		// unit serving all of them: a Unit is one conversation — its own history
		// ring, its own turn slot, its own capture engine — and a shared one would
		// put David's private message to kenward into the ring the group chat's
		// prompt is assembled from.
		if r.cfg.AgentPerMember() {
			for _, m := range r.cfg.DomainMembers() {
				if !m.Enrolled() {
					continue
				}
				if err := r.buildHouseholdUnit(m); err != nil {
					turnCancel()
					r.closeOwned()
					return nil, err
				}
			}
		}
	}
	if len(r.pending) == 0 && rc.claimer == nil {
		turnCancel()
		clockCancel()
		r.closeOwned()
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return r, nil
}

func (r *runner) buildDeps() error {
	if r.rc.transport == nil {
		// Resolved here, at the point of use, and handed straight to the
		// transport: the value is never stored where %#v could print it.
		token, err := r.rc.botToken()
		if err != nil {
			return fmt.Errorf("supervisor: resolving bot token: %w", err)
		}
		t, err := transport.NewTelegram(token.Value(), transport.WithLogger(r.logger))
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
			routing.NewHTTPCompleter(nil, r.rc.endpointKey, r.logger))
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
	return scope.Resolve(cfg, r.rc.bot, in)
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
	k := unitKey{member: m.ID}
	u, clock, err := r.buildUnit(view, k, "member:"+string(m.ID), m.Tiers)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = append(r.pending, pendingUnit{key: k, unit: u, view: view, clock: clock})
	r.served[telegramID] = struct{}{}
	r.units[k] = struct{}{}
	r.mu.Unlock()
	r.tracker.add(k)
	return nil
}

// buildGroupUnit constructs the household group's unit over a view scoped to the
// configured group chat and nothing else.
func (r *runner) buildGroupUnit() error {
	groupChatID := r.cfg.Household.GroupChatID
	view := r.mux.View(func(in transport.Inbound) bool {
		return in.IsGroup && in.ChatID == groupChatID
	})
	k := unitKey{group: true}
	u, clock, err := r.buildUnit(view, k, "group", r.cfg.Household.Tiers)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = append(r.pending, pendingUnit{key: k, unit: u, view: view, clock: clock})
	r.units[k] = struct{}{}
	r.mu.Unlock()
	r.tracker.add(k)
	return nil
}

// buildHouseholdUnit constructs one member's private conversation with kenward, over
// a view scoped to their direct messages on the household's bot.
//
// The view is the same shape as buildMemberUnit's and means something different, and
// the difference is entirely the bot this process polls: on a member's own bot those
// messages are their own assistant's, and on the household's they are kenward's. The
// view does not decide that and could not — scope.Resolve does, from the bot — so the
// two can never both exist over one transport.
//
// It runs on the household's tier chain, not the member's, because everything in this
// conversation is the household's material.
func (r *runner) buildHouseholdUnit(m domain.Member) error {
	telegramID := m.TelegramID
	view := r.mux.View(func(in transport.Inbound) bool {
		return !in.IsGroup && in.UserID == telegramID
	})
	k := unitKey{member: m.ID, group: true}
	u, clock, err := r.buildUnit(view, k, "household:"+string(m.ID), r.cfg.Household.Tiers)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = append(r.pending, pendingUnit{key: k, unit: u, view: view, clock: clock})
	r.served[telegramID] = struct{}{}
	r.units[k] = struct{}{}
	r.mu.Unlock()
	r.tracker.add(k)
	return nil
}

// buildUnit constructs one unit and the clock that serves it.
//
// The reminder store is built here, per unit, over a path derived from the unit's own
// identity — never one store handed to several units. That is the same rule the
// capture engine follows and it is not stylistic: a member's unit shares no mutable
// state with any other, which is what lets identical code run as a goroutine beside
// its siblings and alone in a pod, and a reminder table keyed by member would be the
// first thing to break it.
func (r *runner) buildUnit(view transport.Transport, k unitKey, name string, tiers []string) (*assistant.Unit, *remind.Clock, error) {
	store, err := remind.Open(r.cfg.RemindersPath(k.member, k.group), r.rc.remindOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("supervisor: opening reminders for unit %s: %w", name, err)
	}
	logger := r.logger.With("unit", name)
	unitOpts := r.unitOptions(k, tiers)
	// The capture engine and the assistant read the same language, resolved once
	// here rather than twice: a member whose announcements arrive in Catalan and
	// whose refusals arrive in English would be a worse product than either alone.
	engine := r.captureEngine(view, unitOpts.Persona.Language)
	u, err := assistant.New(assistant.Deps{
		Resolve:   r.resolve,
		Memory:    r.memory,
		Router:    r.router,
		Transport: view,
		Sessions:  r.sessions,
		Capture:   engine,
		Reminders: store,
		Logger:    logger,
	}, unitOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("supervisor: building unit %s: %w", name, err)
	}
	// The clock is given the unit's own view and nothing else. There is deliberately
	// no router here: a reminder sends stored text and cannot reach a model, so a
	// timer cannot spend a token or widen a tier chain even by accident.
	clock := remind.NewClock(store, func(ctx context.Context, chatID int64, text string) error {
		return view.Send(ctx, transport.Outbound{ChatID: chatID, Text: text})
	}, r.rc.now, logger)
	return u, clock, nil
}

// captureEngine builds one unit's capture engine over its own transport view.
//
// It is separate from buildUnit so that the write policy has somewhere to be asserted:
// the engine keeps its options private, so the only way to check that a household's
// capture.private_writes reached it is to build one the way production does and watch
// what it does with a proposal.
func (r *runner) captureEngine(view transport.Transport, language string) *capture.Engine {
	opts := capture.Options{
		MaxProposalsPerTurn: r.cfg.Capture.MaxProposalsPerTurn,
		Shared:              domain.SpaceID(r.cfg.Household.SharedSpace),
		// Which language this conversation's announcements and buttons are in. It
		// is the unit's persona language, which for the group chat is the
		// household's — the group has no member to ask, and every notice it emits
		// is addressed to everyone.
		Language: language,
	}
	// Mapped rather than shared: the configuration's spelling is an operator's
	// vocabulary and the engine's is a Go enum, and the two packages are entitled to
	// disagree about either without one silently redefining the other.
	if r.cfg.Capture.PrivateWrites == config.PrivateWriteAsk {
		opts.PrivateWrites = capture.PrivateWriteAsk
	}
	return capture.New(r.memory, view, opts)
}

// unitOptions resolves one unit's options from the shared seed and the unit's own
// tier chain.
//
// The context budget and the completion cap are per unit because they are per
// scope: the assistant assembles the prompt before the router picks an endpoint, so
// both must hold at the smallest endpoint this conversation's chain can reach — the
// group's cloud chain and a member's local-only chain are legitimately different
// sizes, and one household-wide number gets one of them wrong. Explicit seed values
// win; otherwise both come from config.ChainLimits, and a chain that reaches no
// endpoint with either stated falls back to the assistant's own defaults.
func (r *runner) unitOptions(k unitKey, tiers []string) assistant.Options {
	o := r.rc.unitOpts
	if o.HouseholdName == "" {
		o.HouseholdName = r.cfg.Household.Name
	}
	// The persona is per unit because it is per conversation. The group chat is
	// always kenward's and always gets the household's; a member's private chat gets
	// their own under household.agents: per_member and the household's under shared,
	// which is what "one assistant for the household" means and is why the wizard has
	// to say that kenward's persona is everyone's persona before an admin chooses it.
	//
	// Not guarded by a zero check on the seed, unlike the numbers above: an explicit
	// seed would be a test's, and a test that sets a persona means that persona
	// rather than "unless the configuration has one".
	if o.Persona == (assistant.Persona{}) {
		o.Persona = personaFor(r.cfg, k)
	}
	if o.SearchLimit == 0 {
		o.SearchLimit = r.cfg.Memory.SearchLimit
	}
	window, maxTokens := r.cfg.ChainLimits(tiers)
	if o.ContextBudget == 0 {
		o.ContextBudget = window
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = maxTokens
	}
	// Not guarded by a zero check, unlike the numbers above: the off value is the
	// zero value, so a seed that turned it off would be indistinguishable from a
	// seed that said nothing, and the configuration is the authority either way.
	if !r.cfg.Memory.AnnouncesReads() {
		o.ReadNotices = assistant.ReadNoticesOff
	}
	// Not guarded by a zero check either, and for the same reason: off is the zero
	// value here too, so a seed that turned the schedule off would be
	// indistinguishable from a seed that never mentioned it.
	//
	// It is household-wide and reaches every unit, the group's included. A member's
	// tier chain is per member because it is that member's privacy policy; how stale
	// a conversation may get is not a privacy decision and has nothing per-member to
	// say. Each unit still keeps its own ring and crosses the boundary on its own
	// next turn, so the group chat and a member's chat are cleared independently.
	o.HistoryReset = r.cfg.History.ResetEvery.Duration()
	return o
}

// personaFor maps one unit onto the persona its conversation is written in.
//
// Mapped rather than shared, exactly as capture.PrivateWrites is: the configuration's
// spelling is an operator's vocabulary and the assistant's is this package's, and
// internal/assistant must not learn the shape of a configuration file in order to
// render a prompt. Which persona a unit gets is config.PersonaFor's decision; turning
// it into the assistant's type is this line's.
func personaFor(cfg *config.Config, k unitKey) assistant.Persona {
	p := cfg.HouseholdPersona()
	if !k.group {
		p = cfg.PersonaFor(string(k.member))
	}
	return assistant.Persona{
		Name:      p.AgentName,
		Language:  p.Language,
		Tone:      p.Tone,
		Character: p.Character,
	}
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
	if err := r.launchBackstop(ctx); err != nil {
		r.shutdown(r.drainContext())
		return err
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
	r.launchClock(pu)
	return nil
}

// launchClock gives one unit's reminder clock a goroutine.
//
// It is tracked on clockWg and deliberately NOT in the runner's active-worker count.
// That count exists to detect one specific failure — every pump exiting, which means
// the bot's update stream ended and no unit can receive anything — and a clock is not
// a pump. Counting one would keep the tally above zero forever, so a dead transport
// would never be noticed and the process would sit there serving nobody. It is the
// kind of mistake that only shows up as a hang, and it did.
func (r *runner) launchClock(pu pendingUnit) {
	if pu.clock == nil {
		return
	}
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.clockWg.Add(1)
	go func() {
		defer r.clockWg.Done()
		defer r.recoverPump(pu.key, "reminder clock")
		pu.clock.Run(r.clockCtx)
	}()
}

// runUnit is one unit's pump. Each message is dispatched to Handle on its own
// goroutine — the Unit serialises turns itself, and concurrent Handle calls are
// what let a member whose capture question is still waiting get their next
// message answered instead of a frozen assistant. A turn that panics reports
// back here; restarting the unit then means exactly what the package doc
// promises — pause intake for a backoff and dispatch again — because a Unit
// holds no state a crash can corrupt beyond the turn that crashed.
func (r *runner) runUnit(k unitKey, u *assistant.Unit, ch <-chan transport.Inbound) {
	defer r.workerDone()
	defer r.recoverPump(k, "unit")
	r.tracker.set(k, StateReady)
	bo := newBackoff(r.rc.restartBackoff, r.rc.maxRestartBackoff)
	// lastPanic dates the schedule so a unit that has been up since well before
	// this panic starts over at the base delay; see restartAfterPanic.
	var lastPanic time.Time
	panicCh := make(chan error, 1)
	for {
		// Prefer the drain signal over queued backlog: drained means "stop
		// accepting", and a message accepted but not yet started is not a turn
		// the household is owed. A pending panic likewise outranks new intake.
		select {
		case <-r.draining:
			r.tracker.set(k, StateStopped)
			return
		case <-panicCh:
			if !r.restartAfterPanic(k, bo, &lastPanic) {
				return
			}
			continue
		default:
		}
		select {
		case <-r.draining:
			r.tracker.set(k, StateStopped)
			return
		case <-panicCh:
			if !r.restartAfterPanic(k, bo, &lastPanic) {
				return
			}
		case in, ok := <-ch:
			if !ok {
				r.tracker.set(k, StateStopped)
				return
			}
			r.dispatchTurn(k, u, in, panicCh)
		}
	}
}

// restartAfterPanic pauses intake for the unit's backoff and puts it back in
// service, reporting false when the drain arrived first.
//
// A unit that stayed up for healthyReset after its last panic has its schedule
// returned to base before the pause is computed: without that, the delay only
// ever doubles, and one panic long ago leaves the next restart waiting the
// maximum delay for a unit that has been serving perfectly since. It is the same
// judgement the isolated supervisor makes on HealthyReset, made from the last
// failure rather than from an uptime clock because a pump between turns is
// otherwise indistinguishable from one that has never run.
func (r *runner) restartAfterPanic(k unitKey, bo *backoff, lastPanic *time.Time) bool {
	now := r.rc.now()
	if !lastPanic.IsZero() && now.Sub(*lastPanic) >= r.rc.healthyReset {
		bo.reset()
	}
	*lastPanic = now
	d := bo.next()
	if r.testHookBackoff != nil {
		r.testHookBackoff(k, d)
	}
	if !r.sleep(d) {
		r.tracker.set(k, StateStopped)
		return false
	}
	r.tracker.set(k, StateReady)
	return true
}

// recoverPump contains a panic raised by a pump goroutine itself, outside any
// turn. Only the turn handler had this: a panic in the enrolment pump, the
// backstop or a unit's own loop took the process with it, and one member's
// trouble taking the whole household down is the single thing the design
// promises never happens. The pump is over either way — there is no member
// waiting on a reply here, and nothing to restart — but the household keeps
// running and the failure is loud in the log.
func (r *runner) recoverPump(k unitKey, what string) {
	rec := recover()
	if rec == nil {
		return
	}
	err := fmt.Errorf("supervisor: %s pump panicked: %v", what, rec)
	r.logger.Error("supervisor: pump crashed; the rest of the household keeps running",
		"pump", what, "unit", k.member, "group", k.group, "error", err)
	r.tracker.fail(k, err)
	r.tracker.set(k, StateStopped)
}

// dispatchTurn runs one message's turn on its own goroutine, tracked for the
// drain. A panic is contained to the turn that raised it: it is recorded against
// the unit and reported to the pump, which pauses intake and restarts.
func (r *runner) dispatchTurn(k unitKey, u *assistant.Unit, in transport.Inbound, panicCh chan<- error) {
	r.turnWg.Add(1)
	go func() {
		defer r.turnWg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				err := fmt.Errorf("supervisor: unit panicked: %v", rec)
				r.logger.Error("supervisor: unit crashed, restarting",
					"unit", k.member, "group", k.group, "error", err)
				r.tracker.fail(k, err)
				select {
				case panicCh <- err:
				default:
				}
			}
		}()
		r.handleErr(u.Handle(r.turnCtx, in), in)
	}()
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

func (r *runner) hasUnit(k unitKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.units[k]
	return ok
}

// launchBackstop registers the catch-all view that makes scope resolution — not
// Mux matching — the authority over every inbound message. The unit views and the
// enrolment view are registered before it, so anything arriving here matched
// nothing; instead of the Mux silently dropping it, the backstop resolves it and
// acts on what resolution says. The Mux is thereby an optimisation: were its
// matching ever wrong, the message still meets scope.Resolve, and a unit meeting
// a message resolves it again inside Handle — the boundary holds even for a
// message that never passed through a view at all.
//
// Its match declines direct messages from users a unit serves, rather than
// relying on registration order, because units enrolled mid-run register their
// views after this one.
func (r *runner) launchBackstop(ctx context.Context) error {
	view := r.mux.View(func(in transport.Inbound) bool {
		return in.IsGroup || !r.servesUser(in.UserID)
	})
	ch, err := view.Updates(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: opening updates for scope backstop: %w", err)
	}
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return errors.New("supervisor: stopping")
	}
	r.active++
	r.mu.Unlock()
	go r.runBackstop(ch)
	return nil
}

// runBackstop resolves every unrouted message and answers with what the
// authorization boundary decided — which is silence on every path, but three
// different silences: a stranger is refused by resolution exactly as the design
// describes; a scope served by another process (a sibling pod's member, say) is
// simply not this process's conversation; and a scope this process runs a unit
// for reaching the backstop means Mux matching has drifted from scope resolution,
// which is logged as the bug it is rather than quietly absorbed.
func (r *runner) runBackstop(ch <-chan transport.Inbound) {
	defer r.workerDone()
	defer r.recoverPump(unitKey{}, "scope backstop")
	for {
		select {
		case <-r.draining:
			return
		case in, ok := <-ch:
			if !ok {
				return
			}
			sc, err := r.resolve(in)
			switch {
			case err != nil:
				// The designed outcome: unrecognised means silence, decided by
				// the authorization boundary, not by routing.
				r.logger.Debug("supervisor: refused by scope resolution", "chat", in.ChatID)
			case r.hasUnit(scopeUnitKey(sc)):
				r.logger.Error("supervisor: BUG: message resolved to a unit this process runs but no view matched; dropped",
					"chat", in.ChatID, "group", sc.Kind == domain.ScopeGroup)
			default:
				r.logger.Debug("supervisor: message for a unit served elsewhere", "chat", in.ChatID)
			}
		}
	}
}

// scopeUnitKey names the unit a resolved scope belongs to.
//
// Stated as "not a member's own conversation" rather than "is the group", because a
// household scope is both: it belongs to the household and it belongs to one member's
// chat, and it has a unit of its own for exactly that reason.
func scopeUnitKey(sc domain.Scope) unitKey {
	if sc.Member == nil {
		return unitKey{group: true}
	}
	if !sc.TouchesPrivateMemory() {
		return unitKey{member: sc.Member.ID, group: true}
	}
	return unitKey{member: sc.Member.ID}
}

// runEnrol hands each stranger's message to the Claimer and sends back exactly the
// messages it returned — which is nothing, on every path but a successful claim.
// Errors are for the log and never for the sender; turning one into a reply would
// confirm the bot exists, which is the fact silence protects.
func (r *runner) runEnrol(view transport.Transport, ch <-chan transport.Inbound) {
	defer r.workerDone()
	defer r.recoverPump(unitKey{}, "enrolment")

	// A node that died between a member's greeting and the end of their tutorial
	// owes them the explanation. Nothing else would ever send it: they are enrolled
	// now, so they never reach this pump again.
	if err := r.rc.claimer.FinishInterrupted(r.turnCtx, view); err != nil {
		r.logger.Warn("supervisor: could not finish an interrupted onboarding", "error", err)
	}

	for {
		select {
		case <-r.draining:
			return
		case in, ok := <-ch:
			if !ok {
				return
			}
			// A member part-way through onboarding is not a stranger presenting a
			// code; what they sent is an answer, and the tutorial is waiting for it.
			if r.deliverToTutorial(view, in) {
				continue
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
				// Before the tutorial, not after it. See bound.
				r.bound(res.Member)
				r.startTutorial(view, res.Member, in.ChatID)
			}
		}
	}
}

// deliverToTutorial hands a message to an onboarding in flight in that chat,
// reporting whether it took it.
//
// The send is non-blocking onto an unbuffered channel, so a message is delivered
// only if the tutorial is waiting for typed input at this instant. That is the whole
// filter: something a member types while a button question is on screen is not an
// answer to anything, and buffering it would make it the answer to whatever question
// comes next.
//
// Dropping it silently was the other half, and it was wrong. The member is looking at
// a question that reads as answerable by typing, they typed, and nothing at all came
// back — the one outcome that teaches somebody the node is broken. So the drop says
// so, in the tutorial's own language rather than the household's: the sentence comes
// from the tutorial, which is the only thing that knows a member chose Spanish at
// question one, and Swap takes it, so a member who types five times gets one reply
// and the next question arms a fresh one.
func (r *runner) deliverToTutorial(view transport.Transport, in transport.Inbound) bool {
	r.mu.Lock()
	tut, ok := r.tutorials[in.ChatID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case tut.answers <- in:
		return true
	default:
	}
	if s := tut.nudge.Swap(nil); s != nil && *s != "" {
		if err := view.Send(r.turnCtx, transport.Outbound{ChatID: in.ChatID, Text: *s}); err != nil {
			r.logger.Warn("supervisor: telling a member their typed answer was not one failed", "error", err)
		}
	}
	return true
}

// startTutorial walks a freshly claimed member through their personal setup, and
// brings them into service when it ends.
//
// It runs on its own goroutine so that one member answering slowly does not stop the
// household enrolling anybody else, and it is tracked on turnWg so a drain waits for
// it exactly as it waits for a turn.
//
// The member is promoted afterwards rather than before, because until then their
// messages have to keep arriving here for the tutorial to read. What that costs is
// bounded and worth naming: for the length of the tutorial — the member's own doing,
// and capped by enrol.DefaultTutorialTimeout — anything they say in the household
// group is unrouted. They are at that moment reading a private walk-through, and the
// alternative is a second reader racing this one for their next message.
func (r *runner) startTutorial(view transport.Transport, m domain.Member, chatID int64) {
	answers := make(chan transport.Inbound)
	session := &tutorial{answers: answers}
	r.mu.Lock()
	if r.tutorials == nil {
		r.tutorials = make(map[int64]*tutorial)
	}
	r.tutorials[chatID] = session
	r.mu.Unlock()

	tut := r.rc.claimer.Tutorial(view, m, chatID, answers)
	if tut.Logger == nil {
		tut.Logger = r.logger
	}
	// What to say to a member who types while a button question is up. The tutorial
	// owns the sentence because it owns the language; this pump owns the sending
	// because it is the only thing that sees the message. See deliverToTutorial.
	tut.Nudge = func(s string) {
		if s == "" {
			session.nudge.Store(nil)
			return
		}
		session.nudge.Store(&s)
	}
	r.turnWg.Add(1)
	go func() {
		defer r.turnWg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.tutorials, chatID)
			r.mu.Unlock()
			// Their answers, before their unit is built from them. The store has
			// the same values and is the record, but nothing re-reads the state
			// file while the node runs — so a member who has just named their
			// agent Alfred would be answered by kenward until the next restart,
			// which is the same feature not working with none of the symptoms.
			//
			// Unconditionally, like the promotion below: a tutorial that stopped
			// half way still gave the answers it collected, and they are already
			// on disk.
			r.personaAnswered(m.ID, tut.Answered().PersonaConfig)
			// Unconditionally: a tutorial that failed, was abandoned or panicked
			// still leaves an enrolled member, and withholding their unit over a
			// setup question would be the worse failure by a distance.
			r.enrolled(m)
		}()
		defer func() {
			if rec := recover(); rec != nil {
				r.logger.Error("supervisor: onboarding tutorial panicked; the member is enrolled on defaults",
					"member", string(m.ID), "error", rec)
			}
		}()
		if err := tut.Run(r.turnCtx); err != nil {
			r.logger.Warn("supervisor: onboarding tutorial did not finish", "member", string(m.ID), "error", err)
		}
	}()
}

// personaAnswered folds what a member chose in their tutorial into the running
// configuration, copy-on-write, exactly as a binding is folded.
//
// Per field, and an unanswered field leaves whatever the file said, which is
// config.MergeState's rule and has to be the same rule: the two are the same fold
// happening at different times — this one while the node runs, that one on the next
// start — and a member who skipped the language question must not lose a language an
// operator wrote for them either way.
func (r *runner) personaAnswered(id domain.MemberID, p config.PersonaConfig) {
	if p == (config.PersonaConfig{}) {
		return
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	out := snapshotConfig(r.cfg)
	for i := range out.Members {
		if domain.MemberID(out.Members[i].ID) != id {
			continue
		}
		held := out.Members[i].Persona
		out.Members[i].Persona = config.PersonaConfig{
			AgentName: orElse(p.AgentName, held.AgentName),
			Language:  orElse(p.Language, held.Language),
			Tone:      orElse(p.Tone, held.Tone),
			Character: orElse(p.Character, held.Character),
		}
		r.cfg = out
		return
	}
}

func orElse(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// enrolled brings a freshly claimed member into service without a restart: fold
// their binding into a new configuration snapshot, swap it in, provision and unlock
// their key, and give them a unit. The swap is copy-on-write, so no unit ever reads
// a half-updated member set.
//
// The key comes before the unit deliberately. A unit without one answers its
// member's first private message with the locked notice — a message naming a remedy
// only an operator can perform — and enrolment would be reporting itself complete
// while the thing it exists to deliver does not work.
//
// The order of the two enrolment decisions is load-bearing. serveOnEnrol runs first,
// so a claim that lands on this bot for somebody another process serves returns
// before any key is touched. Without that, a member's pod would provision a second
// member's key under its own passphrase, which is the one failure isolated mode
// exists to prevent — worse than the bug this path fixes, and invisible.
func (r *runner) enrolled(m domain.Member) {
	// Already done at the claim, and repeated because enrolled is also reached by
	// paths that did not come through this process's enrolment pump. It is a
	// copy-on-write fold of the same values: doing it twice changes nothing.
	r.bound(m)

	if r.rc.serveOnEnrol != nil && !r.rc.serveOnEnrol(m) {
		// Someone else's claim landed on this bot. Their unit lives in their
		// own process; minting one here would put a member's conversation in
		// an address space it must never enter.
		r.logger.Info("supervisor: member enrolled; served by another process", "member", string(m.ID))
		return
	}
	r.tracker.promote(m.ID)

	if r.rc.unlockOnEnrol != nil {
		// Failing here is not a reason to withhold the unit: the member is
		// enrolled either way, their group conversation works, and a unit that
		// says it is locked is better than a member the node treats as a
		// stranger. The operator sees why in the log, and a restart still fixes
		// it.
		if err := r.rc.unlockOnEnrol(r.turnCtx, m); err != nil {
			r.logger.Error("supervisor: could not unlock the new member's key; their private chat stays locked until this node restarts",
				"member", string(m.ID), "error", err)
		}
	}

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

// bound folds a claim into the running configuration, copy-on-write, at the moment
// the code is redeemed.
//
// It is separate from enrolled, and earlier, because the two answer different
// questions and only one of them can wait. Whether this process runs a unit for the
// member is enrolled's business and genuinely does wait for their tutorial: until it
// ends their messages have to keep reaching the enrolment pump, which is what reads
// their answers. Whether the household's group chat recognises them is this, and it
// waits for nothing — the binding exists from the redemption, the group unit serves
// the chat rather than the member, and the only thing standing between the two was
// scope.Resolve reading a configuration that had not been told.
//
// The cost of getting that wrong is a silence nobody can see. A member who claimed
// mid-run, finished their DM and then spoke in the group got no reply and no log
// line: resolution refused them exactly as it refuses a stranger, which is a Debug
// line and is meant to be, because answering a stranger at all confirms the bot. The
// remedy was a restart. There is now nothing to restart for.
//
// The fold is also what lets personaAnswered find a member the Binder created from
// an invited name: it runs before enrolled, and before this it was searching a
// configuration that did not yet have the row.
func (r *runner) bound(m domain.Member) {
	r.cfgMu.Lock()
	r.cfg = configWithBinding(r.cfg, m)
	r.cfgMu.Unlock()
}

// workerDone retires one pump and, when it was the last, tells the drain so.
//
// `launched` is what stops a transient zero during startup from being read as "the
// update stream ended" — between the first pump exiting and the second being
// launched, the count really is zero and means nothing. `stopping` has to count too,
// and its absence was a twenty-second hang: a Stop that lands while start is still
// walking its units makes the next launch return "supervisor: stopping", so start
// gives up and shuts down having never reached `launched = true`. The pumps it did
// start then exit on the drain signal, the count reaches zero, and nobody closes
// allDone — so the drain waits out its whole deadline and reports itself unclean for
// pumps that had all gone. Once stopping is set, launch refuses to add any more, so a
// zero count is final and closing on it is right.
func (r *runner) workerDone() {
	r.mu.Lock()
	r.active--
	last := r.active == 0 && (r.launched || r.stopping)
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

		// The clocks stop here rather than at the end, with the sessions. A
		// reminder is not owed a finish the way a turn is — nobody is waiting on
		// one — and starting to message a household during the shutdown that is
		// taking their assistant away is the worst possible moment for the one
		// thing this node sends unprompted.
		//
		// Waited for here and not with the turns: a clock returns as soon as it sees
		// the cancellation, and nothing downstream may run while one could still be
		// mid-send through a view the mux is about to close.
		r.clockCancel()
		r.clockWg.Wait()

		// pumpsDone records whether every pump was seen to exit. Waiting on
		// turnWg is only meaningful once they have: a pump still in dispatchTurn
		// can Add while this goroutine Waits, which is the one thing a WaitGroup
		// forbids, and the count it would be waiting on is not a closed set
		// anyway.
		pumpsDone := true
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
				case <-time.After(r.rc.cancelGrace):
					r.logger.Error("supervisor: pumps did not exit after cancellation")
					pumpsDone = false
				}
			}
		}

		// The pumps have stopped dispatching; now wait for the turns they already
		// dispatched, capture questions included. No pump is left to add more, so
		// this wait is against a closed set. A wedged pump is the exception: its
		// turns have been cancelled and are not waited for, because there is no
		// closed set to wait on and the drain has already been reported unclean.
		if pumpsDone {
			turnsDone := make(chan struct{})
			go func() {
				r.turnWg.Wait()
				close(turnsDone)
			}()
			select {
			case <-turnsDone:
			case <-ctx.Done():
				if r.stopErr == nil {
					r.stopErr = ctx.Err()
				}
				r.turnCancel()
				select {
				case <-turnsDone:
				case <-time.After(r.rc.cancelGrace):
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

// remindOptions reads the household's reminder policy out of the configuration.
//
// The timezone error is propagated rather than defaulted away. Validation has already
// rejected an unloadable name, so reaching here means the supervisor was handed an
// unvalidated configuration — and a household whose reminders quietly arrived on UTC
// instead of their own clock would have no way to tell that from a bug.
func remindOptions(cfg *config.Config) (remind.Options, error) {
	loc, err := cfg.Reminders.Location()
	if err != nil {
		return remind.Options{}, fmt.Errorf("supervisor: reminders.timezone: %w", err)
	}
	return remind.Options{
		Location:  loc,
		MaxPerDay: cfg.Reminders.MaxPerDay,
		MaxStored: cfg.Reminders.MaxStored,
		CatchUp:   cfg.Reminders.CatchUpWindow.Duration(),
	}, nil
}

// endpointKeyFunc builds the router's key resolver over config's accessor, so
// precedence across file, environment and credential sources lives in exactly
// one place. An endpoint the configuration does not name gets no key, which is
// also what an endpoint that needs no authentication gets.
func endpointKeyFunc(cfg *config.Config, secrets *config.Secrets) routing.KeyFunc {
	endpoints := append([]config.EndpointConfig(nil), cfg.Endpoints...)
	return func(ep routing.Endpoint) (string, error) {
		for _, ec := range endpoints {
			if ec.Name == ep.Name {
				key, err := ec.APIKey(secrets)
				if err != nil {
					return "", err
				}
				return key.Value(), nil
			}
		}
		return "", nil
	}
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
