// Package enrol turns a stranger into a household member, and back again.
//
// A Telegram bot username is public and anyone on the internet can send it /start.
// Without a secret in the loop, dynamic enrolment would quietly change kenward's
// authorization model from "anyone not in the member map is ignored" into "anyone
// who found the bot is in the member map". Claim codes are what stops that: a
// single-use, expiring, high-entropy code, minted by the operator out of band and
// spoken or messaged to the person it is for.
//
// The second half of the defence is silence. A sender who has not presented a valid
// code gets nothing back — no error, no prompt, not even a typing indicator. Any
// reply at all confirms to a stranger that this bot is live and belongs to someone,
// which is the fact the whole arrangement exists to withhold. Every failure path in
// this package therefore produces an empty Result. The errors it returns are for the
// operator's log; they are never for the sender, and a caller that turns one into a
// message has broken the model.
package enrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Sentinel errors. Every one of them must produce the same thing on the wire —
// nothing — and they are distinguished only so the operator can tell a typo from an
// attack when reading logs.
var (
	// ErrNoStore is returned by New when no Store was supplied.
	ErrNoStore = errors.New("enrol: no store")
	// ErrNoBinder is returned when a Claimer built without a Binder is asked to
	// bind or unbind. Minting does not need one.
	ErrNoBinder = errors.New("enrol: no binder")
	// ErrNoName is returned by Mint when the invite names nobody.
	ErrNoName = errors.New("enrol: invite needs a name")
	// ErrInvalidCode is returned by a Store asked to save a malformed record.
	ErrInvalidCode = errors.New("enrol: invalid code record")
	// ErrDuplicateCode is returned by a Store asked to save a digest it already
	// holds. In practice this means a repeat of an 80-bit random value, so it is a
	// bug or a broken entropy source, not a collision worth handling.
	ErrDuplicateCode = errors.New("enrol: code already stored")
	// ErrNoCode means the message contained nothing code-shaped. The ordinary case
	// for a stranger who has simply found the bot.
	ErrNoCode = errors.New("enrol: message contains no claim code")
	// ErrNotDirect means a code was presented in a group chat. Refused: a code sent
	// to a group is a code every member of that group has seen.
	ErrNotDirect = errors.New("enrol: claim codes are only accepted in a direct chat")
	// ErrUnknownCode means the digest matched no stored code.
	ErrUnknownCode = errors.New("enrol: unknown claim code")
	// ErrCodeExpired means the code was real but is past its expiry.
	ErrCodeExpired = errors.New("enrol: claim code expired")
	// ErrCodeConsumed means the code was real and has already been redeemed.
	ErrCodeConsumed = errors.New("enrol: claim code already used")
	// ErrRateLimited means this chat has spent its attempts for the hour. Nothing
	// about the attempt was examined; it was not processed at all.
	ErrRateLimited = errors.New("enrol: too many claim attempts")
	// ErrUnknownMember is returned by Revoke for a member the Binder does not know.
	ErrUnknownMember = errors.New("enrol: unknown member")
)

// Defaults for a Claimer built without options.
const (
	// DefaultTTL is how long a minted code stays redeemable.
	DefaultTTL = 24 * time.Hour
	// DefaultAttemptLimit is how many claim attempts one chat may make per window.
	DefaultAttemptLimit = 5
	// DefaultAttemptWindow is the sliding window the attempt limit applies over.
	DefaultAttemptWindow = time.Hour
)

// Code is a minted claim code as it is stored: hashed, never in plaintext.
//
// The plaintext exists once, in the operator's terminal, and is not recoverable
// from this record. Losing it means minting another, which is the correct cost.
//
// There is deliberately no record of which Telegram id redeemed a code. Store's
// Consume takes a digest and nothing else, and the binding it produces is the
// Member's business; keeping a second copy of the mapping here would be an audit
// trail nobody asked for in a file that outlives the invite.
type Code struct {
	// Hash is the PBKDF2 digest of the normalized plaintext, hex encoded.
	Hash string `json:"hash"`
	// Name is the person the invite was minted for, as the operator typed it. It is
	// what the onboarding greets them by.
	Name string `json:"name"`
	// MemberID is the member this code enrols.
	MemberID domain.MemberID `json:"member_id"`
	// IssuedAt is when the code was minted.
	IssuedAt time.Time `json:"issued_at"`
	// ExpiresAt is when it stops being redeemable.
	ExpiresAt time.Time `json:"expires_at"`
	// ConsumedAt is zero until the code has been redeemed.
	ConsumedAt time.Time `json:"consumed_at,omitempty"`
}

// Consumed reports whether the code has been redeemed.
func (c Code) Consumed() bool { return !c.ConsumedAt.IsZero() }

// Expired reports whether the code is past its expiry as of now. The boundary is
// inclusive: a code is dead at its expiry instant, not a moment after.
func (c Code) Expired(now time.Time) bool { return !c.ExpiresAt.After(now) }

// Live reports whether the code could still be redeemed as of now.
func (c Code) Live(now time.Time) bool { return !c.Consumed() && !c.Expired(now) }

// validate rejects records a Store must not accept.
func (c Code) validate() error {
	switch {
	case len(c.Hash) != hashLen*2:
		return fmt.Errorf("%w: hash is %d chars, want %d", ErrInvalidCode, len(c.Hash), hashLen*2)
	case c.Name == "":
		return fmt.Errorf("%w: no name", ErrInvalidCode)
	case c.MemberID == "":
		return fmt.Errorf("%w: no member id", ErrInvalidCode)
	case c.ExpiresAt.IsZero():
		return fmt.Errorf("%w: no expiry", ErrInvalidCode)
	}
	return nil
}

// Binder owns the member set and performs the binding a claim results in.
//
// It lives outside this package because enrolment does not own the configuration:
// which members exist, where their private space is and what tier chain they may use
// are decided elsewhere. This package decides only whether a claim is legitimate.
type Binder interface {
	// Bind attaches a Telegram user id to a member, creating the member from the
	// invited name if the configuration does not already carry one, and returns the
	// member as it now stands. It must reject a Telegram id already bound to a
	// different member rather than moving it.
	Bind(ctx context.Context, id domain.MemberID, name string, telegramID int64, at time.Time) (domain.Member, error)
	// Unbind clears a member's Telegram binding and returns the member as it was
	// before. It returns ErrUnknownMember for a member it does not hold.
	Unbind(ctx context.Context, id domain.MemberID) (domain.Member, error)
}

// Result is the outcome of handling one inbound message from an unknown sender.
//
// The zero value is the silent outcome and is what every rejection produces. A
// caller can treat "no messages" as the complete instruction: send nothing.
type Result struct {
	// Enrolled reports whether this message completed a claim.
	Enrolled bool
	// Member is the newly bound member. Meaningful only when Enrolled.
	Member domain.Member
	// Messages is the onboarding, in order. Empty on every failure path.
	Messages []transport.Outbound
}

// Revocation is what Revoke reports back.
//
// It exists as a type rather than a bare error so the key-rotation caveat travels
// with the result and cannot be dropped by a caller that only checks err == nil.
type Revocation struct {
	// Member is the member as it was before the binding was cleared.
	Member domain.Member
	// Space is the lore space the member could read, and still can if they kept a
	// copy of its key.
	Space domain.SpaceID
	// At is when the binding was cleared.
	At time.Time
	// Deferred says the binding this revocation clears does not live where this
	// process can reach it, so what happened here is a record rather than the act.
	//
	// It is isolated mode, where a member's binding is written by that member's own
	// pod into that member's own volume. The host must not write there — every
	// mechanism that could is one edit from reading it back, and that volume holds
	// the member's wrapped key and their lore — so the revocation is recorded on the
	// host and applied by the pod when it is next created. Until then the pod carries
	// on serving them, and Warning says so instead of saying the opposite.
	Deferred bool
}

// KeyRotationRequired always reports true.
//
// It is a method rather than a field because it is not a condition to evaluate: it
// is true of every revocation kenward can perform. Unbinding stops messages from
// that Telegram account being served. It does nothing to a key the member already
// holds, and kenward has no authority to re-key a lore space.
func (r Revocation) KeyRotationRequired() bool { return true }

// Warning is the operator-facing text for a revocation. It says what was actually
// done and what was not, because a revocation that reads as complete when it is not
// is a false security claim, and those are worse than no claim.
func (r Revocation) Warning() string {
	unbound := fmt.Sprintf("%s is unbound: messages from that Telegram account are ignored from now on.",
		r.Member.Name)
	if r.Deferred {
		// The opposite fact, said first, because an operator who reads one line of
		// this reads the first one and a revocation that has not happened yet must
		// not be mistaken for one that has. See Deferred.
		unbound = fmt.Sprintf("%s is NOT unbound yet: the binding lives in their own pod, and this command\n"+
			"has recorded the revocation rather than performed it.", r.Member.Name)
	}
	return fmt.Sprintf(
		"%s\n"+
			"Their lore space %q has NOT been re-keyed — kenward cannot rotate a lore key.\n"+
			"Anyone still holding the old key can read everything written to that space,\n"+
			"including anything written after this point. Rotate it in lore now.",
		unbound, string(r.Space))
}

// Option configures a Claimer.
type Option func(*Claimer)

// WithClock replaces the clock. For tests.
func WithClock(now func() time.Time) Option {
	return func(c *Claimer) {
		if now != nil {
			c.now = now
		}
	}
}

// WithTTL sets the default lifetime of a minted code.
func WithTTL(d time.Duration) Option {
	return func(c *Claimer) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithRateLimit sets how many claim attempts one chat may make per sliding window.
func WithRateLimit(attempts int, window time.Duration) Option {
	return func(c *Claimer) {
		if attempts > 0 && window > 0 {
			c.limit, c.window = attempts, window
		}
	}
}

// WithAskPrivateWrites says the household has capture.private_writes: ask, so the
// onboarding describes a question rather than a write with an Undo button. Unset is
// kenward's default, which writes first and shows the member what it wrote.
func WithAskPrivateWrites() Option {
	return func(c *Claimer) { c.askPrivate = true }
}

// Claimer mints claim codes and processes messages from senders who are not yet
// members. It is safe for concurrent use.
type Claimer struct {
	store      Store
	binder     Binder
	now        func() time.Time
	ttl        time.Duration
	iters      int
	limit      int
	window     time.Duration
	askPrivate bool

	mu       sync.Mutex
	attempts map[int64][]time.Time
}

// New returns a Claimer over the given store.
//
// binder may be nil for a mint-only use such as `kenward invite`, which needs to
// write a code but never to bind anything; Handle and Revoke then return
// ErrNoBinder rather than pretending to have enrolled someone.
func New(store Store, binder Binder, opts ...Option) (*Claimer, error) {
	if store == nil {
		return nil, ErrNoStore
	}
	c := &Claimer{
		store:    store,
		binder:   binder,
		now:      time.Now,
		ttl:      DefaultTTL,
		iters:    kdfIterations,
		limit:    DefaultAttemptLimit,
		window:   DefaultAttemptWindow,
		attempts: make(map[int64][]time.Time),
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Mint generates a single-use claim code for the named person, stores its digest
// and returns the plaintext, formatted for printing.
//
// The member it enrols is MemberIDFor(name), which is the right answer only for a
// household that has not chosen an id of its own. A caller holding a declared member
// must use MintFor and say which member it means; see that method.
//
// This is the only moment the code exists in the clear. Nothing keeps a copy: if
// the operator loses it before it reaches the person, the only recovery is minting
// another and letting the first one expire.
//
// A ttl of zero or less means the Claimer's default (DefaultTTL, 24h).
//
// The signature carries a context because Save can block on a filesystem, which the
// module's ground rules say makes it a context-taking call.
func (c *Claimer) Mint(ctx context.Context, name string, ttl time.Duration) (string, error) {
	return c.MintFor(ctx, MemberIDFor(strings.TrimSpace(name)), name, ttl)
}

// MintFor is Mint for a member the configuration already declares, recording the id
// that configuration gave them rather than one derived from their name.
//
// The two are not the same and the difference is not cosmetic. A household whose
// `id: dave` carries `name: David` produces MemberIDFor("David") == "dave" only by
// luck; when it does not, the code is stored against a member nobody declares, and
// the Binder that redeems it refuses to create one — `enrol: bind "david": config: no
// provisioning`. The operator sees a code minted successfully and the person holding
// it sees the silence enrolment owes a stranger. So the id travels with the mint, and
// an id that is empty is refused here rather than by Code.validate one layer down.
func (c *Claimer) MintFor(ctx context.Context, id domain.MemberID, name string, ttl time.Duration) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrNoName
	}
	if id == "" {
		return "", fmt.Errorf("%w: no member id for %q", ErrNoName, name)
	}
	if ttl <= 0 {
		ttl = c.ttl
	}
	plaintext, err := generateCode()
	if err != nil {
		return "", err
	}
	digest, err := hash(plaintext, c.iters)
	if err != nil {
		return "", err
	}
	now := c.now()
	if err := c.store.Save(ctx, Code{
		Hash:      digest,
		Name:      name,
		MemberID:  id,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}); err != nil {
		return "", err
	}
	return Format(plaintext), nil
}

// Handle processes one inbound message from a sender who is not a member.
//
// It returns the onboarding to send on a successful claim, and an empty Result on
// every other path. The Result is returned rather than sent so that silence is a
// value the caller can assert on, not the absence of a side effect nobody checked.
//
// The order of the checks is the security order. Group chats are refused before
// anything is parsed; the rate limit is charged before the digest is derived, so a
// flood costs the attacker their budget and costs kenward one hash at most; and the
// code is consumed before the member is bound, so a Binder that fails cannot leave a
// code spendable a second time.
func (c *Claimer) Handle(ctx context.Context, in transport.Inbound) (Result, error) {
	if in.IsGroup {
		return Result{}, ErrNotDirect
	}
	plaintext, ok := extract(in.Text)
	if !ok {
		return Result{}, ErrNoCode
	}
	if c.binder == nil {
		return Result{}, ErrNoBinder
	}
	now := c.now()
	if !c.allow(in.ChatID, now) {
		return Result{}, ErrRateLimited
	}
	digest, err := hash(plaintext, c.iters)
	if err != nil {
		return Result{}, err
	}
	code, err := c.store.Consume(ctx, digest, now)
	if err != nil {
		return Result{}, err
	}
	member, err := c.binder.Bind(ctx, code.MemberID, code.Name, in.UserID, now)
	if err != nil {
		return Result{}, fmt.Errorf("enrol: bind %q: %w", code.MemberID, err)
	}
	return Result{
		Enrolled: true,
		Member:   member,
		Messages: Onboarding(in.ChatID, member.Name, c.askPrivate),
	}, nil
}

// Revoke unbinds a member's Telegram id.
//
// It stops that account being served and nothing more. See Revocation.Warning: the
// member's lore space key is untouched, and kenward cannot touch it.
func (c *Claimer) Revoke(ctx context.Context, id domain.MemberID) (Revocation, error) {
	if c.binder == nil {
		return Revocation{}, ErrNoBinder
	}
	member, err := c.binder.Unbind(ctx, id)
	if err != nil {
		return Revocation{}, err
	}
	return Revocation{Member: member, Space: member.Private, At: c.now()}, nil
}

// Purge drops stored codes that were consumed or expired before the given time. It
// is housekeeping; an expired code was already unredeemable.
func (c *Claimer) Purge(ctx context.Context, before time.Time) error {
	return c.store.Purge(ctx, before)
}

// allow charges one attempt against a chat's sliding window, reporting whether it
// fits.
//
// The window is sliding rather than fixed: a fixed bucket would let an attacker
// spend five attempts at the end of one hour and five more moments later.
//
// Exceeding the limit changes nothing the sender can observe. They get the same
// silence as an attempt that was processed and rejected, so the limit itself is not
// something the outside can measure.
func (c *Claimer) allow(chatID int64, now time.Time) bool {
	cutoff := now.Add(-c.window)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop chats whose whole history has aged out, so a flood from many chat ids
	// does not grow this map forever.
	for id, ts := range c.attempts {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(c.attempts, id)
		}
	}

	kept := c.attempts[chatID][:0:0]
	for _, t := range c.attempts[chatID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= c.limit {
		c.attempts[chatID] = kept
		return false
	}
	c.attempts[chatID] = append(kept, now)
	return true
}

// MemberIDFor derives a stable member id from an invited name: lower-cased, with
// runs of anything that is not a letter or digit collapsed to a single hyphen.
//
// It is a starting point for an operator who ran `kenward invite --name "David"`
// and did not want to invent an id. A household that cares can set the id itself in
// the configuration; a name that collapses to nothing yields the empty id, which
// Code.validate rejects.
func MemberIDFor(name string) domain.MemberID {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			dash = false
			out.WriteRune(r)
		default:
			dash = true
		}
	}
	return domain.MemberID(out.String())
}
