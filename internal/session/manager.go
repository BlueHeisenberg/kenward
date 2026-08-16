package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Mode says who holds the passphrases that wrap member keys. It changes what
// Provision enforces and what Custody reports; the unlock path is the same
// code in both modes, because each member's key is an independent record with
// its own salt and its own wrapping key regardless of who knows the
// passphrase. There is no shared unlock shortcut for Simple mode to take and
// therefore none for Isolated mode to inherit by accident.
type Mode int

const (
	// ModeSimple wraps every member's key under one operator-held node
	// passphrase. The operator can unlock any member — that is this mode's
	// stated limitation, not a bug, and Custody reports it in exactly those
	// terms. Provision enforces the single passphrase by verifying it against
	// an already-provisioned member before accepting a new one.
	ModeSimple Mode = iota + 1

	// ModeIsolated wraps each member's key under that member's own
	// passphrase. No verification ties the passphrases together; their
	// independence is the point.
	ModeIsolated
)

// String names the mode the way configuration files spell it.
func (m Mode) String() string {
	switch m {
	case ModeSimple:
		return "simple"
	case ModeIsolated:
		return "isolated"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

func (m Mode) valid() bool { return m == ModeSimple || m == ModeIsolated }

// Errors returned by Manager beyond the ones the Sessions interface defines.
var (
	// ErrManagerClosed is returned by Unlock and Provision after Close.
	ErrManagerClosed = errors.New("session: manager closed")

	// ErrPassphraseMismatch is returned by Provision in Simple mode when the
	// supplied passphrase does not open the keys already provisioned. One node
	// passphrase wraps every member's key in that mode; enrolling members
	// under divergent passphrases would make that claim quietly false.
	ErrPassphraseMismatch = errors.New("session: passphrase does not match the node passphrase already in use")
)

// Defaults for the tunable knobs. All of them are configurable through
// Options; none of them can be configured away entirely.
const (
	// DefaultIdleTimeout is how long an unwrapped key survives without a
	// Touch before it is zeroed. Zero means never, and zero is the default:
	// idle expiry is a capability this package offers, not a behaviour it
	// assumes.
	//
	// That default is a consequence of D-019, not an oversight, and it should
	// not be "fixed" back to a duration. A passphrase never travels over
	// Telegram, so a member has no in-band way to unlock again; the only
	// re-unlock path is somebody at the machine starting the process with the
	// passphrase. An idle timeout therefore does not degrade an idle member,
	// it breaks them — their assistant simply stops answering after a quiet
	// afternoon. Against that, the at-rest gain is marginal: the process is
	// still running and still holds whatever else it was unlocked with, and
	// the claim that survives is unchanged either way — nothing is readable
	// from a disk, from a backup, or from a process nobody has unlocked.
	DefaultIdleTimeout time.Duration = 0

	// DefaultMaxConcurrentDerivations bounds how many passphrase derivations
	// run at once. Each derivation allocates the KDF's memory cost (64 MiB at
	// keel/vault's defaults), so unbounded concurrency is a memory-exhaustion
	// lever pointed at the household's own node by anything that can cause an
	// unlock attempt. Two at a time is plenty for a household.
	DefaultMaxConcurrentDerivations = 2

	// DefaultFailureThreshold is how many consecutive unlock failures a
	// member id accumulates before attempts start being refused for a
	// backoff window.
	DefaultFailureThreshold = 3

	// DefaultBackoffBase is the first backoff window, doubling per further
	// failure up to DefaultBackoffCap.
	DefaultBackoffBase = 2 * time.Second

	// DefaultBackoffCap is the largest backoff window.
	DefaultBackoffCap = time.Minute
)

// derivEstimate is the delay a rate-limited attempt takes when no real
// derivation has been timed yet, so refusal does not announce itself by
// returning instantly.
const derivEstimate = 750 * time.Millisecond

// attemptRetention is how long failure bookkeeping for a member id outlives
// its last attempt before the sweeper forgets it.
const attemptRetention = 15 * time.Minute

// Option configures a Manager.
type Option func(*Manager) error

// WithIdleTimeout overrides DefaultIdleTimeout. Zero keeps idle expiry off; a
// positive duration turns it on and works exactly as it says.
//
// A household that sets one is choosing the trade knowingly, and the trade is
// this: after that much quiet, the member's key is zeroed and their assistant
// stops answering until someone at the machine starts the process again with
// the passphrase. There is no in-band way back — see DefaultIdleTimeout and
// D-019. A negative duration is a mistake rather than a shorthand for off, so
// it is rejected.
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) error {
		if d < 0 {
			return errors.New("session: idle timeout must not be negative; zero means no idle expiry")
		}
		m.idle = d
		return nil
	}
}

// WithKDFParams overrides keel/vault's default derivation cost for keys
// provisioned by this Manager. Unlocking always uses the parameters persisted
// alongside the wrapped key, so changing this never breaks existing members.
func WithKDFParams(p vault.KDFParams) Option {
	return func(m *Manager) error {
		cp := p
		m.kdf = &cp
		return nil
	}
}

// WithMaxConcurrentDerivations overrides DefaultMaxConcurrentDerivations.
func WithMaxConcurrentDerivations(n int) Option {
	return func(m *Manager) error {
		if n < 1 {
			return errors.New("session: derivation concurrency must be at least 1")
		}
		m.maxDeriv = n
		return nil
	}
}

// WithUnlockRateLimit overrides the failure threshold and the backoff window
// applied to repeated unlock failures for one member id.
func WithUnlockRateLimit(threshold int, base, cap time.Duration) Option {
	return func(m *Manager) error {
		if threshold < 1 {
			return errors.New("session: failure threshold must be at least 1")
		}
		if base <= 0 || cap < base {
			return errors.New("session: backoff must satisfy 0 < base <= cap")
		}
		m.threshold = threshold
		m.backoffBase = base
		m.backoffCap = cap
		return nil
	}
}

// unlocked is one member's live session: the unwrapped key and the last time
// anyone touched it.
type unlocked struct {
	key        []byte
	lastActive time.Time
}

// attemptState is the failure bookkeeping behind the unlock rate limit.
type attemptState struct {
	failures    int       // consecutive failed derivations
	nextAllowed time.Time // zero until failures reaches the threshold
	last        time.Time // last attempt of any kind, for pruning
}

// Manager implements Sessions on top of keel/vault, with wrapped keys
// persisted through a Store.
//
// Unwrapped keys live in this process's memory and nowhere else. They are
// zeroed on Lock, LockAll, Close and — only if a household configured an idle
// timeout, which is off by default — on idle expiry. Best effort, in the same
// sense keel/vault means it: Go's runtime may hold copies in registers, in
// stack frames it has moved, or in heap memory the collector has not reused,
// and none of that is reachable from here. Zeroing narrows the window in
// which a memory dump captures a key; it does not close it. The package
// documentation says what this is actually worth: an idle machine, a backup
// or a stolen disk yields nothing.
//
// Unlock failures are uniform on purpose. A wrong passphrase, an
// unprovisioned member, a corrupted record and a rate-limited attempt all
// return the identical ErrBadPassphrase value; the missing-member path burns
// a decoy derivation (inside keel/vault) and the rate-limited path waits
// roughly as long as a real derivation, so neither is trivially
// distinguishable by timing; and nothing is logged on any of those paths.
// The timing equivalence is best effort — it is closest when records use
// keel/vault's default derivation cost — and no stronger claim is made.
//
// Passphrase derivation is expensive by design (argon2id, 64 MiB at default
// cost), so the Manager never holds its lock across one: many members can
// read keys and reset timers while an unlock derives. Concurrent derivations
// are bounded by a semaphore, and repeated failures against one member id
// back off, because an unbounded derivation path is a memory-exhaustion
// lever.
//
// The zero value is not usable; call NewManager, and Close when done with it.
type Manager struct {
	mode     Mode
	store    Store
	idle     time.Duration
	kdf      *vault.KDFParams // nil means keel/vault defaults; used by Provision only
	maxDeriv int

	threshold   int
	backoffBase time.Duration
	backoffCap  time.Duration

	// sem bounds concurrent passphrase derivations. It is a semaphore, not a
	// mutex: it caps how many KDF runs exist at once without serialising the
	// Manager, and it is never held together with mu.
	sem chan struct{}

	// derivNanos is the duration of the most recent real derivation, feeding
	// the rate-limited path's decoy delay. derivActive and derivPeak observe
	// the semaphore from tests.
	derivNanos  atomic.Int64
	derivActive atomic.Int32
	derivPeak   atomic.Int32

	// mu guards everything below. Nothing slow ever runs under it.
	mu       sync.Mutex
	closed   bool
	sessions map[domain.MemberID]*unlocked
	attempts map[domain.MemberID]*attemptState

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// Manager implements Sessions.
var _ Sessions = (*Manager)(nil)

// NewManager returns a Manager in the given mode, persisting wrapped keys
// through store. It starts a sweeper goroutine that prunes failure bookkeeping
// and, where an idle timeout is configured, zeroes idle keys on its own
// schedule; Close stops it.
func NewManager(mode Mode, store Store, opts ...Option) (*Manager, error) {
	if !mode.valid() {
		return nil, fmt.Errorf("session: invalid mode %d", int(mode))
	}
	if store == nil {
		return nil, errors.New("session: nil store")
	}
	m := &Manager{
		mode:        mode,
		store:       store,
		idle:        DefaultIdleTimeout,
		maxDeriv:    DefaultMaxConcurrentDerivations,
		threshold:   DefaultFailureThreshold,
		backoffBase: DefaultBackoffBase,
		backoffCap:  DefaultBackoffCap,
		sessions:    make(map[domain.MemberID]*unlocked),
		attempts:    make(map[domain.MemberID]*attemptState),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}
	m.sem = make(chan struct{}, m.maxDeriv)
	go m.sweep(sweepInterval(m.idle))
	return m, nil
}

// Mode reports which mode the Manager was constructed in.
func (m *Manager) Mode() Mode { return m.mode }

// sweepInterval picks how often the sweeper wakes: often enough that a key
// does not outlive its idle window by much, rarely enough to cost nothing.
//
// With expiry off there is no window to honour, but the sweeper still runs —
// slowly — because it is also what prunes failure bookkeeping.
func sweepInterval(idle time.Duration) time.Duration {
	if idle <= 0 {
		return time.Minute
	}
	iv := idle / 8
	if iv < 10*time.Millisecond {
		iv = 10 * time.Millisecond
	}
	if iv > time.Minute {
		iv = time.Minute
	}
	return iv
}

// memberAAD is the additional authenticated data binding a sealed space key
// to one member's identity. The id passed here must always be the identity
// the caller asked for — the Unlock or Provision argument — and never one
// read out of a stored record: a record able to name itself would reopen the
// ciphertext-relocation hole this binding closes. The string is part of the
// persisted format; changing it strands every existing record.
func memberAAD(id domain.MemberID) []byte {
	return []byte("kenward/session: member space key v1\x00" + string(id))
}

// Unlock derives and unwraps the member's key. It is safe to call repeatedly;
// a successful call refreshes the key and resets the idle timer, and a failed
// call leaves any existing session untouched.
//
// Every failure that could reveal anything — wrong passphrase, unknown
// member, corrupted record, rate-limited attempt — returns the identical
// ErrBadPassphrase value. Only store I/O failures and context cancellation
// surface as themselves: both are node-level conditions, the same for every
// member id, and carry no per-member information.
func (m *Manager) Unlock(ctx context.Context, id domain.MemberID, passphrase string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	now := time.Now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	limited := false
	if a := m.attempts[id]; a != nil && now.Before(a.nextAllowed) {
		limited = true
		a.last = now
	}
	m.mu.Unlock()
	if limited {
		m.decoyDelay(ctx)
		return ErrBadPassphrase
	}

	rec, err := m.store.Load(ctx, id)
	missing := false
	if err != nil {
		if !errors.Is(err, ErrUnknownMember) {
			return fmt.Errorf("session: load wrapped key: %w", err)
		}
		missing = true
	}

	key, err := m.unwrap(ctx, id, rec, missing, passphrase)
	if err != nil {
		if errors.Is(err, ErrBadPassphrase) {
			m.recordFailure(id)
		}
		return err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		zeroBytes(key)
		return ErrManagerClosed
	}
	delete(m.attempts, id)
	if old := m.sessions[id]; old != nil {
		zeroBytes(old.key)
	}
	m.sessions[id] = &unlocked{key: key, lastActive: time.Now()}
	m.mu.Unlock()
	return nil
}

// unwrap runs the expensive part of an unlock — the passphrase derivation and
// the two envelope opens — under the derivation semaphore and under no other
// lock. All cryptographic failures collapse into ErrBadPassphrase here, with
// nothing logged and nothing wrapped, so the returned value is bit-identical
// whatever went wrong.
func (m *Manager) unwrap(ctx context.Context, id domain.MemberID, rec Record, missing bool, passphrase string) ([]byte, error) {
	if err := m.acquireDeriv(ctx); err != nil {
		return nil, err
	}
	defer m.releaseDeriv()

	start := time.Now()
	defer func() { m.derivNanos.Store(time.Since(start).Nanoseconds()) }()

	var kr vault.Keyring = recordKeyring{rec: rec.keyRecord()}
	if missing {
		// keel/vault's absent-keyring path burns a decoy derivation and
		// returns the same error as a wrong passphrase; lean on it rather
		// than reimplementing the timing discipline.
		kr = absentKeyring{}
	}
	v, err := vault.Open(ctx, kr, passphrase)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	defer v.Close()

	key, err := v.Open(memberAAD(id), rec.SealedKey)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return key, nil
}

// acquireDeriv takes a derivation slot, giving up if the context ends first.
func (m *Manager) acquireDeriv(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	n := m.derivActive.Add(1)
	for {
		p := m.derivPeak.Load()
		if n <= p || m.derivPeak.CompareAndSwap(p, n) {
			break
		}
	}
	return nil
}

func (m *Manager) releaseDeriv() {
	m.derivActive.Add(-1)
	<-m.sem
}

// decoyDelay makes a rate-limited attempt take roughly as long as a real one:
// the duration of the most recent derivation, or a fixed estimate before any
// has run. Returning instantly would tell a prober their attempts are being
// refused rather than failing, which is exactly the signal the rate limit
// must not add.
func (m *Manager) decoyDelay(ctx context.Context) {
	d := time.Duration(m.derivNanos.Load())
	if d <= 0 {
		d = derivEstimate
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// recordFailure counts a failed derivation against the member id and, past
// the threshold, opens a backoff window that doubles per further failure up
// to the cap. Attempts refused inside the window are not counted — they did
// no derivation and proved nothing.
func (m *Manager) recordFailure(id domain.MemberID) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.attempts[id]
	if a == nil {
		a = &attemptState{}
		m.attempts[id] = a
	}
	a.failures++
	a.last = now
	if a.failures >= m.threshold {
		exp := a.failures - m.threshold
		d := m.backoffCap
		if exp < 20 {
			if shifted := m.backoffBase << exp; shifted > 0 && shifted < d {
				d = shifted
			}
		}
		a.nextAllowed = now.Add(d)
	}
}

// Key returns a copy of the unwrapped key if the member has an active,
// unexpired session. The copy is the caller's to zero when the turn is done;
// the session's own buffer is what Lock and expiry zero, and handing out that
// buffer would race against them.
func (m *Manager) Key(id domain.MemberID) ([]byte, bool) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return nil, false
	}
	if m.expired(s, now) {
		m.drop(id, s)
		return nil, false
	}
	return append([]byte(nil), s.key...), true
}

// Touch resets the idle timer. Touching a locked or already-expired session
// does nothing; in particular it cannot resurrect a key whose window has
// lapsed.
func (m *Manager) Touch(id domain.MemberID) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return
	}
	if m.expired(s, now) {
		m.drop(id, s)
		return
	}
	s.lastActive = now
}

// Lock zeroes and forgets one member's key. Locking a member with no session
// is a no-op.
func (m *Manager) Lock(id domain.MemberID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		m.drop(id, s)
	}
}

// LockAll zeroes everything. Called on shutdown.
func (m *Manager) LockAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		m.drop(id, s)
	}
}

// Close stops the sweeper goroutine, waits for it to exit, and locks every
// session. It is idempotent; the Manager cannot be used afterwards.
func (m *Manager) Close() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.stop)
		<-m.done
	})
	m.LockAll()
}

// expired reports whether the session's idle window has lapsed. With no idle
// timeout configured nothing ever expires, which is the default. Callers hold mu.
func (m *Manager) expired(s *unlocked, now time.Time) bool {
	return m.idle > 0 && now.Sub(s.lastActive) >= m.idle
}

// drop zeroes a session's key and removes it. Callers hold mu.
func (m *Manager) drop(id domain.MemberID, s *unlocked) {
	zeroBytes(s.key)
	s.key = nil
	delete(m.sessions, id)
}

// sweep is the background expiry loop. Lazy checks in Key and Touch make
// expiry correct regardless; the sweeper exists so a key whose member simply
// went away is zeroed on schedule rather than lingering until someone asks.
// With no idle timeout configured expireIdle drops nothing and the loop only
// prunes failure bookkeeping.
func (m *Manager) sweep(interval time.Duration) {
	defer close(m.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.expireIdle(time.Now())
		}
	}
}

// expireIdle drops every session past its window and prunes stale failure
// bookkeeping. Entries still inside a backoff window are never pruned, so
// waiting out the sweeper does not reset a backoff.
func (m *Manager) expireIdle(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if m.expired(s, now) {
			m.drop(id, s)
		}
	}
	for id, a := range m.attempts {
		if now.Sub(a.last) >= attemptRetention && now.After(a.nextAllowed) {
			delete(m.attempts, id)
		}
	}
}

// Provision generates a member's space key, wraps it under the passphrase,
// binds it to the member's identity, and persists the wrapped record. The
// plaintext key exists only inside this call and is zeroed before it returns;
// nothing is left unlocked.
//
// In Simple mode the passphrase is the node passphrase, and Provision
// verifies it against an already-provisioned member before accepting it —
// returning ErrPassphraseMismatch otherwise — so "one node passphrase wraps
// every member's key" is enforced rather than assumed. In Isolated mode the
// passphrase is the member's own and no cross-member check exists.
//
// Provision is an operator- or enrolment-time surface, not a message-path
// one; unlike Unlock, its errors are allowed to be specific.
func (m *Manager) Provision(ctx context.Context, id domain.MemberID, passphrase string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return errors.New("session: empty member id")
	}
	if passphrase == "" {
		return errors.New("session: empty passphrase")
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrManagerClosed
	}

	if _, err := m.store.Load(ctx, id); err == nil {
		return ErrDuplicateMember
	} else if !errors.Is(err, ErrUnknownMember) {
		return fmt.Errorf("session: read key store: %w", err)
	}

	if m.mode == ModeSimple {
		if err := m.verifyNodePassphrase(ctx, passphrase); err != nil {
			return err
		}
	}

	spaceKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, spaceKey); err != nil {
		return fmt.Errorf("session: generate space key: %w", err)
	}
	defer zeroBytes(spaceKey)

	capture := &captureKeyring{}
	var opts []vault.Option
	if m.kdf != nil {
		opts = append(opts, vault.WithKDFParams(*m.kdf))
	}
	if err := m.acquireDeriv(ctx); err != nil {
		return err
	}
	v, err := vault.Init(ctx, capture, passphrase, opts...)
	m.releaseDeriv()
	if err != nil {
		return fmt.Errorf("session: wrap member key: %w", err)
	}
	defer v.Close()

	sealed, err := v.Seal(memberAAD(id), spaceKey)
	if err != nil {
		return fmt.Errorf("session: seal member key: %w", err)
	}

	rec := Record{
		Salt:       capture.rec.Salt,
		Params:     capture.rec.Params,
		WrappedDEK: capture.rec.WrappedKey,
		SealedKey:  sealed,
	}
	return m.store.Save(ctx, id, rec)
}

// verifyNodePassphrase checks, in Simple mode, that the passphrase offered
// for a new member opens a key already provisioned. With no members yet there
// is nothing to check and the first passphrase becomes the node passphrase.
func (m *Manager) verifyNodePassphrase(ctx context.Context, passphrase string) error {
	ids, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("session: list members: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	rec, err := m.store.Load(ctx, ids[0])
	if err != nil {
		return fmt.Errorf("session: load wrapped key: %w", err)
	}
	if err := m.acquireDeriv(ctx); err != nil {
		return err
	}
	v, err := vault.Open(ctx, recordKeyring{rec: rec.keyRecord()}, passphrase)
	m.releaseDeriv()
	if err != nil {
		return ErrPassphraseMismatch
	}
	v.Close()
	return nil
}

// CustodyReport is what `kenward doctor` prints about key custody: which mode
// the node runs in and which members hold wrapped keys. It contains no key
// material and no session state.
type CustodyReport struct {
	// Mode is the custody mode the Manager was constructed in.
	Mode Mode
	// Members lists every member holding a wrapped key, sorted.
	Members []domain.MemberID
}

// Custody reports, truthfully, who can unlock what.
func (m *Manager) Custody(ctx context.Context) (CustodyReport, error) {
	ids, err := m.store.List(ctx)
	if err != nil {
		return CustodyReport{}, fmt.Errorf("session: list members: %w", err)
	}
	slices.Sort(ids)
	return CustodyReport{Mode: m.mode, Members: ids}, nil
}

// String renders the report in doctor's voice. Simple mode's wording states
// the operator's reach plainly rather than borrowing sealed-memory language
// it has not earned; Isolated mode's wording still concedes that an unlocked
// key sits in process memory for the length of a session.
func (r CustodyReport) String() string {
	names := make([]string, len(r.Members))
	for i, id := range r.Members {
		names[i] = string(id)
	}
	list := strings.Join(names, ", ")
	if list == "" {
		list = "none yet"
	}
	switch r.Mode {
	case ModeSimple:
		return fmt.Sprintf("key custody: simple mode. One node passphrase, held by the operator, wraps every member's key (members: %s). The operator can unlock any member's key and with it that member's private memory. That is this mode's stated limitation, not a bug.", list)
	case ModeIsolated:
		return fmt.Sprintf("key custody: isolated mode. Each member's key (members: %s) is wrapped by that member's own passphrase; no single passphrase unlocks more than one member. While a member is in session their unwrapped key is in that pod's memory — root on the host still wins.", list)
	default:
		return fmt.Sprintf("key custody: unknown mode %d", int(r.Mode))
	}
}

// recordKeyring hands keel/vault a record already loaded from the Store. It
// is read-only: this package never calls vault.Rotate, and a Save reaching it
// would mean key material moving somewhere the Store cannot see.
type recordKeyring struct{ rec vault.KeyRecord }

// Load returns the wrapped record.
func (k recordKeyring) Load(context.Context) (vault.KeyRecord, error) { return k.rec, nil }

// Save refuses; see the type comment.
func (k recordKeyring) Save(context.Context, vault.KeyRecord) error {
	return errors.New("session: keyring is read-only")
}

// absentKeyring reports no key record, which steers vault.Open onto its
// decoy-derivation path: same error as a wrong passphrase, comparable time.
type absentKeyring struct{}

// Load reports that no record exists.
func (absentKeyring) Load(context.Context) (vault.KeyRecord, error) {
	return vault.KeyRecord{}, vault.ErrNoKey
}

// Save refuses; nothing should ever be written through this keyring.
func (absentKeyring) Save(context.Context, vault.KeyRecord) error {
	return errors.New("session: keyring is read-only")
}

// captureKeyring collects the KeyRecord that vault.Init produces so Provision
// can persist it through the Store instead.
type captureKeyring struct{ rec vault.KeyRecord }

// Load reports empty so vault.Init proceeds.
func (k *captureKeyring) Load(context.Context) (vault.KeyRecord, error) {
	return vault.KeyRecord{}, vault.ErrNoKey
}

// Save captures the record.
func (k *captureKeyring) Save(_ context.Context, rec vault.KeyRecord) error {
	k.rec = rec
	return nil
}

// zeroBytes best-effort erases key material, in the same spirit and with the
// same caveats as keel/vault's helper: the KeepAlive discourages the compiler
// from dropping the writes, but Go guarantees nothing about copies the
// runtime has already made.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}
