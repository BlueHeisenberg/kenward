package supervisor

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
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
		Transport: h.fake,
		Memory:    h.mem,
		Router:    h.router,
		Sessions:  h.sessions,
		// Shaped like production: nothing pre-unlocks a member who has not
		// claimed yet, and the key arrives on the claim path. A harness that
		// unlocked everybody up front would pass whether or not the claim ever
		// provisioned anything, which is how the mid-run lock went unnoticed.
		UnlockOnEnrol: func(_ context.Context, m domain.Member) error {
			h.sessions.unlock(m.ID)
			return nil
		},
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
	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: davidTelegramID, Text: "hi all", MessageID: 7, IsGroup: true, Addressed: true})
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
	if direct == nil || replyBody(direct.Text) != "via:local" {
		t.Fatalf("direct reply = %+v, want text via:local in chat %d", direct, davidTelegramID)
	}
	if group == nil || replyBody(group.Text) != "via:cloud" {
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
	if len(sent) != 1 || replyBody(sent[0].Text) != "via:local" {
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
	if got, _ := h.fake.LastSent(); replyBody(got.Text) != "via:local" {
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

// TestSimpleEnrolmentMintsUnitMidRun.
//
// The mid-run claim has to deliver a working assistant, not the shape of one. This
// test used to unlock ana's key by hand before starting, which quietly hid the
// defect it should have caught: keys were provisioned and unlocked at startup only,
// so a member who claimed while the node ran got their unit, their onboarding, and
// then "Your assistant is locked" on their first private message — until an operator
// restarted the node. Onboarding is the first thing a household ever does, and the
// remedy named a person who may not be the one holding the phone.
//
// So the harness no longer pre-unlocks anybody. The key arrives the way production
// delivers it: UnlockOnEnrol, called by the runner on the claim path.
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

	// Her next message is served by her own unit, over her own tier chain — and
	// answered, not refused. Anything else here is the reply a real member reads
	// as their first private word from the assistant, so the failure prints it.
	before := len(h.fake.Sent())
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: "hi", MessageID: 3})
	waitFor(t, "ana's first turn", func() bool { return len(h.fake.Sent()) > before })
	got, _ := h.fake.LastSent()
	if replyBody(got.Text) != "via:local" || got.ChatID != anaTelegramID {
		t.Fatalf("ana's first private message after claiming was answered with %q; "+
			"a member who has just enrolled must get a real answer, not a notice whose only remedy is an operator restart", got.Text)
	}

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
			if o.ChatID == davidTelegramID && replyBody(o.Text) == "via:local" {
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
	if got, ok := h.fake.LastSent(); !ok || replyBody(got.Text) != "via:local" {
		t.Fatalf("enrolled member's direct Handle sent %+v, want via:local", got)
	}

	_ = h.sup.Stop(context.Background())
	_ = h.fake.Close()
}

// budgetTestConfig is simpleTestConfig with endpoints that state their own windows
// and caps: monster is a large local machine serving a reasoning model, mini is a
// small one on the same local tier, and the cloud tier is somewhere in between.
func budgetTestConfig() *config.Config {
	cfg := simpleTestConfig()
	cfg.Endpoints = []config.EndpointConfig{
		{
			Name: "monster", BaseURL: "http://monster:8000/v1", Model: "big",
			Tags: []string{"local"}, ContextWindow: 262144, MaxCompletionTokens: 32768,
		},
		{
			Name: "mini", BaseURL: "http://mini:11434/v1", Model: "small",
			Tags: []string{"local-slow"}, ContextWindow: 8192, MaxCompletionTokens: 2048,
		},
		{
			Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "cloudy",
			Tags: []string{"cloud"}, ContextWindow: 200000, MaxCompletionTokens: 8192,
		},
	}
	return cfg
}

func TestPerUnitContextBudgetFromTierChain(t *testing.T) {
	// The derivation: an endpoint's declared window is honoured, a chain takes the
	// minimum of every endpoint it reaches in either direction, and a chain that
	// reaches no endpoint derives nothing and leaves the assistant's default.
	cases := []struct {
		tiers            []string
		window, maxToken int
	}{
		{[]string{"local"}, 262144, 32768},
		{[]string{"cloud"}, 200000, 8192},
		{[]string{"local", "local-slow"}, 8192, 2048},
		{[]string{"local-slow", "local"}, 8192, 2048},
		{[]string{"local", "local-slow", "cloud"}, 8192, 2048},
		{[]string{"mystery"}, 0, 0},
		{nil, 0, 0},
	}
	cfg := budgetTestConfig()
	for _, c := range cases {
		w, m := cfg.ChainLimits(c.tiers)
		if w != c.window || m != c.maxToken {
			t.Fatalf("ChainLimits(%v) = (%d, %d), want (%d, %d)", c.tiers, w, m, c.window, c.maxToken)
		}
	}

	// Wired through: david's local-only chain and the household's cloud chain
	// resolve to different budgets in the same supervisor — the budget is per
	// scope, never per household.
	h := newSimpleHarness(t, budgetTestConfig(), func(o *SimpleOptions) {})
	member := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"})
	group := h.sup.run.unitOptions(unitKey{group: true}, []string{"cloud"})
	if member.ContextBudget != 262144 || group.ContextBudget != 200000 {
		t.Fatalf("budgets member=%d group=%d, want 262144 and 200000",
			member.ContextBudget, group.ContextBudget)
	}
	if member.MaxTokens != 32768 || group.MaxTokens != 8192 {
		t.Fatalf("completion caps member=%d group=%d, want 32768 and 8192",
			member.MaxTokens, group.MaxTokens)
	}

	// A chain reaching no endpoint that states anything leaves both zero, which is
	// what makes assistant.New apply its own defaults rather than a derived zero.
	if o := h.sup.run.unitOptions(unitKey{group: true}, []string{"mystery"}); o.ContextBudget != 0 || o.MaxTokens != 0 {
		t.Fatalf("undeclared chain = (%d, %d), want (0, 0) so the assistant defaults apply",
			o.ContextBudget, o.MaxTokens)
	}

	// An explicit household-wide budget still wins when the operator sets one.
	h2 := newSimpleHarness(t, budgetTestConfig(), func(o *SimpleOptions) {
		o.Unit.ContextBudget = 4096
		o.Unit.MaxTokens = 512
	})
	if o := h2.sup.run.unitOptions(unitKey{group: true}, []string{"cloud"}); o.ContextBudget != 4096 || o.MaxTokens != 512 {
		t.Fatalf("explicit budget = (%d, %d), want (4096, 512)", o.ContextBudget, o.MaxTokens)
	}

	_ = h.sup.Stop(context.Background())
	_ = h2.sup.Stop(context.Background())
	_ = h.fake.Close()
	_ = h2.fake.Close()
}

// TestDerivedBudgetNeverContradictsTheAssistant is the interaction the two checks have
// to survive. assistant.New refuses MaxTokens >= ContextBudget; the budget and the cap
// are derived independently, as two separate minima over the same chain. They cannot
// disagree — the endpoint holding the smallest window contributes a cap smaller than
// that window, by validation — and this asserts it over the shapes that would break a
// naive derivation, including the one where the largest window and the smallest cap
// come from different endpoints.
func TestDerivedBudgetNeverContradictsTheAssistant(t *testing.T) {
	cfg := budgetTestConfig()
	// A fourth endpoint whose cap is the smallest in the household but whose window
	// is the largest: min(window) and min(cap) now come from different machines.
	cfg.Endpoints = append(cfg.Endpoints, config.EndpointConfig{
		Name: "huge-but-terse", BaseURL: "http://huge:8000/v1", Model: "h",
		Tags: []string{"local", "cloud"}, ContextWindow: 1000000, MaxCompletionTokens: 256,
	})
	cfg.ApplyDefaults()
	if err := cfg.Validate(func(string) (string, bool) { return "t", true }); err != nil {
		t.Fatalf("the fixture must itself be a valid configuration: %v", err)
	}

	for _, chain := range [][]string{
		{"local"}, {"cloud"}, {"local-slow"},
		{"local", "cloud"}, {"local", "local-slow", "cloud"},
	} {
		window, maxTokens := cfg.ChainLimits(chain)
		if maxTokens >= window {
			t.Fatalf("ChainLimits(%v) = (%d, %d): the cap must stay below the budget or "+
				"assistant.New refuses to build the unit", chain, window, maxTokens)
		}
	}
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

// TestConversationResetReachesTheUnits is the wiring assertion for
// history.reset_every, and it is worth having for exactly the reason the read-line one
// is: off is the zero value of the target field, so a wiring path that assigns nothing
// at all looks perfect until a household sets the key.
//
// It also pins the scope decision. The setting is household-wide and reaches every
// unit, the group's included: how stale a conversation may get is not a privacy
// question and has nothing per-member to say.
func TestConversationResetReachesTheUnits(t *testing.T) {
	t.Run("off unless the household asks", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		if got := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).HistoryReset; got != 0 {
			t.Errorf("HistoryReset = %v, want off by default", got)
		}
	})

	t.Run("a schedule reaches every unit", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.History.ResetEvery = config.Duration(6 * time.Hour)
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		for _, tiers := range [][]string{{"local"}, {"local", "cloud"}} {
			if got, want := h.sup.run.unitOptions(unitKey{group: true}, tiers).HistoryReset, 6*time.Hour; got != want {
				t.Errorf("HistoryReset = %v for chain %v, want %v", got, tiers, want)
			}
		}
	})
}

// TestMemoryPolicyReachesTheUnits: the two new keys are not decoration in the schema.
// Both are read straight off the configuration on the wiring path, so this asserts
// them where they are actually consumed — the assistant options and the capture
// engine's own view of its policy.
//
// The default half matters as much as the set half. Both settings default to the
// behaviour the product ships with, and both defaults are the zero value of their
// target type, so a wiring bug that never assigns anything at all looks correct until
// somebody sets the other value.
func TestMemoryPolicyReachesTheUnits(t *testing.T) {
	t.Run("defaults announce reads and write private notes", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		if got := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).ReadNotices; got != assistant.ReadNoticesOn {
			t.Errorf("ReadNotices = %v, want on by default", got)
		}
		if got := cfg.Capture.PrivateWrites; got != config.PrivateWriteSave {
			t.Errorf("private_writes defaulted to %q, want %q", got, config.PrivateWriteSave)
		}
	})

	t.Run("a household can turn the read line off", func(t *testing.T) {
		cfg := budgetTestConfig()
		off := false
		cfg.Memory.AnnounceReads = &off
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		if got := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).ReadNotices; got != assistant.ReadNoticesOff {
			t.Errorf("ReadNotices = %v, want off", got)
		}
	})

	t.Run("a household can ask before private writes", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.Capture.PrivateWrites = config.PrivateWriteAsk
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		// The engine does not expose its options, so the behaviour is what is
		// asserted: a personal proposal puts a question and writes nothing until
		// it is answered. Under the default policy this same call writes.
		sc := domain.Scope{
			Kind:   domain.ScopeDirect,
			Member: &domain.Member{ID: "david", Name: "David", TelegramID: davidTelegramID, Private: "david-private"},
			Write:  "david-private",
			Read:   []domain.SpaceID{"david-private"},
			ChatID: davidTelegramID,
		}
		engine := h.sup.run.captureEngine(transport.NewFake(), "")
		engine.BeginTurn(sc, "turn-1")
		out, err := engine.Offer(context.Background(), sc, capture.Proposal{
			Draft:  memory.Draft{Domain: "household", Title: "Coffee order", Body: "Oat milk."},
			Target: capture.TargetPersonal,
		}, davidTelegramID)
		if err != nil {
			t.Fatalf("Offer: %v", err)
		}
		if out.Kind != capture.OutcomeTimedOut {
			t.Errorf("outcome = %v, want a question nobody answered; the ask policy did not reach the engine", out.Kind)
		}
	})
}

// TestGroupServesAMemberWhoseTutorialIsStillRunning is D4 from the second live run.
//
// A member claimed while the node was running. Their DM and their tutorial worked.
// Their next message in the household group got no reply and produced no log line
// at all, and the identical message was answered after a restart.
//
// The cause is not the group unit, which serves the chat and not the member: it is
// that a claim only reaches the running configuration when the tutorial ends, and
// scope.Resolve refuses a sender the configuration does not know. So for the whole
// length of somebody's setup conversation — their own doing, and bounded by
// enrol.DefaultTutorialTimeout — the household chat treats them as a stranger, and
// treating a stranger as a stranger is silent by design. The binding exists from the
// moment the code is redeemed; nothing was waiting on the tutorial but the fold.
func TestGroupServesAMemberWhoseTutorialIsStillRunning(t *testing.T) {
	claimer, err := enrol.New(enrol.NewMemStore(), testBinder{})
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	code, err := claimer.Mint(context.Background(), "Ana", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	h := newSimpleHarness(t, simpleTestConfig(), func(o *SimpleOptions) { o.Enrol = claimer })

	// The tutorial's first question is held open, so the whole test runs inside the
	// window the live member was in.
	held := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(held) }) }
	defer release()
	h.fake.SetAnswerFunc(func(transport.Question) transport.Answer {
		<-held
		return transport.Answer{TimedOut: true}
	})

	h.start(t)

	const anaTelegramID = int64(333)
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: code, MessageID: 2})
	waitFor(t, "the tutorial is asking its first question", func() bool { return len(h.fake.Asked()) >= 1 })

	before := len(h.fake.Sent())
	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: anaTelegramID, Text: "hi all", MessageID: 3, IsGroup: true, Addressed: true})
	waitFor(t, "a group reply for the member who has just claimed", func() bool {
		for _, o := range h.fake.Sent()[before:] {
			if o.ChatID == groupChatID {
				return true
			}
		}
		return false
	})

	release()
	h.stop(t)
}
