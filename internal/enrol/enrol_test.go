package enrol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// testIters is the PBKDF2 work factor used in tests.
//
// kdfIterations is a production constant deliberately chosen to be slow, and the
// tests here mint and redeem hundreds of codes under -race. The digests never leave
// the test, and every test that compares one derives both sides at the same cost,
// so lowering it changes nothing the tests are actually asserting.
// TestDefaultWorkFactor exercises the real constant once.
const testIters = 2

var epoch = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// clock is a hand-advanced time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: epoch} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeBinder stands in for the configuration, which owns the member set.
type fakeBinder struct {
	mu      sync.Mutex
	members map[domain.MemberID]domain.Member
	byTG    map[int64]domain.MemberID
	err     error // forced failure from Bind
	binds   int
}

func newBinder() *fakeBinder {
	return &fakeBinder{
		members: map[domain.MemberID]domain.Member{},
		byTG:    map[int64]domain.MemberID{},
	}
}

func (b *fakeBinder) Bind(ctx context.Context, id domain.MemberID, name string, telegramID int64, at time.Time) (domain.Member, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return domain.Member{}, b.err
	}
	if other, ok := b.byTG[telegramID]; ok && other != id {
		return domain.Member{}, fmt.Errorf("telegram id %d already bound to %q", telegramID, other)
	}
	m := domain.Member{
		ID:         id,
		Name:       name,
		TelegramID: telegramID,
		Private:    domain.SpaceID(string(id) + "-private"),
		Tiers:      []string{"local"},
		EnrolledAt: at,
	}
	b.members[id] = m
	b.byTG[telegramID] = id
	b.binds++
	return m, nil
}

func (b *fakeBinder) Unbind(ctx context.Context, id domain.MemberID) (domain.Member, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[id]
	if !ok {
		return domain.Member{}, fmt.Errorf("%w: %q", ErrUnknownMember, id)
	}
	delete(b.byTG, m.TelegramID)
	m.TelegramID = 0
	b.members[id] = m
	return m, nil
}

func (b *fakeBinder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.binds
}

// harness is a Claimer plus everything the tests need to steer it.
type harness struct {
	claimer *Claimer
	store   Store
	binder  *fakeBinder
	clock   *clock
}

func newHarness(t *testing.T, store Store, opts ...Option) *harness {
	t.Helper()
	if store == nil {
		store = NewMemStore()
	}
	clk := newClock()
	binder := newBinder()
	c, err := New(store, binder, append([]Option{WithClock(clk.now)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.iters = testIters
	return &harness{claimer: c, store: store, binder: binder, clock: clk}
}

// claim builds an inbound direct message carrying the given text.
func claim(chatID, userID int64, text string) transport.Inbound {
	return transport.Inbound{ChatID: chatID, UserID: userID, Text: text, MessageID: 1}
}

// assertSilent is the property the whole package exists to hold: a rejected sender
// gets nothing at all, and nothing was bound on the way to deciding that.
func assertSilent(t *testing.T, res Result, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if res.Enrolled {
		t.Error("rejected claim reported Enrolled")
	}
	if len(res.Messages) != 0 {
		t.Errorf("rejected claim produced %d outbound messages, want 0: %+v", len(res.Messages), res.Messages)
	}
	if res.Member.ID != "" || res.Member.TelegramID != 0 {
		t.Errorf("rejected claim returned a member: %+v", res.Member)
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(nil, newBinder()); !errors.Is(err, ErrNoStore) {
		t.Fatalf("New(nil, ...) error = %v, want ErrNoStore", err)
	}
}

func TestMintRedeem(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := Normalize(code); len(got) != CodeSymbols {
		t.Fatalf("minted code %q normalizes to %d symbols, want %d", code, len(got), CodeSymbols)
	}
	if !strings.Contains(code, "-") {
		t.Errorf("minted code %q is not grouped for reading", code)
	}

	res, err := h.claimer.Handle(ctx, claim(500, 42, "/start "+code))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Enrolled {
		t.Fatal("valid code did not enrol")
	}
	if res.Member.ID != "david" || res.Member.Name != "David" || res.Member.TelegramID != 42 {
		t.Errorf("bound member = %+v", res.Member)
	}
	if !res.Member.Enrolled() {
		t.Error("bound member does not report Enrolled")
	}
	if got := len(res.Messages); got != 1 {
		t.Fatalf("a claim sends %d messages before the tutorial, want 1 (the greeting)", got)
	}
	for i, m := range res.Messages {
		if m.ChatID != 500 {
			t.Errorf("message %d addressed to chat %d, want 500", i, m.ChatID)
		}
		if strings.TrimSpace(m.Text) == "" {
			t.Errorf("message %d is empty", i)
		}
	}
	if !strings.Contains(res.Messages[0].Text, "David") {
		t.Errorf("onboarding does not greet the member by name: %q", res.Messages[0].Text)
	}
}

func TestMintRejectsEmptyName(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.claimer.Mint(context.Background(), "   ", 0); !errors.Is(err, ErrNoName) {
		t.Fatalf("Mint(\"\") error = %v, want ErrNoName", err)
	}
}

func TestMintStoresNoPlaintext(t *testing.T) {
	store := NewMemStore()
	h := newHarness(t, store)
	code, err := h.claimer.Mint(context.Background(), "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, c := range store.codes {
		if strings.Contains(c.Hash, Normalize(code)) || c.Hash == Normalize(code) {
			t.Fatalf("stored record holds the plaintext: %+v", c)
		}
		if c.Consumed() {
			t.Errorf("freshly minted code is already consumed: %+v", c)
		}
		if !c.Live(h.clock.now()) {
			t.Errorf("freshly minted code is not live: %+v", c)
		}
	}
}

func TestExpiredCode(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h.clock.advance(time.Hour + time.Second)

	res, err := h.claimer.Handle(ctx, claim(500, 42, code))
	assertSilent(t, res, err, ErrCodeExpired)
	if h.binder.count() != 0 {
		t.Error("expired code bound a member")
	}
}

func TestExpiryBoundaryIsInclusive(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h.clock.advance(time.Hour)
	res, err := h.claimer.Handle(ctx, claim(500, 42, code))
	assertSilent(t, res, err, ErrCodeExpired)
}

func TestDefaultTTL(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h.clock.advance(DefaultTTL - time.Minute)
	if _, err := h.claimer.Handle(ctx, claim(500, 42, code)); err != nil {
		t.Fatalf("code died before the default TTL: %v", err)
	}
}

func TestAlreadyConsumedCode(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := h.claimer.Handle(ctx, claim(500, 42, code)); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	res, err := h.claimer.Handle(ctx, claim(501, 43, code))
	assertSilent(t, res, err, ErrCodeConsumed)
	if h.binder.count() != 1 {
		t.Errorf("binder called %d times, want 1", h.binder.count())
	}
}

func TestWrongCode(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, err := h.claimer.Mint(ctx, "David", 0); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	res, err := h.claimer.Handle(ctx, claim(500, 42, "ZZZZ-ZZZZ-ZZZZ-ZZZZ"))
	assertSilent(t, res, err, ErrUnknownCode)
	if h.binder.count() != 0 {
		t.Error("wrong code bound a member")
	}
}

// TestSilence is the security property stated plainly: for every way a sender can
// fail to be a member, the outbound message count is zero.
func TestSilence(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		in   transport.Inbound
		want error
		prep func(*harness) transport.Inbound
	}{
		{name: "plain hello", in: claim(500, 42, "hello"), want: ErrNoCode},
		{name: "start with no code", in: claim(500, 42, "/start"), want: ErrNoCode},
		{name: "empty message", in: claim(500, 42, ""), want: ErrNoCode},
		{name: "unknown code", in: claim(500, 42, "ZZZZ-ZZZZ-ZZZZ-ZZZZ"), want: ErrUnknownCode},
		{
			name: "valid code in a group chat",
			want: ErrNotDirect,
			prep: func(h *harness) transport.Inbound {
				code, err := h.claimer.Mint(ctx, "David", 0)
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				in := claim(-100200, 42, code)
				in.IsGroup = true
				return in
			},
		},
		{
			name: "expired code",
			want: ErrCodeExpired,
			prep: func(h *harness) transport.Inbound {
				code, err := h.claimer.Mint(ctx, "David", time.Minute)
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				h.clock.advance(2 * time.Minute)
				return claim(500, 42, code)
			},
		},
		{
			name: "consumed code",
			want: ErrCodeConsumed,
			prep: func(h *harness) transport.Inbound {
				code, err := h.claimer.Mint(ctx, "David", 0)
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				if _, err := h.claimer.Handle(ctx, claim(500, 42, code)); err != nil {
					t.Fatalf("first redemption: %v", err)
				}
				return claim(501, 43, code)
			},
		},
		{
			name: "rate limited",
			want: ErrRateLimited,
			prep: func(h *harness) transport.Inbound {
				in := claim(500, 42, "ZZZZ-ZZZZ-ZZZZ-ZZZZ")
				for i := 0; i < DefaultAttemptLimit; i++ {
					if _, err := h.claimer.Handle(ctx, in); !errors.Is(err, ErrUnknownCode) {
						t.Fatalf("attempt %d: %v", i, err)
					}
				}
				return in
			},
		},
		{
			name: "binder refuses",
			want: ErrNoBinder,
			prep: func(h *harness) transport.Inbound {
				code, err := h.claimer.Mint(ctx, "David", 0)
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				h.claimer.binder = nil
				return claim(500, 42, code)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			in := tc.in
			if tc.prep != nil {
				in = tc.prep(h)
			}
			res, err := h.claimer.Handle(ctx, in)
			assertSilent(t, res, err, tc.want)
		})
	}
}

// TestGroupChatIsRefusedBeforeAnythingIsSpent proves the group check runs before the
// code is looked at: a code shouted into a group must still be redeemable in private.
func TestGroupChatIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	in := claim(-100200, 42, code)
	in.IsGroup = true
	res, err := h.claimer.Handle(ctx, in)
	assertSilent(t, res, err, ErrNotDirect)

	if _, err := h.claimer.Handle(ctx, claim(500, 42, code)); err != nil {
		t.Fatalf("code was spent by the group attempt: %v", err)
	}
}

// TestConcurrentRedemption is the atomicity test. Many chats present the same code
// at once; exactly one may be enrolled, and the binder may be called exactly once.
func TestConcurrentRedemption(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) Store
	}{
		{"mem", func(*testing.T) Store { return NewMemStore() }},
		{"file", func(t *testing.T) Store { return NewFileStore(filepath.Join(t.TempDir(), "codes.json")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.store(t))
			ctx := context.Background()
			code, err := h.claimer.Mint(ctx, "David", 0)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}

			const racers = 48
			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				wins    int
				losses  int
				surplus []error
			)
			start := make(chan struct{})
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					// A distinct chat id each, so the rate limit is not what
					// decides this race.
					res, err := h.claimer.Handle(ctx, claim(int64(1000+i), int64(2000+i), code))
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil && res.Enrolled && len(res.Messages) == 1:
						wins++
					case errors.Is(err, ErrCodeConsumed):
						losses++
					default:
						surplus = append(surplus, fmt.Errorf("racer %d: res=%+v err=%v", i, res, err))
					}
				}(i)
			}
			close(start)
			wg.Wait()

			if wins != 1 {
				t.Errorf("%d racers were enrolled, want exactly 1", wins)
			}
			if losses != racers-1 {
				t.Errorf("%d racers got ErrCodeConsumed, want %d", losses, racers-1)
			}
			for _, err := range surplus {
				t.Errorf("unexpected outcome: %v", err)
			}
			if got := h.binder.count(); got != 1 {
				t.Errorf("binder called %d times, want 1", got)
			}
		})
	}
}

func TestRateLimitSlidingWindow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	bad := claim(500, 42, "ZZZZ-ZZZZ-ZZZZ-ZZZZ")

	for i := 0; i < DefaultAttemptLimit; i++ {
		if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrUnknownCode) {
			t.Fatalf("attempt %d was refused early: %v", i+1, err)
		}
		h.clock.advance(time.Minute)
	}
	if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("attempt %d error = %v, want ErrRateLimited", DefaultAttemptLimit+1, err)
	}

	// Another chat is unaffected: the budget is per chat id.
	if _, err := h.claimer.Handle(ctx, claim(501, 43, "ZZZZ-ZZZZ-ZZZZ-ZZZZ")); !errors.Is(err, ErrUnknownCode) {
		t.Fatalf("a second chat was caught by the first chat's limit: %v", err)
	}

	// Half an hour on, the window has not slid far enough. Clock: epoch+35m.
	h.clock.advance(30 * time.Minute)
	if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limit lifted early: %v", err)
	}

	// Just past the first attempt's hour (epoch+60m30s), exactly one slot frees up:
	// the attempt made at epoch. The other four were a minute apart and are still
	// inside the window. A fixed hourly bucket would have freed all five at once.
	h.clock.advance(25*time.Minute + 30*time.Second)
	if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrUnknownCode) {
		t.Fatalf("window did not slide: %v", err)
	}
	if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("more than one slot was freed: %v", err)
	}
}

// TestRateLimitStopsProcessing proves the limit is not cosmetic: a refused attempt
// never reaches the store, so a valid code presented over the limit is not spent.
func TestRateLimitStopsProcessing(t *testing.T) {
	h := newHarness(t, nil, WithRateLimit(2, time.Hour))
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	bad := claim(500, 42, "ZZZZ-ZZZZ-ZZZZ-ZZZZ")
	for i := 0; i < 2; i++ {
		if _, err := h.claimer.Handle(ctx, bad); !errors.Is(err, ErrUnknownCode) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	res, err := h.claimer.Handle(ctx, claim(500, 42, code))
	assertSilent(t, res, err, ErrRateLimited)

	// The code survived, and is redeemable from a chat with budget left.
	if _, err := h.claimer.Handle(ctx, claim(501, 43, code)); err != nil {
		t.Fatalf("rate-limited attempt consumed the code: %v", err)
	}
}

// TestBindFailureDoesNotResurrectTheCode: if binding fails after the code has been
// consumed, the code stays consumed. Better a wasted invite than a spendable one.
func TestBindFailureDoesNotResurrectTheCode(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h.binder.err = errors.New("member set is read-only")

	res, err := h.claimer.Handle(ctx, claim(500, 42, code))
	if err == nil {
		t.Fatal("bind failure was not reported")
	}
	if res.Enrolled || len(res.Messages) != 0 {
		t.Errorf("bind failure produced output: %+v", res)
	}

	h.binder.err = nil
	if _, err := h.claimer.Handle(ctx, claim(501, 43, code)); !errors.Is(err, ErrCodeConsumed) {
		t.Fatalf("code was spendable again after a bind failure: %v", err)
	}
}

func TestMemberIDFor(t *testing.T) {
	tests := []struct{ in, want string }{
		{"David", "david"},
		{"  David  ", "david"},
		{"Ana María", "ana-mar-a"},
		{"Jo-Anne", "jo-anne"},
		{"J. R. Smith", "j-r-smith"},
		{"!!!", ""},
	}
	for _, tc := range tests {
		if got := MemberIDFor(tc.in); string(got) != tc.want {
			t.Errorf("MemberIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRevoke(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	code, err := h.claimer.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := h.claimer.Handle(ctx, claim(500, 42, code)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rev, err := h.claimer.Revoke(ctx, "david")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rev.Member.TelegramID != 0 {
		t.Errorf("revoked member still bound to telegram id %d", rev.Member.TelegramID)
	}
	if rev.Space != "david-private" {
		t.Errorf("revocation names space %q", rev.Space)
	}
	if !rev.KeyRotationRequired() {
		t.Error("KeyRotationRequired must always be true")
	}
	w := rev.Warning()
	for _, want := range []string{"David", "david-private", "NOT", "kenward cannot rotate", "Rotate it in lore"} {
		if !strings.Contains(w, want) {
			t.Errorf("revocation warning does not mention %q:\n%s", want, w)
		}
	}
}

func TestRevokeUnknownMember(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.claimer.Revoke(context.Background(), "nobody"); !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("Revoke(unknown) error = %v, want ErrUnknownMember", err)
	}
}

func TestMintOnlyClaimerHasNoBinder(t *testing.T) {
	c, err := New(NewMemStore(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.iters = testIters
	ctx := context.Background()
	if _, err := c.Mint(ctx, "David", 0); err != nil {
		t.Fatalf("Mint without a binder: %v", err)
	}
	if _, err := c.Revoke(ctx, "david"); !errors.Is(err, ErrNoBinder) {
		t.Fatalf("Revoke error = %v, want ErrNoBinder", err)
	}
}

func TestPurge(t *testing.T) {
	store := NewMemStore()
	h := newHarness(t, store)
	ctx := context.Background()

	if _, err := h.claimer.Mint(ctx, "David", time.Hour); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	keep, err := h.claimer.Mint(ctx, "Ana", 48*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if store.Len() != 2 {
		t.Fatalf("store holds %d codes, want 2", store.Len())
	}

	h.clock.advance(2 * time.Hour)
	if err := h.claimer.Purge(ctx, h.clock.now()); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("store holds %d codes after purge, want 1", store.Len())
	}
	if _, err := h.claimer.Handle(ctx, claim(500, 42, keep)); err != nil {
		t.Fatalf("purge removed a live code: %v", err)
	}
}

// TestDefaultWorkFactor runs one mint and one redemption at the production PBKDF2
// cost, so the constant cannot drift somewhere absurd unnoticed.
func TestDefaultWorkFactor(t *testing.T) {
	clk := newClock()
	c, err := New(NewMemStore(), newBinder(), WithClock(clk.now))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.iters != kdfIterations {
		t.Fatalf("default work factor is %d, want %d", c.iters, kdfIterations)
	}
	ctx := context.Background()
	code, err := c.Mint(ctx, "David", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	res, err := c.Handle(ctx, claim(500, 42, code))
	if err != nil || !res.Enrolled {
		t.Fatalf("Handle at production cost: res=%+v err=%v", res, err)
	}
}

func TestExplanationCopy(t *testing.T) {
	// The third message describes the buttons the member is about to be shown, so
	// it has to differ with capture.private_writes. Under the default a private
	// note is written and then announced with Undo; under "ask" it is a question
	// first. Promising the wrong one is the failure this test exists to catch — it
	// went out over real Telegram once.
	cases := []struct {
		name       string
		askPrivate bool
		want       []string
		reject     []string
	}{
		{
			name: "default writes then offers Undo",
			want: []string{
				"I write it down and then show you exactly what I wrote",
				"Undo button",
				"shared memory I ask about first",
			},
			reject: []string{"I never save anything by myself"},
		},
		{
			name:       "ask puts it as a question first",
			askPrivate: true,
			want: []string{
				"I never save anything by myself",
				"If you don't answer, I don't save it.",
			},
			reject: []string{"Undo button"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := Explanation(500, lang.For(lang.English), tc.askPrivate, privacy.ModeSimple)
			if len(msgs) != 3 {
				t.Fatalf("explanation is %d messages, want 3", len(msgs))
			}
			all := ""
			for _, m := range msgs {
				if m.ChatID != 500 {
					t.Errorf("message addressed to chat %d", m.ChatID)
				}
				if m.ReplyTo != 0 {
					t.Errorf("the explanation should not reply to a message id")
				}
				all += m.Text + "\n"
			}
			want := append([]string{
				"your private memory",
				"household group chat is the shared memory",
			}, tc.want...)
			for _, w := range want {
				if !strings.Contains(all, w) {
					t.Errorf("onboarding does not say %q:\n%s", w, all)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(all, r) {
					t.Errorf("onboarding says %q, which is the other policy's promise:\n%s", r, all)
				}
			}
			// It must not promise anything kenward does not do.
			for _, bad := range []string{"encrypt", "delete everything", "anytime", "AI-powered"} {
				if strings.Contains(strings.ToLower(all), strings.ToLower(bad)) {
					t.Errorf("onboarding claims %q, which nothing downstream implements", bad)
				}
			}
		})
	}
}
