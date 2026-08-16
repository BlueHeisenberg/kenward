package supervisor

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

const (
	davidTelegramID = int64(111)
	groupChatID     = int64(-100)
)

// simpleTestConfig is a household with one enrolled member (david), one who has
// not claimed their invite (ana), and a group chat.
func simpleTestConfig() *config.Config {
	return &config.Config{
		Mode: config.ModeSimple,
		Household: config.HouseholdConfig{
			Name:        "Home",
			SharedSpace: "household",
			GroupChatID: groupChatID,
			Tiers:       []string{"cloud"},
		},
		Telegram: config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Members: []config.MemberConfig{
			{
				ID: "david", Name: "David", TelegramID: davidTelegramID,
				PrivateSpace: "david-private", Tiers: []string{"local"},
				EnrolledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			{
				ID: "ana", Name: "Ana",
				PrivateSpace: "ana-private", Tiers: []string{"local"},
			},
		},
		Memory: config.MemoryConfig{LoreCommand: []string{"lore", "mcp"}, SearchLimit: 4},
	}
}

// simpleHarness wires a Simple over fakes and runs Start in the background.
type simpleHarness struct {
	sup      *Simple
	fake     *transport.Fake
	mem      *fakeMemory
	router   *fakeRouter
	sessions *fakeSessions
	cancel   context.CancelFunc
	startErr chan error
}

func newSimpleHarness(t *testing.T, cfg *config.Config, mutate func(*SimpleOptions)) *simpleHarness {
	t.Helper()
	h := &simpleHarness{
		fake:     transport.NewFake(),
		mem:      &fakeMemory{},
		router:   &fakeRouter{},
		sessions: newFakeSessions("david"),
		startErr: make(chan error, 1),
	}
	opts := SimpleOptions{
		Transport:      h.fake,
		Memory:         h.mem,
		Router:         h.router,
		Sessions:       h.sessions,
		RestartBackoff: time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	sup, err := NewSimple(cfg, opts)
	if err != nil {
		t.Fatalf("NewSimple: %v", err)
	}
	h.sup = sup
	return h
}

func (h *simpleHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.startErr <- h.sup.Start(ctx) }()
	waitFor(t, "units ready", func() bool {
		hs := mustHealth(t, h.sup)
		return hs["david"].State == StateReady && hs["group"].State == StateReady
	})
}

func (h *simpleHarness) stop(t *testing.T) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-h.startErr:
		if err != nil {
			t.Fatalf("Start returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	h.cancel()
	_ = h.fake.Close()
}

func TestSimpleRoutesByScope(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)
	h.start(t)

	// A direct message from david reaches david's unit: the reply lands in his
	// chat and the completion walked his tier chain, not the household's.
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "hello", MessageID: 1})
	waitFor(t, "direct reply", func() bool { return len(h.fake.Sent()) >= 1 })

	// A group message reaches the group unit: reply in the group chat, quoting
	// the message, over the household's tier chain.
	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: davidTelegramID, Text: "hi all", MessageID: 7, IsGroup: true})
	waitFor(t, "group reply", func() bool { return len(h.fake.Sent()) >= 2 })

	var direct, group *transport.Outbound
	for _, o := range h.fake.Sent() {
		o := o
		switch o.ChatID {
		case davidTelegramID:
			direct = &o
		case groupChatID:
			group = &o
		}
	}
	if direct == nil || direct.Text != "via:local" {
		t.Fatalf("direct reply = %+v, want text via:local in chat %d", direct, davidTelegramID)
	}
	if group == nil || group.Text != "via:cloud" {
		t.Fatalf("group reply = %+v, want text via:cloud in chat %d", group, groupChatID)
	}
	if group.ReplyTo != 7 {
		t.Fatalf("group reply quotes message %d, want 7", group.ReplyTo)
	}

	// Retrieval never touched the unenrolled member's private space, and the
	// group turn read the shared space only.
	sawShared := 0
	for _, sp := range h.mem.searchedSpaces() {
		switch sp {
		case "ana-private":
			t.Fatal("retrieval touched an unenrolled member's private space")
		case "household":
			sawShared++
		case "david-private":
		default:
			t.Fatalf("retrieval touched unexpected space %q", sp)
		}
	}
	// Not a count: a turn issues one search per query term, so how many searches
	// the shared space saw is a function of what the members typed. That it saw
	// any is the invariant — both turns can read it, neither can read ana's.
	if sawShared == 0 {
		t.Fatal("no turn searched the shared space")
	}

	h.stop(t)
}

func TestSimpleUnenrolledMemberHasNoUnit(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)
	h.start(t)

	hs := mustHealth(t, h.sup)
	ana, ok := hs["ana"]
	if !ok {
		t.Fatal("unenrolled member missing from Health")
	}
	if ana.State != StateNotEnrolled {
		t.Fatalf("unenrolled member state = %v, want StateNotEnrolled — a known situation, never a failure", ana.State)
	}
	if ana.Err != nil {
		t.Fatalf("unenrolled member err = %v, want nil: nothing has gone wrong", ana.Err)
	}
	if ana.Healthy() {
		t.Fatal("unenrolled member reported healthy")
	}
	if got := ana.State.String(); got != "awaiting enrolment" {
		t.Fatalf("doctor's rendering = %q, want %q", got, "awaiting enrolment")
	}

	// A message from an unknown account gets nothing at all — no unit, no reply.
	h.fake.Inject(transport.Inbound{ChatID: 999, UserID: 999, Text: "hello?", MessageID: 1})
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.Sent()); n != 0 {
		t.Fatalf("stranger got %d replies, want silence", n)
	}

	h.stop(t)

	// Still not enrolled after the drain; the record never pretends it stopped.
	hs = mustHealth(t, h.sup)
	if got := hs["ana"]; got.State != StateNotEnrolled || got.Err != nil {
		t.Fatalf("after Stop, unenrolled member = %+v, want still awaiting enrolment", got)
	}
}

func TestSimpleStopDrainsInFlightTurnThenLocks(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) {})
	h.router.gate = gate
	h.router.entered = entered

	var sentAtLockAll int
	var lockMu sync.Mutex
	h.sessions.onLockAll = func() {
		lockMu.Lock()
		sentAtLockAll = len(h.fake.Sent())
		lockMu.Unlock()
	}

	h.start(t)

	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "slow one", MessageID: 3})
	<-entered // the turn is now in flight, held open by the router gate

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopDone <- h.sup.Stop(ctx)
	}()

	// Stop must wait for the turn, not race past it.
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned %v while a turn was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Health stays answerable mid-drain and never blocks on the stuck turn.
	if _, err := h.sup.Health(context.Background()); err != nil {
		t.Fatalf("Health during drain: %v", err)
	}

	close(gate) // let the turn finish
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The in-flight turn's reply was delivered, and sessions were locked only
	// after it was.
	sent := h.fake.Sent()
	if len(sent) != 1 || sent[0].Text != "via:local" {
		t.Fatalf("in-flight turn's reply = %+v, want one via:local", sent)
	}
	if h.sessions.lockAllCount() != 1 {
		t.Fatalf("LockAll called %d times, want 1", h.sessions.lockAllCount())
	}
	lockMu.Lock()
	got := sentAtLockAll
	lockMu.Unlock()
	if got != 1 {
		t.Fatalf("LockAll ran with %d replies delivered, want 1 (after the turn)", got)
	}

	select {
	case err := <-h.startErr:
		if err != nil {
			t.Fatalf("Start returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	h.cancel()
	_ = h.fake.Close()
}

func TestSimpleHealthBeforeStartAndAfterStop(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)

	hs := mustHealth(t, h.sup)
	if len(hs) != 3 {
		t.Fatalf("Health before Start reported %d units, want 3", len(hs))
	}
	if hs["david"].State != StateUnknown || hs["group"].State != StateUnknown {
		t.Fatalf("units before Start: david=%v group=%v, want unknown", hs["david"].State, hs["group"].State)
	}
	if hs["ana"].State != StateNotEnrolled {
		t.Fatalf("ana before Start = %v, want awaiting enrolment", hs["ana"].State)
	}

	h.start(t)
	h.stop(t)

	hs = mustHealth(t, h.sup)
	if hs["david"].State != StateStopped || hs["group"].State != StateStopped {
		t.Fatalf("after Stop: david=%v group=%v, want stopped", hs["david"].State, hs["group"].State)
	}
	if hs["david"].Healthy() {
		t.Fatal("stopped unit reported healthy")
	}

	// Stop is idempotent.
	if err := h.sup.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestSimpleRestartsUnitGoroutineAfterPanic(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)
	h.router.panicOnce = true
	h.start(t)

	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "boom", MessageID: 1})
	waitFor(t, "unit restarted", func() bool {
		hs := mustHealth(t, h.sup)
		return hs["david"].Restarts == 1 && hs["david"].State == StateReady
	})

	hs := mustHealth(t, h.sup)
	if hs["david"].Err == nil || !strings.Contains(hs["david"].Err.Error(), "panicked") {
		t.Fatalf("restarted unit's Err = %v, want the retained panic", hs["david"].Err)
	}

	// The restarted goroutine serves the next message as if nothing happened.
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "again", MessageID: 2})
	waitFor(t, "reply after restart", func() bool { return len(h.fake.Sent()) >= 1 })
	if got, _ := h.fake.LastSent(); got.Text != "via:local" {
		t.Fatalf("reply after restart = %+v", got)
	}

	h.stop(t)
}

// testBinder enrols anyone the Claimer accepts, handing back a member shaped like
// the configuration's ana row.
type testBinder struct{}

func (testBinder) Bind(_ context.Context, id domain.MemberID, name string, telegramID int64, at time.Time) (domain.Member, error) {
	return domain.Member{
		ID: id, Name: name, TelegramID: telegramID,
		Private: "ana-private", Tiers: []string{"local"}, EnrolledAt: at,
	}, nil
}

func (testBinder) Unbind(_ context.Context, id domain.MemberID) (domain.Member, error) {
	return domain.Member{ID: id}, nil
}

func TestSimpleEnrolmentMintsUnitMidRun(t *testing.T) {
	claimer, err := enrol.New(enrol.NewMemStore(), testBinder{})
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	code, err := claimer.Mint(context.Background(), "Ana", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	anaID := enrol.MemberIDFor("Ana")

	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) { o.Enrol = claimer })
	h.sessions.unlock(anaID)
	h.start(t)

	const anaTelegramID = int64(333)

	// Junk from a stranger earns silence.
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: "hello?", MessageID: 1})
	time.Sleep(30 * time.Millisecond)
	if n := len(h.fake.Sent()); n != 0 {
		t.Fatalf("stranger without a code got %d replies, want silence", n)
	}

	// The claim code enrols her: onboarding arrives and a unit spins up.
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: code, MessageID: 2})
	waitFor(t, "onboarding sent", func() bool { return len(h.fake.Sent()) >= 1 })
	waitFor(t, "ana's unit ready", func() bool {
		return mustHealth(t, h.sup)[string(anaID)].State == StateReady
	})

	// Her next message is served by her own unit, over her own tier chain.
	before := len(h.fake.Sent())
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: "hi", MessageID: 3})
	waitFor(t, "ana's first turn", func() bool {
		for _, o := range h.fake.Sent()[before:] {
			if o.Text == "via:local" && o.ChatID == anaTelegramID {
				return true
			}
		}
		return false
	})

	h.stop(t)
}

// panicBinder blows up inside the enrolment pump — outside any turn, where the
// recover around a unit's Handle cannot help.
type panicBinder struct {
	mu     sync.Mutex
	called bool
}

func (b *panicBinder) Bind(context.Context, domain.MemberID, string, int64, time.Time) (domain.Member, error) {
	b.mu.Lock()
	b.called = true
	b.mu.Unlock()
	panic("panicBinder: scripted panic")
}

func (b *panicBinder) Unbind(_ context.Context, id domain.MemberID) (domain.Member, error) {
	return domain.Member{ID: id}, nil
}

func (b *panicBinder) wasCalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.called
}

// TestEnrolmentPumpPanicLeavesTheHouseholdServing.
//
// Panic containment used to cover the turn handler alone. A panic raised in the
// enrolment pump — one claim, one broken dependency — unwound through the
// goroutine and took the process with it, and with it every other member's
// assistant. One member's trouble is never the household's outage, and that
// applies to the pumps as much as to the turns they dispatch.
func TestEnrolmentPumpPanicLeavesTheHouseholdServing(t *testing.T) {
	binder := &panicBinder{}
	claimer, err := enrol.New(enrol.NewMemStore(), binder)
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	code, err := claimer.Mint(context.Background(), "Ana", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) { o.Enrol = claimer })
	h.start(t)

	// A valid code from a stranger reaches the binder, which panics. Without
	// containment the test binary dies right here.
	const anaTelegramID = int64(333)
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: code, MessageID: 1})
	waitFor(t, "the enrolment pump to panic", binder.wasCalled)

	// David's assistant is untouched by someone else's failed claim.
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "still there?", MessageID: 2})
	waitFor(t, "david's reply", func() bool {
		for _, o := range h.fake.Sent() {
			if o.ChatID == davidTelegramID && o.Text == "via:local" {
				return true
			}
		}
		return false
	})
	if hs := mustHealth(t, h.sup); !hs["david"].Healthy() || !hs["group"].Healthy() {
		t.Fatalf("units after an enrolment panic: david=%v group=%v, want both serving", hs["david"].State, hs["group"].State)
	}

	h.stop(t)
}

// testClock is a hand-wound clock, so a test can put a unit's uptime well past
// HealthyReset without waiting for it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestUnitBackoffReturnsToBaseAfterRecovery.
//
// The per-unit backoff only ever doubled. A unit that panicked once months ago
// and has served perfectly since would wait the maximum delay before its next
// restart, punishing it for ancient history. Isolated mode resets a pod's
// schedule once it has stayed up for HealthyReset; an in-process unit gets the
// same treatment, measured from its last panic.
func TestUnitBackoffReturnsToBaseAfterRecovery(t *testing.T) {
	const base = 2 * time.Millisecond
	clock := newTestClock()
	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) {
		o.Now = clock.now
		o.RestartBackoff = base
		o.MaxRestartBackoff = 64 * time.Millisecond
	})

	var mu sync.Mutex
	var delays []time.Duration
	h.sup.run.testHookBackoff = func(_ unitKey, d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}

	panicOnNextTurn := func() {
		h.router.mu.Lock()
		h.router.panicOnce = true
		h.router.mu.Unlock()
	}
	restarted := func(n int) func() bool {
		return func() bool {
			d := mustHealth(t, h.sup)["david"]
			return d.Restarts == n && d.State == StateReady
		}
	}

	panicOnNextTurn()
	h.start(t)
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "boom", MessageID: 1})
	waitFor(t, "the first restart", restarted(1))

	// The unit then serves for far longer than HealthyReset before the next
	// unrelated panic.
	clock.advance(2 * DefaultHealthyReset)
	panicOnNextTurn()
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "boom again", MessageID: 2})
	waitFor(t, "the second restart", restarted(2))

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("restart delays = %v, want two", got)
	}
	if got[0] != base || got[1] != base {
		t.Fatalf("restart delays = %v, want both at the base %v: a recovered unit starts its schedule over", got, base)
	}

	h.stop(t)
}

// TestDrainAfterAWedgedPumpDoesNotWaitOnTurns.
//
// On the drain-timeout branch the pumps have not been seen to exit, so one of
// them may still be between receiving a message and dispatching its turn. The
// drain used to go on and Wait on the turn WaitGroup anyway, which is the one
// thing a WaitGroup forbids: an Add racing a Wait. With the pumps unaccounted
// for there is no closed set to wait on, and the drain has already been
// reported unclean, so it must not wait at all.
func TestDrainAfterAWedgedPumpDoesNotWaitOnTurns(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)
	r := h.sup.run
	r.rc.cancelGrace = 20 * time.Millisecond

	// A pump that never exits, holding a turn it dispatched: allDone never
	// closes and turnWg never comes back to zero.
	r.mu.Lock()
	r.started = true
	r.launched = true
	r.active = 1
	r.mu.Unlock()
	r.turnWg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := r.shutdown(ctx); err == nil {
		t.Fatal("a drain that ran out of patience reported itself clean")
	}
	if n := waitersInsideShutdown(); n != 0 {
		t.Fatalf("%d goroutines left waiting on the turn WaitGroup; a pump that never exited can still Add to it", n)
	}
	_ = h.fake.Close()
}

// waitersInsideShutdown counts goroutines parked in a WaitGroup inside shutdown.
func waitersInsideShutdown() int {
	buf := make([]byte, 1<<19)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "sync.(*WaitGroup).Wait") && strings.Contains(g, "(*runner).shutdown") {
			count++
		}
	}
	return count
}

func TestSimpleTransportDeathIsFatal(t *testing.T) {
	h := newSimpleHarness(t, simpleTestConfig(), nil)
	h.start(t)

	// The bot's stream ending is the one failure this mode cannot ride out.
	_ = h.fake.Close()
	select {
	case err := <-h.startErr:
		if err == nil || !strings.Contains(err.Error(), "update stream ended") {
			t.Fatalf("Start returned %v, want update-stream-ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after the transport died")
	}
	h.cancel()
}

func TestSimpleNoGoroutineLeaksAfterStop(t *testing.T) {
	before := runtime.NumGoroutine()

	h := newSimpleHarness(t, simpleTestConfig(), nil)
	h.start(t)
	h.fake.Inject(transport.Inbound{ChatID: davidTelegramID, UserID: davidTelegramID, Text: "hello", MessageID: 1})
	waitFor(t, "reply", func() bool { return len(h.fake.Sent()) >= 1 })
	h.stop(t)

	waitFor(t, "goroutines to drain", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}

func TestUnitRefusesStrangerWithoutMux(t *testing.T) {
	// The Mux is an optimisation, not the gate: a message handed straight to a
	// unit, bypassing every view, must still be refused by scope resolution.
	// This is the invariant that survives someone later changing Mux matching.
	h := newSimpleHarness(t, simpleTestConfig(), nil)

	if len(h.sup.run.pending) == 0 {
		t.Fatal("no constructed units to test against")
	}
	for _, pu := range h.sup.run.pending {
		err := pu.unit.Handle(context.Background(), transport.Inbound{
			ChatID: 999, UserID: 999, Text: "let me in", MessageID: 1,
		})
		if !errors.Is(err, scope.ErrNotEnrolled) {
			t.Fatalf("unit %+v answered a stranger with %v, want scope.ErrNotEnrolled", pu.key, err)
		}
	}
	// Refused in silence: resolution failed before anything could be sent.
	if n := len(h.fake.Sent()); n != 0 {
		t.Fatalf("stranger received %d messages from a direct Handle call, want none", n)
	}

	// The same delivery from an enrolled member is served — proving the direct
	// path exercised above is the real serving path, not a stub.
	err := h.sup.run.pending[0].unit.Handle(context.Background(), transport.Inbound{
		ChatID: davidTelegramID, UserID: davidTelegramID, Text: "hello", MessageID: 2,
	})
	if err != nil {
		t.Fatalf("enrolled member's direct Handle: %v", err)
	}
	if got, ok := h.fake.LastSent(); !ok || got.Text != "via:local" {
		t.Fatalf("enrolled member's direct Handle sent %+v, want via:local", got)
	}

	_ = h.sup.Stop(context.Background())
	_ = h.fake.Close()
}

func TestPerUnitContextBudgetFromTierChain(t *testing.T) {
	windows := map[string]int{"local": 8192, "cloud": 200000}

	// The derivation itself: minimum across the chain, unknown tiers do not
	// constrain, empty derivation falls back to the assistant's default.
	cases := []struct {
		tiers []string
		want  int
	}{
		{[]string{"local"}, 8192},
		{[]string{"cloud"}, 200000},
		{[]string{"local", "cloud"}, 8192},
		{[]string{"cloud", "local"}, 8192},
		{[]string{"mystery"}, 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := minWindow(windows, c.tiers); got != c.want {
			t.Fatalf("minWindow(%v) = %d, want %d", c.tiers, got, c.want)
		}
	}

	// Wired through: david's local-only chain and the household's cloud chain
	// resolve to different budgets in the same supervisor — the budget is per
	// scope, never per household.
	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) {
		o.TierWindows = windows
	})
	member := h.sup.run.unitOptions([]string{"local"})
	group := h.sup.run.unitOptions([]string{"cloud"})
	if member.ContextBudget != 8192 || group.ContextBudget != 200000 {
		t.Fatalf("budgets member=%d group=%d, want 8192 and 200000",
			member.ContextBudget, group.ContextBudget)
	}

	// An explicit household-wide budget still wins when the operator sets one.
	h2 := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) {
		o.TierWindows = windows
		o.Unit.ContextBudget = 4096
	})
	if got := h2.sup.run.unitOptions([]string{"cloud"}).ContextBudget; got != 4096 {
		t.Fatalf("explicit budget = %d, want 4096", got)
	}

	_ = h.sup.Stop(context.Background())
	_ = h2.sup.Stop(context.Background())
	_ = h.fake.Close()
	_ = h2.fake.Close()
}

func TestSimpleBotTokenResolvedThroughSecrets(t *testing.T) {
	// The configuration states a token file and no environment variable. With
	// no injected transport, construction must fail inside token resolution —
	// proving the token goes through config's Secrets API, where file, env and
	// credential precedence lives, rather than through a raw environment read
	// that would leave the shipped systemd unit (LoadCredential=, no
	// EnvironmentFile=) unable to start.
	cfg := simpleTestConfig()
	cfg.Telegram = config.TelegramConfig{BotTokenFile: "/etc/kenward/bot-token"}
	secrets := config.NewSecrets(config.SecretOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		FS:        fakeSecretFS{},
	})
	_, err := NewSimple(cfg, SimpleOptions{
		Memory: &fakeMemory{}, Router: &fakeRouter{}, Sessions: newFakeSessions(),
		Secrets: secrets,
	})
	if err == nil || !strings.Contains(err.Error(), "resolving bot token") {
		t.Fatalf("NewSimple = %v, want a bot-token resolution failure through Secrets", err)
	}
}

func TestSingleBotTokenResolvedThroughSecrets(t *testing.T) {
	// Same property for the pod-side runtime: the member's token resolves
	// through Secrets, so a pod can hold a 0600 file or credential and no
	// environment variable at all.
	secrets := config.NewSecrets(config.SecretOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		FS:        fakeSecretFS{},
	})
	_, err := NewSingle(singleTestConfig(), SingleOptions{
		Member: "david",
		Memory: &fakeMemory{}, Router: &fakeRouter{}, Sessions: newFakeSessions(),
		Secrets: secrets,
	})
	if err == nil || !strings.Contains(err.Error(), "resolving bot token") {
		t.Fatalf("NewSingle = %v, want a bot-token resolution failure through Secrets", err)
	}
}

func TestSimpleConstructionErrors(t *testing.T) {
	// Wrong mode is refused: the supervisor is the only place that knows the
	// mode, and it must not be constructible against the wrong one.
	cfg := simpleTestConfig()
	cfg.Mode = config.ModeIsolated
	if _, err := NewSimple(cfg, SimpleOptions{
		Transport: transport.NewFake(), Memory: &fakeMemory{},
		Router: &fakeRouter{}, Sessions: newFakeSessions(),
	}); err == nil {
		t.Fatal("NewSimple accepted an isolated-mode configuration")
	}

	// Nothing to run and no Claimer to change that.
	empty := simpleTestConfig()
	empty.Members = nil
	empty.Household.GroupChatID = 0
	_, err := NewSimple(empty, SimpleOptions{
		Transport: transport.NewFake(), Memory: &fakeMemory{},
		Router: &fakeRouter{}, Sessions: newFakeSessions(),
	})
	if !errors.Is(err, ErrNoUnits) {
		t.Fatalf("NewSimple = %v, want ErrNoUnits", err)
	}
}
