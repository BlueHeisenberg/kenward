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
	if sawShared != 2 {
		t.Fatalf("shared space searched %d times, want 2 (one per turn)", sawShared)
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
	if ana.State != StateUnknown || ana.State == StateFailed {
		t.Fatalf("unenrolled member state = %v, want unknown and never failed", ana.State)
	}
	if !errors.Is(ana.Err, ErrNotEnrolled) {
		t.Fatalf("unenrolled member err = %v, want ErrNotEnrolled", ana.Err)
	}
	if ana.Healthy() {
		t.Fatal("unenrolled member reported healthy")
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
	if got := hs["ana"]; got.State != StateUnknown || !errors.Is(got.Err, ErrNotEnrolled) {
		t.Fatalf("after Stop, unenrolled member = %+v, want unknown/not-enrolled", got)
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
	for name, u := range hs {
		if u.State != StateUnknown {
			t.Fatalf("unit %s before Start = %v, want unknown", name, u.State)
		}
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
