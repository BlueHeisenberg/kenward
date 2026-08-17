package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
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
	clock    *testClock
	store    *countingStore
	claimer  *enrol.Claimer
	personas enrol.PersonaStore
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
	extra := opts.enrolOpts
	opts.enrolFor = func(t *testing.T, cfg *config.Config) *enrol.Claimer {
		t.Helper()
		// The zero Provisioning, as the binary passes: a claim may bind a member
		// the configuration declares and may never invent one.
		binder, err := config.NewBinder(cfg, config.Provisioning{})
		if err != nil {
			t.Fatalf("building the enrolment binder: %v", err)
		}
		// The store the binary uses: the member's own answers land in
		// members[].persona, in the state file, written by the object that wrote
		// their binding. Nothing about enrolment is faked here and this is part of
		// it — a separate persona file would be a second writer this wiring does
		// not have.
		hh.personas = enrol.BinderPersonas(binder)
		c, err := enrol.New(hh.store, binder, append([]enrol.Option{
			enrol.WithClock(hh.clock.now),
			enrol.WithPersonas(hh.personas),
		}, extra...)...)
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
	h.waitForReply(davidChatID, onboardingMessages)

	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("stranger received %d message(s): %+v; an unknown sender gets silence", len(got), got)
	}
	// A typing indicator is an answer too, and there is one now: the assistant shows
	// one while a member waits on the model. It is started inside the turn, after the
	// scope has resolved, so a stranger can never reach it — which is a claim worth
	// asserting rather than reasoning about, because the natural place to put an
	// indicator is the top of Handle, where it would fire for everybody.
	if got := h.tr.TypingCount(strangerChatID); got != 0 {
		t.Errorf("stranger's chat showed %d typing indicators; an unknown sender gets silence, and an indicator says somebody is listening", got)
	}
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
	onboarding := h.waitForReply(meiChatID, onboardingMessages)

	if len(onboarding) != onboardingMessages {
		t.Fatalf("claim produced %d messages, want %d: the greeting, the note that the "+
			"tutorial was left on defaults, and the three explanation messages: %+v",
			len(onboarding), onboardingMessages, onboarding)
	}
	if !strings.Contains(onboarding[0].Text, "Hello Mei") {
		t.Errorf("onboarding opens with %q, want it to greet the member by the configuration's name", onboarding[0].Text)
	}

	// The claim is only real if the next message works.
	h.waitForUnit("mei")
	h.tr.InjectText(meiChatID, meiTelegramID, "when is my appointment?", false)
	sent := h.waitForReply(meiChatID, onboardingMessages+1)

	if got := replyBody(sent[onboardingMessages].Text); got != "The 3rd." {
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
	h.waitForReply(meiChatID, onboardingMessages)

	// Somebody else presents the same code.
	h.tr.InjectText(strangerChatID, strangerUserID, code, false)
	h.waitForConsumes(2)

	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("the second redemption produced %d message(s): %+v; a spent code is refused in silence", len(got), got)
	}
	if got := len(h.sentTo(meiChatID)); got != onboardingMessages {
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
	onboarding := h.waitForReply(meiChatID, onboardingMessages)
	if len(onboarding) != onboardingMessages {
		t.Fatalf("the private claim produced %d messages, want %d: %+v", len(onboarding), onboardingMessages, onboarding)
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
	sent := h.waitForReply(meiChatID, onboardingMessages+1)
	if got := replyBody(sent[onboardingMessages].Text); got != "Noted." {
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
	h.waitForReply(meiChatID, onboardingMessages)

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
	first.waitForReply(meiChatID, onboardingMessages)
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

	if replyBody(sent[0].Text) != "The 3rd." {
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

// TestTheTutorialIsMultiTurnAndTheMemberIsServedAfterIt is the whole of what
// enrolment stopped being when it stopped being one-shot.
//
// A member part-way through onboarding is a state this node has never had, and it
// has three properties nothing else asserts: what they type is an answer rather than
// another claim attempt, each answer is on disk before the next question goes out,
// and they get their unit when the tutorial ends rather than when the code is
// redeemed. Get the first wrong and a member's chosen agent name is fed back to the
// Claimer as a claim code; get the last wrong and the tutorial reads nothing,
// because the unit is already eating their messages.
func TestTheTutorialIsMultiTurnAndTheMemberIsServedAfterIt(t *testing.T) {
	h := newHousehold(t, harnessOptions{
		unenrolled: []domain.MemberID{"mei"},
		// One agent each, so the tutorial asks for a name — the only question that
		// cannot be a button and therefore the only one that proves the routing.
		enrolOpts: []enrol.Option{enrol.WithOneEach()},
	})
	code := h.mint("Mei", 0)
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Noted.", FinishReason: "stop"}
	})
	// Tap through the button questions; the typed ones are injected below.
	h.tr.SetAnswerFunc(func(q transport.Question) transport.Answer {
		for _, c := range q.Choices {
			if strings.HasPrefix(c.ID, "lang.") && c.ID != "lang.other" && strings.Contains(c.Label, "English") {
				return transport.Answer{ChoiceID: c.ID, UserID: meiTelegramID}
			}
			if strings.HasPrefix(c.ID, "tone.") {
				return transport.Answer{ChoiceID: c.ID, UserID: meiTelegramID}
			}
		}
		return transport.Answer{TimedOut: true}
	})
	h.start()

	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	// The greeting, then the name question: the tutorial is now waiting on a typed
	// answer and the member is still served by no unit.
	h.waitForReply(meiChatID, 2)
	if memberHealth(t, h.harness, "mei").State == supervisor.StateReady {
		t.Error("the member has a unit while their tutorial is still asking questions; " +
			"their answers would go to the assistant instead of to the question on screen")
	}

	h.tr.InjectText(meiChatID, meiTelegramID, "Jeeves", false)
	waitFor(t, "the agent name to be recorded", func() bool {
		return personaOf(t, h, "mei").AgentName == "Jeeves"
	})
	if got := h.store.count(); got != 1 {
		t.Errorf("the code store saw %d redemptions, want 1; a tutorial answer must not "+
			"be put to the Claimer as another claim", got)
	}

	// Wait for the question before answering it. A typed message is handed to the
	// tutorial only while it is actually waiting for one — see deliverToTutorial,
	// where the non-blocking send is the whole filter — and between the name being
	// recorded and the character question going out the tutorial still has a
	// confirmation to send and a button question to put. Injecting on the strength of
	// the stored name races that, which is a fact about this fake transport rather
	// than about Telegram, where nobody types faster than the next message arrives.
	waitFor(t, "the character question to be on screen", func() bool {
		for _, m := range h.sentTo(meiChatID) {
			if strings.Contains(m.Text, "Anything else about how you'd like me to be") {
				return true
			}
		}
		return false
	})
	h.tr.InjectText(meiChatID, meiTelegramID, "a bit dry, into cycling", false)
	sent := h.waitForReply(meiChatID, onboardingMessages+1)
	if !strings.Contains(sent[len(sent)-1].Text, "write something down") {
		t.Errorf("the tutorial did not end with the explanation: %q", sent[len(sent)-1].Text)
	}

	// Only now is the claim complete in the sense that matters.
	h.waitForUnit("mei")
	h.tr.InjectText(meiChatID, meiTelegramID, "hello", false)
	after := h.waitForReply(meiChatID, len(sent)+1)
	if got := replyBody(after[len(after)-1].Text); got != "Noted." {
		t.Errorf("reply after the tutorial = %q, want the model's text", got)
	}

	p := personaOf(t, h, "mei")
	if p.Language == "" || p.Tone == "" || p.Character != "a bit dry, into cycling" {
		t.Errorf("persona = %+v, want every answer recorded", p)
	}
	if !p.Explained {
		t.Error("a completed tutorial did not record that the explanation was sent")
	}
}

// personaOf reads what the tutorial has committed for a member so far.
func personaOf(t *testing.T, h *household, id domain.MemberID) enrol.Persona {
	t.Helper()
	all, err := h.personas.Personas(context.Background())
	if err != nil {
		t.Fatalf("reading personas: %v", err)
	}
	return all[id]
}

// TestTypingAtAButtonQuestionIsAnsweredOnceInTheTutorialsLanguage is the third live
// complaint: total silence.
//
// A member part-way through onboarding typed at a question that had buttons. The
// message was dropped, which is correct and stays correct — it is not an answer to
// anything, and buffering it would make it the answer to whatever comes next — and
// they got nothing back at all, on a question that looks answerable by typing.
//
// Two things make this more than a constant in the pump, and both are asserted. The
// sentence is in the tutorial's current language, which is not the household's: this
// member picks Spanish at question one and the household is English, so a reply
// composed by the reader that drops the message would go out in the wrong language.
// And a member who types four times gets one reply, not four.
func TestTypingAtAButtonQuestionIsAnsweredOnceInTheTutorialsLanguage(t *testing.T) {
	// One agent each, because this test needs a button question *after* the language
	// one — that is the whole point, the nudge has to come out in the language the
	// member chose rather than the household's. Under one shared assistant the
	// language question is the only question there is: the register and the character
	// are the household's, so they are not asked. See internal/enrol's step list.
	h := newHousehold(t, harnessOptions{
		unenrolled: []domain.MemberID{"mei"},
		enrolOpts:  []enrol.Option{enrol.WithOneEach()},
	})
	code := h.mint("Mei", 0)

	// The register question is held open, which is what a member looking at a
	// keyboard and deciding is. Everything before it is answered at once.
	release := make(chan struct{})
	var held atomic.Bool
	h.tr.SetAnswerFunc(func(q transport.Question) transport.Answer {
		for _, c := range q.Choices {
			if c.ID == "lang.es" {
				return transport.Answer{ChoiceID: c.ID, UserID: meiTelegramID}
			}
			if strings.HasPrefix(c.ID, "tone.") {
				held.Store(true)
				<-release
				return transport.Answer{ChoiceID: c.ID, UserID: meiTelegramID}
			}
		}
		return transport.Answer{TimedOut: true}
	})
	h.start()

	// nudges is every message so far that is the "those are buttons" sentence, in
	// either language. Counting them by content rather than by a message-index delta
	// is what makes the retry below safe: a nudge is armed when a button question goes
	// up and swapped away by the first typed message that misses it, so what has to be
	// asserted is that exactly one exists for this question, not which position in the
	// transcript it landed at.
	nudges := func() []string {
		var out []string
		for _, m := range h.sentTo(meiChatID) {
			if strings.Contains(m.Text, "botones de arriba") || strings.Contains(m.Text, "buttons above") {
				out = append(out, m.Text)
			}
		}
		return out
	}

	h.tr.InjectText(meiChatID, meiTelegramID, code, false)
	// The name question is typed and sits between the language and the register under
	// one agent each, so it is answered here rather than by the answer func above.
	//
	// The wait for it on screen comes first and is not optional: a message typed while
	// the *language* question is up earns the nudge for that question, which is the
	// household's English, and the assertions below would then be reading the wrong
	// sentence.
	waitFor(t, "the name question to be on screen", func() bool {
		for _, m := range h.sentTo(meiChatID) {
			if strings.Contains(m.Text, "Mi nombre") {
				return true
			}
		}
		return false
	})
	// From here the skip is retried rather than injected once. The pump hands a typed
	// message to the tutorial only while the tutorial is blocked waiting for one, on an
	// unbuffered channel with a non-blocking send, so an injection that lands in the
	// gap between the question being sent and the tutorial reaching its receive is
	// dropped on the floor and nothing ever arrives. Waiting for the question on screen
	// narrows that gap and does not close it, and under a loaded machine it stayed open
	// long enough to hang the whole test. A retry that misses at the name question
	// costs nothing — no nudge is armed at a typed one — and the loop stops the moment
	// the register question takes the screen.
	waitFor(t, "the register question to be on screen", func() bool {
		if held.Load() {
			return true
		}
		h.tr.InjectText(meiChatID, meiTelegramID, "/skip", false)
		return held.Load()
	})

	for range 4 {
		h.tr.InjectText(meiChatID, meiTelegramID, "warm please", false)
	}
	waitFor(t, "the member to be told their typing was not an answer", func() bool {
		return len(nudges()) > 0
	})
	nudge := nudges()[0]

	if !strings.Contains(nudge, "botones de arriba") {
		t.Errorf("the member was answered in the wrong language: %q\n"+
			"the household is English and this member chose Spanish at question one, "+
			"which is why the sentence comes from the tutorial and not from the pump", nudge)
	}
	if strings.Contains(nudge, "buttons above") {
		t.Errorf("the household's language reached a member who chose another: %q", nudge)
	}

	// Let the question be answered and drive the tutorial to its end. The last
	// question is a typed one, so a message that arrives there is delivered rather
	// than dropped — which is both the barrier this test needs (everything injected
	// before it has been read by the same single pump) and the property the drop
	// exists to protect: none of the four messages typed at the button question
	// became the answer to this one.
	close(release)
	waitFor(t, "the character question to be on screen", func() bool {
		for _, m := range h.sentTo(meiChatID) {
			if strings.Contains(m.Text, "Algo más") {
				return true
			}
		}
		return false
	})
	h.tr.InjectText(meiChatID, meiTelegramID, "un poco seco", false)
	waitFor(t, "the character to be recorded", func() bool {
		return personaOf(t, h, "mei").Character != ""
	})
	if got := personaOf(t, h, "mei"); got.Character != "un poco seco" || got.Tone == "" {
		t.Errorf("persona = %+v; a message typed at a button question was eaten by a later one", got)
	}

	// Four messages, one reply. A chatty member must not collect one nudge each, and
	// the character question above is the barrier that proves the pump has read all
	// four. One is also the right answer whatever the skip retry contributed, which is
	// the property that makes that retry safe: the nudge is armed once per button
	// question and swapped away by whichever typed message misses it first.
	if got := nudges(); len(got) != 1 {
		t.Errorf("four typed messages at one button question produced %d nudges, want 1: %q", len(got), got)
	}
}
