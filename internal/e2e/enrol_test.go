package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// -----------------------------------------------------------------------------
// an enrolling household
// -----------------------------------------------------------------------------

// testClock is the Claimer's clock, so a test can mint a code and then be in
// tomorrow without sleeping.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// countingStore is the real enrol.FileStore with a tally of redemption attempts.
// It changes nothing: every call is delegated.
//
// It exists because "that attempt was never processed" is invisible from outside.
// A rate-limited sender is shown exactly what a rejected one is shown — nothing —
// so the only place the difference is observable is whether the store was asked at
// all. Counting it here is how the rate limit becomes assertable without giving the
// sender something to measure.
type countingStore struct {
	enrol.Store
	mu       sync.Mutex
	consumes int
}

func (s *countingStore) Consume(ctx context.Context, digest string, now time.Time) (enrol.Code, error) {
	s.mu.Lock()
	s.consumes++
	s.mu.Unlock()
	return s.Store.Consume(ctx, digest, now)
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumes
}

// household is a harness whose supervisor also carries a real enrol.Claimer over a
// real config.Binder and a real on-disk invite store — the wiring `kenward run`
// builds, with nothing about enrolment faked.
type household struct {
	*harness
	clock   *testClock
	store   *countingStore
	claimer *enrol.Claimer
}

func newHousehold(t *testing.T, opts harnessOptions) *household {
	t.Helper()
	hh := &household{clock: newTestClock()}
	if opts.dataDir == "" {
		opts.dataDir = t.TempDir()
	}
	// The same file name `kenward invite` writes, in the same place, so a restart
	// finds the codes exactly where the running node left them.
	hh.store = &countingStore{Store: enrol.NewFileStore(filepath.Join(opts.dataDir, "invites.json"))}
	opts.enrolFor = func(t *testing.T, cfg *config.Config) *enrol.Claimer {
		t.Helper()
		// The zero Provisioning, as the binary passes: a claim may bind a member
		// the configuration declares and may never invent one.
		binder, err := config.NewBinder(cfg, config.Provisioning{})
		if err != nil {
			t.Fatalf("building the enrolment binder: %v", err)
		}
		c, err := enrol.New(hh.store, binder, enrol.WithClock(hh.clock.now))
		if err != nil {
			t.Fatalf("building the claimer: %v", err)
		}
		hh.claimer = c
		return c
	}
	hh.harness = newHarness(t, opts)
	return hh
}

// mint issues a claim code the way `kenward invite --name X` does and returns the
// plaintext, formatted exactly as the operator would read it off their terminal.
func (h *household) mint(name string, ttl time.Duration) string {
	h.t.Helper()
	code, err := h.claimer.Mint(context.Background(), name, ttl)
	if err != nil {
		h.t.Fatalf("minting a code for %s: %v", name, err)
	}
	return code
}

// waitForConsumes blocks until n redemption attempts have reached the store. It is
// the barrier for a message that is answered with silence: silence cannot be waited
// for, but the work behind it can.
func (h *household) waitForConsumes(n int) {
	h.t.Helper()
	waitFor(h.t, fmt.Sprintf("%d redemption attempt(s) to reach the store", n), func() bool {
		return h.store.count() >= n
	})
}

// waitForUnit blocks until the supervisor reports a member's unit serving, which is
// what a completed claim eventually produces and a rejected one never does.
func (h *household) waitForUnit(id domain.MemberID) {
	h.t.Helper()
	waitFor(h.t, fmt.Sprintf("%s's unit to be serving", id), func() bool {
		return memberHealth(h.t, h.harness, id).State == supervisor.StateReady
	})
}

// memberHealth reads one member's record out of the supervisor's own health report.
func memberHealth(t *testing.T, h *harness, id domain.MemberID) supervisor.UnitHealth {
	t.Helper()
	hs, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, u := range hs {
		if u.Member == id && !u.Group {
			return u
		}
	}
	t.Fatalf("no health record for member %s in %+v", id, hs)
	return supervisor.UnitHealth{}
}

// stillUnenrolled reports whether the supervisor still says this member has not
// claimed their invite.
//
// Err must be nil for it: a household half way through onboarding is healthy, and
// an operator watching for someone to accept an invitation must not be shown an
// error while they wait.
func stillUnenrolled(t *testing.T, h *harness, id domain.MemberID) bool {
	t.Helper()
	u := memberHealth(t, h, id)
	return u.State == supervisor.StateNotEnrolled && u.Err == nil
}

// wrongCode is a code-shaped string that was never minted: sixteen symbols of the
// claim alphabet, so it survives extraction and reaches the store, which is what a
// guessing attacker's traffic looks like.
func wrongCode(n int) string { return fmt.Sprintf("%016d", n) }

// providerSawCode reports whether any request that reached an inference endpoint
// carried the code, in either the printed or the normalized spelling.
func providerSawCode(p *fakeProvider, code string) bool {
	needles := []string{code, enrol.Normalize(code)}
	for _, req := range p.all() {
		for _, m := range req.Messages {
			for _, needle := range needles {
				if needle != "" && strings.Contains(m.Content, needle) {
					return true
				}
			}
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// the scenarios
// -----------------------------------------------------------------------------

// TestStrangerWithoutACodeGetsNothingAtAll is the whole of the enrolment threat
// model in one assertion. A bot username is public; anyone can send it /start. A
// reply of any kind — an error, a prompt, an acknowledgement — tells whoever found
// it that this bot is live and belongs to a real household, which is exactly the
// fact the arrangement exists to withhold.
//
// The assertion is on what the transport was asked to emit, not on a return value:
// a Claimer that answered correctly and a supervisor that sent something anyway
// would still have leaked.
func TestStrangerWithoutACodeGetsNothingAtAll(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"david", "mei"}})
	code := h.mint("David", 0)
	h.start()

	h.tr.InjectText(strangerChatID, strangerUserID, "/start", false)
	h.tr.InjectText(strangerChatID, strangerUserID, "hello? who is this?", false)
	// A valid claim behind them on the same pump. The enrolment pump is one
	// goroutine handling messages in order, so once its onboarding has been sent
	// the stranger's two messages have certainly been dealt with — which makes
	// everything below an assertion about absence rather than about timing.
	h.tr.InjectText(davidChatID, davidTelegramID, code, false)
	h.waitForReply(davidChatID, 3)

	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("stranger received %d message(s): %+v; an unknown sender gets silence", len(got), got)
	}
	// A typing indicator would be an answer too. transport.Transport has no such
	// call — Send and Ask are the only ways anything reaches a chat — so asserting
	// both are empty for this chat is the complete statement of "nothing happened".
	for _, q := range h.tr.Asked() {
		if q.ChatID == strangerChatID {
			t.Errorf("stranger was asked %q; a question is an acknowledgement", q.Text)
		}
	}
	if n := h.local.count(); n != 0 {
		t.Errorf("provider saw %d requests; a stranger's words must never reach a model", n)
	}
	if n := len(h.mem.recorded()); n != 0 {
		t.Errorf("memory saw %d calls; a stranger's message must never reach lore", n)
	}
	// One redemption reached the store: the barrier claim. Neither of the
	// stranger's messages carried anything code-shaped, so neither cost the node a
	// single hash.
	if got := h.store.count(); got != 1 {
		t.Errorf("the code store saw %d redemption attempts, want 1 (the barrier claim); "+
			"a message with nothing code-shaped in it must not reach it", got)
	}
}

// TestValidClaimBindsTheMemberAndTheNextMessageIsServed checks the claim is real
// rather than merely acknowledged. The onboarding arriving proves only that the
// Claimer said yes; the claim has actually happened when that Telegram id resolves
// to a scope and holds an ordinary conversation over the member's own spaces.
func TestValidClaimBindsTheMemberAndTheNextMessageIsServed(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"mei"}})
	code := h.mint("Mei", 0)
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The 3rd.", FinishReason: "stop"}
	})
	h.start()

	if !stillUnenrolled(t, h.harness, "mei") {
		t.Fatalf("before the claim mei's health is %+v, want a member who has not enrolled", memberHealth(t, h.harness, "mei"))
	}

	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	onboarding := h.waitForReply(meiChatID, 3)

	if len(onboarding) != 3 {
		t.Fatalf("claim produced %d messages, want the three onboarding messages: %+v", len(onboarding), onboarding)
	}
	if !strings.Contains(onboarding[0].Text, "Hello Mei") {
		t.Errorf("onboarding opens with %q, want it to greet the member by the configuration's name", onboarding[0].Text)
	}

	// The claim is only real if the next message works.
	h.waitForUnit("mei")
	h.tr.InjectText(meiChatID, meiTelegramID, "when is my appointment?", false)
	sent := h.waitForReply(meiChatID, 4)

	if got := sent[3].Text; got != "The 3rd." {
		t.Errorf("reply after enrolment = %q, want the model's text", got)
	}
	searched := h.mem.searchedSpaces()
	if len(searched) != 2 || !containsSpace(searched, meiSpace) || !containsSpace(searched, sharedSpace) {
		t.Errorf("searched %v, want exactly the newly bound member's two spaces", searched)
	}
	// A claim that bound the right Telegram id to the wrong member would look
	// identical up to here and read someone else's memory from now on.
	if containsSpace(h.mem.touchedSpaces(), davidSpace) {
		t.Errorf("mei's first turn touched %s; the claim bound her to the wrong member", davidSpace)
	}
}

// TestAClaimCodeCannotBeRedeemedTwice covers the code being single-use. A code that
// still works after it has been spent is a code that works for whoever the first
// member forwarded it to, or for anyone who read it over their shoulder.
func TestAClaimCodeCannotBeRedeemedTwice(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"mei"}})
	code := h.mint("Mei", 0)
	h.start()

	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	h.waitForReply(meiChatID, 3)

	// Somebody else presents the same code.
	h.tr.InjectText(strangerChatID, strangerUserID, code, false)
	h.waitForConsumes(2)

	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("the second redemption produced %d message(s): %+v; a spent code is refused in silence", len(got), got)
	}
	if got := len(h.sentTo(meiChatID)); got != 3 {
		t.Errorf("mei's chat has %d messages, want only her onboarding", got)
	}
	// And the refusal is not merely quiet: the second sender is nobody.
	if _, ok := h.cfg.MemberByTelegramID(strangerUserID); ok {
		t.Error("the second redemption bound a member; a consumed code binds nobody")
	}
}

// TestAnExpiredClaimCodeIsRefusedInSilence covers the expiry. A code read out over
// the phone and never used is a credential lying around; the expiry is what bounds
// how long it is one, and expiring loudly would tell the holder they had found a
// real bot with a real, merely stale, invite.
func TestAnExpiredClaimCodeIsRefusedInSilence(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"mei"}})
	code := h.mint("Mei", 10*time.Minute)
	h.start()

	h.clock.advance(11 * time.Minute)
	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	h.waitForConsumes(1)

	if got := h.sentTo(meiChatID); len(got) != 0 {
		t.Errorf("an expired code produced %d message(s): %+v; it is refused in silence", len(got), got)
	}
	if !stillUnenrolled(t, h.harness, "mei") {
		t.Errorf("mei's health after an expired code is %+v, want a member who has not enrolled",
			memberHealth(t, h.harness, "mei"))
	}
}

// TestACodeInTheGroupChatIsNotBurnedAndStillWorksInPrivate covers the mistake a
// household will actually make. Someone is handed a code and pastes it into the
// family group.
//
// Two things must both hold. The code must not enrol anyone there — everybody in
// that group has now seen it, so redeeming it would enrol whoever typed it fastest
// — and it must not be spent either. Burning it would punish the member for a typo
// with an invite they can no longer use, and the operator would have to mint
// another without ever learning why.
func TestACodeInTheGroupChatIsNotBurnedAndStillWorksInPrivate(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"mei"}})
	code := h.mint("Mei", 0)
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Noted.", FinishReason: "stop"}
	})
	h.start()

	// Mei pastes her code into the household chat.
	h.tr.InjectText(groupChatID, meiTelegramID, code, true)
	// David, who is enrolled, says something in the same chat straight after. The
	// group unit is one goroutine over one view, so his answer proves the pasted
	// code has already been through it.
	h.tr.InjectText(groupChatID, davidTelegramID, "anyone home?", true)
	h.waitForReply(groupChatID, 1)

	// The code is a credential. A group message is a conversation, and a
	// conversation is sent to an inference endpoint — so a code that resolved to a
	// group turn would have been handed to a model and to whoever runs it.
	if providerSawCode(h.local, code) {
		t.Error("a claim code reached an inference endpoint; a credential pasted in the group must never be sent anywhere")
	}
	if got := h.sentTo(groupChatID); len(got) != 1 {
		t.Errorf("group chat has %d messages, want only David's reply: %+v", len(got), got)
	}

	// The subtle half: the same code, in private, afterwards.
	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	onboarding := h.waitForReply(meiChatID, 3)
	if len(onboarding) != 3 {
		t.Fatalf("the private claim produced %d messages, want the onboarding: %+v", len(onboarding), onboarding)
	}
	// Exactly one redemption ever reached the store, and it is this one. The check
	// is here rather than straight after the group message on purpose: the
	// enrolment pump is a different goroutine from the group unit's, so David's
	// reply does not order it. Mei's onboarding does — it comes off that pump, and
	// anything the pump was going to do with the pasted code it had to do first.
	if got := h.store.count(); got != 1 {
		t.Errorf("the code store saw %d redemption attempts, want 1 (the private claim); "+
			"a code pasted in the group must not even be looked up", got)
	}
	if !strings.Contains(onboarding[0].Text, "Hello Mei") {
		t.Errorf("onboarding opens with %q, want the member greeted by name", onboarding[0].Text)
	}

	// And the enrolment it produced is a working one, not just three messages.
	h.waitForUnit("mei")
	h.tr.InjectText(meiChatID, meiTelegramID, "hello", false)
	sent := h.waitForReply(meiChatID, 4)
	if got := sent[3].Text; got != "Noted." {
		t.Errorf("reply after the private claim = %q, want the model's text", got)
	}
}

// TestRepeatedWrongCodesStopBeingProcessedAndTheSenderCannotTell covers the rate
// limit, which is the only thing standing between an 80-bit code and an attacker
// with time. Two properties matter and they pull in opposite directions: attempts
// past the limit must genuinely not be processed, and the sender must not be able
// to tell that anything changed — a limit that announces itself is a limit an
// attacker can pace themselves against.
func TestRepeatedWrongCodesStopBeingProcessedAndTheSenderCannotTell(t *testing.T) {
	h := newHousehold(t, harnessOptions{unenrolled: []domain.MemberID{"david", "mei"}})
	davidCode := h.mint("David", 0)
	meiCode := h.mint("Mei", 0)
	h.start()

	// The window is an hour and the clock does not move, so every attempt below
	// falls inside one window.
	for i := 0; i < enrol.DefaultAttemptLimit; i++ {
		h.tr.InjectText(strangerChatID, strangerUserID, wrongCode(i), false)
	}
	h.waitForConsumes(enrol.DefaultAttemptLimit)

	// One more guess, and then a genuine code from the same chat. Both are past the
	// limit, and the genuine one is the case that matters: an attacker who guessed
	// their way to a real code must not be let in with it.
	h.tr.InjectText(strangerChatID, strangerUserID, wrongCode(99), false)
	h.tr.InjectText(strangerChatID, strangerUserID, davidCode, false)

	// Mei claims from her own chat, behind them on the same pump: it is the barrier,
	// and it also shows the limit is charged per chat rather than to the household.
	h.tr.InjectText(meiChatID, meiTelegramID, meiCode, false)
	h.waitForReply(meiChatID, 3)

	if got := h.store.count(); got != enrol.DefaultAttemptLimit+1 {
		t.Errorf("the store saw %d redemption attempts, want %d (%d guesses plus mei's claim); "+
			"attempts past the limit must not be processed at all",
			got, enrol.DefaultAttemptLimit+1, enrol.DefaultAttemptLimit)
	}
	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("the rate-limited chat received %d message(s): %+v; being throttled must look exactly like being wrong",
			len(got), got)
	}
	// David's code was presented, correctly, by an account that had spent its
	// attempts. It must not have enrolled anybody.
	if !stillUnenrolled(t, h.harness, "david") {
		t.Errorf("david's health is %+v; a valid code presented past the rate limit must not bind",
			memberHealth(t, h.harness, "david"))
	}
	if _, ok := h.cfg.MemberByTelegramID(strangerUserID); ok {
		t.Error("the rate-limited sender was bound to a member")
	}
}

// TestEnrolmentSurvivesAFreshSupervisorOverTheSameDataDirectory is the assertion
// that catches a binding which only ever lived in one process's memory. A member
// who claims, and is then unenrolled again by an ordinary restart, would be told
// they were in and find the bot silent the next morning — and the operator would
// have to mint a code every day without ever seeing what was wrong.
func TestEnrolmentSurvivesAFreshSupervisorOverTheSameDataDirectory(t *testing.T) {
	dir := t.TempDir()
	first := newHousehold(t, harnessOptions{
		dataDir:    dir,
		unenrolled: []domain.MemberID{"mei"},
	})
	code := first.mint("Mei", 0)
	first.start()

	first.tr.InjectText(meiChatID, meiTelegramID, code, false)
	first.waitForReply(meiChatID, 3)
	first.waitForUnit("mei")

	if err := first.stop(); err != nil {
		t.Fatalf("stopping the first supervisor: %v", err)
	}

	// The binding is on disk beside the configuration, not in it: the second
	// household's YAML still leaves mei's telegram_id out entirely.
	st, err := config.LoadState(filepath.Join(dir, config.StateFileName))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if b, ok := st.Binding("mei"); !ok || b.TelegramID != meiTelegramID {
		t.Fatalf("state file holds %+v for mei, want a binding to %d", b, meiTelegramID)
	}

	second := newHousehold(t, harnessOptions{
		dataDir:    dir,
		unenrolled: []domain.MemberID{"mei"},
	})
	second.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	second.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The 3rd.", FinishReason: "stop"}
	})
	second.start()

	// No claim this time: the member is simply already enrolled, and the proof is
	// that an ordinary message is served over her own spaces.
	if stillUnenrolled(t, second.harness, "mei") {
		t.Fatal("mei is not enrolled in the fresh supervisor; the binding did not outlive the first one")
	}
	second.tr.InjectText(meiChatID, meiTelegramID, "when is my appointment?", false)
	sent := second.waitForReply(meiChatID, 1)

	if sent[0].Text != "The 3rd." {
		t.Errorf("reply after the restart = %q, want the model's text", sent[0].Text)
	}
	searched := second.mem.searchedSpaces()
	if len(searched) != 2 || !containsSpace(searched, meiSpace) || !containsSpace(searched, sharedSpace) {
		t.Errorf("searched %v, want the restored member's own two spaces", searched)
	}
	if second.store.count() != 0 {
		t.Errorf("the restarted household reached the code store %d times; an enrolled member claims nothing", second.store.count())
	}
}
