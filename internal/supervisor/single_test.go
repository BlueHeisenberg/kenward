package supervisor

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// singleTestConfig is the isolated household seen from inside one pod: the full
// member list is visible, but only one unit will run here.
func singleTestConfig() *config.Config {
	return isolatedTestConfig()
}

// singleHarness wires a Single over fakes and runs Start in the background.
type singleHarness struct {
	sup      *Single
	fake     *transport.Fake
	mem      *fakeMemory
	router   *fakeRouter
	sessions *fakeSessions
	cancel   context.CancelFunc
	startErr chan error
}

func newSingleHarness(t *testing.T, cfg *config.Config, mutate func(*SingleOptions)) *singleHarness {
	t.Helper()
	h := &singleHarness{
		fake:     transport.NewFake(),
		mem:      &fakeMemory{},
		router:   &fakeRouter{},
		sessions: newFakeSessions("david"),
		startErr: make(chan error, 1),
	}
	opts := SingleOptions{
		Member:    "david",
		Transport: h.fake,
		Memory:    h.mem,
		Router:    h.router,
		Sessions:  h.sessions,
		// In production this closes over the pod's own member's passphrase, so
		// what the fake must model is that calling it puts a key in this pod.
		// Whether it is ever called for anybody but this pod's member is the
		// thing the mode's whole claim rests on; fakeSessions records it and
		// TestSingleClaimOnlyPodServesAfterClaim checks it.
		UnlockOnEnrol: func(_ context.Context, m domain.Member) error {
			h.sessions.unlock(m.ID)
			return nil
		},
		RestartBackoff: time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	sup, err := NewSingle(cfg, opts)
	if err != nil {
		t.Fatalf("NewSingle: %v", err)
	}
	h.sup = sup
	return h
}

func (h *singleHarness) start(t *testing.T, unit string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.startErr <- h.sup.Start(ctx) }()
	waitFor(t, "unit ready", func() bool {
		return mustHealth(t, h.sup)[unit].State == StateReady
	})
}

func (h *singleHarness) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Stop(ctx); err != nil {
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

func TestSingleServesExactlyOneMember(t *testing.T) {
	h := newSingleHarness(t, singleTestConfig(), nil)
	h.start(t, "david")

	// Exactly one unit exists, and it is david's.
	hs, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(hs) != 1 || string(hs[0].Member) != "david" || hs[0].Group {
		t.Fatalf("Health = %+v, want exactly david's unit", hs)
	}

	// David's message is served over his own tier chain.
	h.fake.Inject(transport.Inbound{ChatID: 1, UserID: 1, Text: "hello", MessageID: 1})
	waitFor(t, "david's reply", func() bool { return len(h.fake.Sent()) >= 1 })
	if got, _ := h.fake.LastSent(); got.Text != "via:local" || got.ChatID != 1 {
		t.Fatalf("reply = %+v, want via:local in chat 1", got)
	}

	// Another member of the same household is not served here — their pod is
	// elsewhere, and this process answers with nothing at all.
	h.fake.Inject(transport.Inbound{ChatID: 2, UserID: 2, Text: "eve here", MessageID: 2})
	// A group message is not served either: the group has its own pod.
	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: 1, Text: "hi all", MessageID: 3, IsGroup: true})
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.Sent()); n != 1 {
		t.Fatalf("other units' messages produced %d extra replies, want none", n-1)
	}

	h.stop(t)
}

func TestSingleServesTheGroup(t *testing.T) {
	h := newSingleHarness(t, singleTestConfig(), func(o *SingleOptions) {
		o.Member = ""
		o.Group = true
	})
	h.start(t, "group")

	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: 1, Text: "hi all", MessageID: 9, IsGroup: true})
	waitFor(t, "group reply", func() bool { return len(h.fake.Sent()) >= 1 })
	got, _ := h.fake.LastSent()
	if got.ChatID != groupChatID || got.ReplyTo != 9 || got.Text != "via:local" {
		t.Fatalf("group reply = %+v", got)
	}

	// A direct message to the group's bot is nobody's conversation here.
	h.fake.Inject(transport.Inbound{ChatID: 1, UserID: 1, Text: "psst", MessageID: 10})
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.Sent()); n != 1 {
		t.Fatalf("direct message to the group pod produced %d extra replies, want none", n-1)
	}

	// The group turn read the shared space and nothing else.
	for _, sp := range h.mem.searchedSpaces() {
		if sp != "household" {
			t.Fatalf("group pod searched space %q, want household only", sp)
		}
	}

	h.stop(t)
}

func TestSingleSelectionErrors(t *testing.T) {
	deps := func(o *SingleOptions) {
		o.Transport = transport.NewFake()
		o.Memory = &fakeMemory{}
		o.Router = &fakeRouter{}
		o.Sessions = newFakeSessions()
	}
	newWith := func(cfg *config.Config, mutate func(*SingleOptions)) error {
		opts := SingleOptions{}
		deps(&opts)
		mutate(&opts)
		_, err := NewSingle(cfg, opts)
		return err
	}

	// An unenrolled member without a claimer is an error: such a pod could
	// neither serve nor enrol anyone, and starting it would only look healthy.
	// With a claimer it starts claim-only — TestSingleClaimOnlyPod covers that.
	err := newWith(singleTestConfig(), func(o *SingleOptions) { o.Member = "ana" })
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("unenrolled member without claimer = %v, want ErrNotEnrolled", err)
	}

	// A member the household does not have.
	if err := newWith(singleTestConfig(), func(o *SingleOptions) { o.Member = "nobody" }); err == nil {
		t.Fatal("NewSingle accepted an unknown member")
	}

	// Neither and both selections.
	if err := newWith(singleTestConfig(), func(o *SingleOptions) {}); err == nil {
		t.Fatal("NewSingle accepted an empty selection")
	}
	if err := newWith(singleTestConfig(), func(o *SingleOptions) { o.Member = "david"; o.Group = true }); err == nil {
		t.Fatal("NewSingle accepted both a member and the group")
	}

	// The group without a configured group chat.
	noGroup := singleTestConfig()
	noGroup.Household.GroupChatID = 0
	if err := newWith(noGroup, func(o *SingleOptions) { o.Group = true }); err == nil {
		t.Fatal("NewSingle accepted the group with no group chat configured")
	}

	// Simple mode has no single-unit processes.
	simple := singleTestConfig()
	simple.Mode = config.ModeSimple
	if err := newWith(simple, func(o *SingleOptions) { o.Member = "david" }); err == nil {
		t.Fatal("NewSingle accepted a simple-mode configuration")
	}
}

func TestSingleClaimOnlyPodServesAfterClaim(t *testing.T) {
	// D-023: a member's bot exists before they claim, and the claim happens in
	// a conversation with that bot. So an unenrolled member's pod starts
	// claim-only — no unit, nothing served — and the successful claim mints
	// their unit in place, without a restart.
	claimer, err := enrol.New(enrol.NewMemStore(), testBinder{})
	if err != nil {
		t.Fatalf("enrol.New: %v", err)
	}
	code, err := claimer.Mint(context.Background(), "Ana", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	anaID := enrol.MemberIDFor("Ana")
	if anaID != "ana" {
		t.Fatalf("MemberIDFor(Ana) = %q; the test config expects %q", anaID, "ana")
	}

	h := newSingleHarness(t, singleTestConfig(), func(o *SingleOptions) {
		o.Member = "ana"
		o.Enrol = claimer
	})
	// Nothing is unlocked up front: ana has no key until she claims, because
	// there is nothing to provision for somebody who may never arrive. The claim
	// path is what gives her one — see UnlockOnEnrol.

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.startErr <- h.sup.Start(ctx) }()

	// The claim-only window: the pod is up, the member is awaiting enrolment,
	// and nothing is served.
	waitFor(t, "claim-only state", func() bool {
		return mustHealth(t, h.sup)["ana"].State == StateNotEnrolled
	})
	const anaTelegramID = int64(333)
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: "hello?", MessageID: 1})
	time.Sleep(30 * time.Millisecond)
	if n := len(h.fake.Sent()); n != 0 {
		t.Fatalf("claim-only pod sent %d messages to a codeless sender, want silence", n)
	}

	// The claim mints the unit in place: onboarding, then service, no restart.
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: code, MessageID: 2})
	waitFor(t, "onboarding sent", func() bool { return len(h.fake.Sent()) >= 1 })
	waitFor(t, "ana's unit ready", func() bool {
		return mustHealth(t, h.sup)["ana"].State == StateReady
	})
	before := len(h.fake.Sent())
	h.fake.Inject(transport.Inbound{ChatID: anaTelegramID, UserID: anaTelegramID, Text: "hi", MessageID: 3})
	waitFor(t, "ana's first turn", func() bool { return len(h.fake.Sent()) > before })
	if got, _ := h.fake.LastSent(); got.Text != "via:local" || got.ChatID != anaTelegramID {
		t.Fatalf("ana's first private message after claiming was answered with %q; "+
			"the pod reported itself serving, so it must serve", got.Text)
	}

	// A different member's code landing on this bot binds them, but must never
	// mint their unit in ana's address space — nor, more seriously, a key. Their
	// pod holds their key under their own passphrase; one provisioned here would
	// be wrapped under ana's, which is precisely the isolation this mode exists
	// to provide.
	bobCode, err := claimer.Mint(context.Background(), "Bob", time.Hour)
	if err != nil {
		t.Fatalf("Mint bob: %v", err)
	}
	h.fake.Inject(transport.Inbound{ChatID: 444, UserID: 444, Text: bobCode, MessageID: 4})
	waitFor(t, "bob's onboarding", func() bool {
		for _, o := range h.fake.Sent() {
			if o.ChatID == 444 {
				return true
			}
		}
		return false
	})
	if _, ok := h.sessions.Key(enrol.MemberIDFor("Bob")); ok {
		t.Fatal("a foreign member's claim provisioned their key in ana's pod, wrapped under ana's passphrase")
	}
	hs, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, u := range hs {
		if u.Member == enrol.MemberIDFor("Bob") {
			t.Fatal("a foreign member's claim minted a unit in this pod")
		}
	}
	if len(hs) != 1 {
		t.Fatalf("Health reports %d units, want ana's alone", len(hs))
	}

	h.stop(t)
}

func TestSingleStopDrainsInFlightTurnThenLocks(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := newSingleHarness(t, singleTestConfig(), nil)
	h.router.gate = gate
	h.router.entered = entered

	var sentAtLockAll int
	h.sessions.onLockAll = func() { sentAtLockAll = len(h.fake.Sent()) }

	h.start(t, "david")
	h.fake.Inject(transport.Inbound{ChatID: 1, UserID: 1, Text: "slow", MessageID: 1})
	<-entered

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopDone <- h.sup.Stop(ctx)
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned %v while a turn was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := h.sup.Health(context.Background()); err != nil {
		t.Fatalf("Health during drain: %v", err)
	}

	close(gate)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sent := h.fake.Sent()
	if len(sent) != 1 || sent[0].Text != "via:local" {
		t.Fatalf("in-flight turn's reply = %+v, want one via:local", sent)
	}
	if h.sessions.lockAllCount() != 1 || sentAtLockAll != 1 {
		t.Fatalf("LockAll count=%d with %d replies delivered, want 1 and 1",
			h.sessions.lockAllCount(), sentAtLockAll)
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

func TestSingleHealthBeforeStartAndAfterStop(t *testing.T) {
	h := newSingleHarness(t, singleTestConfig(), nil)

	hs := mustHealth(t, h.sup)
	if len(hs) != 1 || hs["david"].State != StateUnknown {
		t.Fatalf("Health before Start = %+v, want one unknown unit", hs)
	}

	h.start(t, "david")
	h.stop(t)

	hs = mustHealth(t, h.sup)
	if hs["david"].State != StateStopped {
		t.Fatalf("after Stop = %v, want stopped", hs["david"].State)
	}
	if err := h.sup.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestSingleNoGoroutineLeaksAfterStop(t *testing.T) {
	before := runtime.NumGoroutine()

	h := newSingleHarness(t, singleTestConfig(), nil)
	h.start(t, "david")
	h.fake.Inject(transport.Inbound{ChatID: 1, UserID: 1, Text: "hello", MessageID: 1})
	waitFor(t, "reply", func() bool { return len(h.fake.Sent()) >= 1 })
	h.stop(t)

	waitFor(t, "goroutines to drain", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}
