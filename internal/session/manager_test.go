package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// fastKDF keeps tests quick. Unlocking always uses the parameters persisted
// with the record, so nothing here weakens what production provisions.
func fastKDF() vault.KDFParams { return vault.KDFParams{Time: 1, MemoryKiB: 8, Threads: 1} }

func newTestManager(t *testing.T, mode Mode, st Store, opts ...Option) *Manager {
	t.Helper()
	m, err := NewManager(mode, st, append([]Option{WithKDFParams(fastKDF())}, opts...)...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func mustProvision(t *testing.T, m *Manager, id domain.MemberID, pass string) {
	t.Helper()
	if err := m.Provision(context.Background(), id, pass); err != nil {
		t.Fatalf("Provision %s: %v", id, err)
	}
}

func TestUnlockKeyLockRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	mustProvision(t, m, "ada", "correct horse")

	if _, ok := m.Key("ada"); ok {
		t.Fatal("key obtainable before unlock")
	}
	if err := m.Unlock(ctx, "ada", "correct horse"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	key, ok := m.Key("ada")
	if !ok || len(key) != 32 {
		t.Fatalf("Key after unlock: ok=%v len=%d", ok, len(key))
	}

	// Unlock is safe to call repeatedly and yields the same key.
	if err := m.Unlock(ctx, "ada", "correct horse"); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
	again, ok := m.Key("ada")
	if !ok || !bytes.Equal(key, again) {
		t.Fatal("repeated unlock changed the key")
	}

	// A failed unlock leaves the live session untouched.
	if err := m.Unlock(ctx, "ada", "wrong"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: got %v", err)
	}
	if _, ok := m.Key("ada"); !ok {
		t.Fatal("failed unlock killed the live session")
	}

	m.Lock("ada")
	if _, ok := m.Key("ada"); ok {
		t.Fatal("key obtainable after Lock")
	}
}

func TestKeyReturnsCopy(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	mustProvision(t, m, "ada", "pw")
	if err := m.Unlock(ctx, "ada", "pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	k1, _ := m.Key("ada")
	orig := append([]byte(nil), k1...)
	for i := range k1 {
		k1[i] = 0
	}
	k2, ok := m.Key("ada")
	if !ok || !bytes.Equal(k2, orig) {
		t.Fatal("mutating a returned key reached the session's buffer")
	}
}

func TestWrongPassphraseAndMissingMemberIndistinguishable(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	mustProvision(t, m, "ada", "pw")

	errWrong := m.Unlock(ctx, "ada", "not the passphrase")
	errMissing := m.Unlock(ctx, "nobody", "not the passphrase")

	if !errors.Is(errWrong, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: got %v", errWrong)
	}
	if !errors.Is(errMissing, ErrBadPassphrase) {
		t.Fatalf("missing member: got %v", errMissing)
	}
	// Not merely equivalent: the identical error value, so no message or wrap
	// chain can tell them apart.
	if errWrong != errMissing {
		t.Fatalf("errors distinguishable: %#v vs %#v", errWrong, errMissing)
	}
}

// TestRelocationRejected is the AAD test: a wrapped record copied from one
// member's slot into another's must not open, even under the passphrase that
// legitimately opens it in its own slot.
func TestRelocationRejected(t *testing.T) {
	ctx := context.Background()
	src := NewMemStore()
	m := newTestManager(t, ModeSimple, src, WithUnlockRateLimit(100, time.Millisecond, time.Millisecond))
	mustProvision(t, m, "ada", "node pass")
	mustProvision(t, m, "bob", "node pass")

	// Control: both records open in their own slots.
	if err := m.Unlock(ctx, "ada", "node pass"); err != nil {
		t.Fatalf("control unlock ada: %v", err)
	}
	if err := m.Unlock(ctx, "bob", "node pass"); err != nil {
		t.Fatalf("control unlock bob: %v", err)
	}

	// Attack: ada's sealed record placed in bob's slot.
	recA, err := src.Load(ctx, "ada")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	forged := NewMemStore()
	if err := forged.Save(ctx, "bob", recA); err != nil {
		t.Fatalf("Save forged: %v", err)
	}
	m2 := newTestManager(t, ModeSimple, forged)
	if err := m2.Unlock(ctx, "bob", "node pass"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("relocated record opened as bob: %v", err)
	}
	if _, ok := m2.Key("bob"); ok {
		t.Fatal("relocated record yielded a key")
	}

	// Control for the control: the same record in the right slot still opens.
	rightful := NewMemStore()
	if err := rightful.Save(ctx, "ada", recA); err != nil {
		t.Fatalf("Save rightful: %v", err)
	}
	m3 := newTestManager(t, ModeSimple, rightful)
	if err := m3.Unlock(ctx, "ada", "node pass"); err != nil {
		t.Fatalf("record failed in its own slot: %v", err)
	}
}

// TestFileRelocationRejected replays the relocation attack at the file level:
// the ids in the persisted file are swapped, so each record's embedded
// identity disagrees with the identity it was sealed under. Nothing the file
// says about itself may win over the identity the caller asked for.
func TestFileRelocationRejected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")
	m := newTestManager(t, ModeSimple, NewFileStore(path), WithUnlockRateLimit(100, time.Millisecond, time.Millisecond))
	mustProvision(t, m, "ada", "node pass")
	mustProvision(t, m, "bob", "node pass")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var f keyFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(f.Members))
	}
	f.Members[0].ID, f.Members[1].ID = f.Members[1].ID, f.Members[0].ID
	swapped, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := m.Unlock(ctx, "ada", "node pass"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("swapped record opened as ada: %v", err)
	}
	if err := m.Unlock(ctx, "bob", "node pass"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("swapped record opened as bob: %v", err)
	}
}

func TestIdleExpirySweepsWithoutAccess(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore(), WithIdleTimeout(100*time.Millisecond))
	mustProvision(t, m, "ada", "pw")
	if err := m.Unlock(ctx, "ada", "pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Grab the session's own buffer so the zeroing is observable.
	m.mu.Lock()
	buf := m.sessions["ada"].key
	m.mu.Unlock()

	// Never call Key or Touch: the sweeper alone must expire it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		_, alive := m.sessions["ada"]
		m.mu.Unlock()
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session survived its idle window with nobody asking")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("key byte %d not zeroed on expiry", i)
		}
	}
	if _, ok := m.Key("ada"); ok {
		t.Fatal("key obtainable after expiry")
	}
}

func TestTouchResetsIdleTimer(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore(), WithIdleTimeout(600*time.Millisecond))
	mustProvision(t, m, "ada", "pw")
	if err := m.Unlock(ctx, "ada", "pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Keep touching well past the window; the session must survive.
	for i := 0; i < 5; i++ {
		time.Sleep(200 * time.Millisecond)
		m.Touch("ada")
	}
	if _, ok := m.Key("ada"); !ok {
		t.Fatal("session expired despite being touched")
	}

	// Stop touching; it must die.
	time.Sleep(900 * time.Millisecond)
	if _, ok := m.Key("ada"); ok {
		t.Fatal("session survived after touches stopped")
	}
}

func TestLockAllAndClose(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	for _, id := range []domain.MemberID{"a", "b", "c"} {
		mustProvision(t, m, id, "pw-"+string(id))
		if err := m.Unlock(ctx, id, "pw-"+string(id)); err != nil {
			t.Fatalf("Unlock %s: %v", id, err)
		}
	}
	m.LockAll()
	for _, id := range []domain.MemberID{"a", "b", "c"} {
		if _, ok := m.Key(id); ok {
			t.Fatalf("key %s obtainable after LockAll", id)
		}
	}

	if err := m.Unlock(ctx, "a", "pw-a"); err != nil {
		t.Fatalf("re-unlock after LockAll: %v", err)
	}
	m.Close()
	if _, ok := m.Key("a"); ok {
		t.Fatal("key obtainable after Close")
	}
	if err := m.Unlock(ctx, "a", "pw-a"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Unlock after Close: %v", err)
	}
	if err := m.Provision(ctx, "d", "pw"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Provision after Close: %v", err)
	}
	m.Close() // idempotent
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	st := NewMemStore()
	m, err := NewManager(ModeIsolated, st, WithKDFParams(fastKDF()), WithIdleTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Provision(context.Background(), "ada", "pw"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := m.Unlock(context.Background(), "ada", "pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	m.Close()

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after Close", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSimpleModeEnforcesOneNodePassphrase(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeSimple, NewMemStore())
	mustProvision(t, m, "ada", "the node pass")

	if err := m.Provision(ctx, "bob", "a different pass"); !errors.Is(err, ErrPassphraseMismatch) {
		t.Fatalf("divergent passphrase accepted in simple mode: %v", err)
	}
	if _, err := m.store.Load(ctx, "bob"); !errors.Is(err, ErrUnknownMember) {
		t.Fatal("refused provision left a record behind")
	}
	mustProvision(t, m, "bob", "the node pass")

	// The node passphrase unlocks everyone — the mode's stated limitation.
	for _, id := range []domain.MemberID{"ada", "bob"} {
		if err := m.Unlock(ctx, id, "the node pass"); err != nil {
			t.Fatalf("node passphrase failed to unlock %s: %v", id, err)
		}
	}
}

func TestIsolatedModePassphrasesAreIndependent(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	mustProvision(t, m, "ada", "ada's pass")
	mustProvision(t, m, "bob", "bob's pass") // no mismatch check in this mode

	if err := m.Unlock(ctx, "ada", "bob's pass"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("bob's passphrase opened ada's key: %v", err)
	}
	if err := m.Unlock(ctx, "ada", "ada's pass"); err != nil {
		t.Fatalf("Unlock ada: %v", err)
	}
}

func TestProvisionValidation(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore())
	if err := m.Provision(ctx, "", "pw"); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := m.Provision(ctx, "ada", ""); err == nil {
		t.Fatal("empty passphrase accepted")
	}
	mustProvision(t, m, "ada", "pw")
	if err := m.Provision(ctx, "ada", "pw"); !errors.Is(err, ErrDuplicateMember) {
		t.Fatalf("duplicate provision: got %v", err)
	}
}

func TestUnlockRateLimitIsIndistinguishable(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore(),
		WithUnlockRateLimit(2, 400*time.Millisecond, time.Second))
	mustProvision(t, m, "ada", "right")

	// Two failures reach the threshold and open a backoff window.
	for i := 0; i < 2; i++ {
		if err := m.Unlock(ctx, "ada", "wrong"); !errors.Is(err, ErrBadPassphrase) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}

	// Inside the window even the CORRECT passphrase is refused, with the
	// identical error value a wrong passphrase produces.
	errLimited := m.Unlock(ctx, "ada", "right")
	if errLimited != ErrBadPassphrase {
		t.Fatalf("rate-limited attempt: got %#v, want the bare ErrBadPassphrase", errLimited)
	}
	if _, ok := m.Key("ada"); ok {
		t.Fatal("rate-limited attempt yielded a key")
	}

	// After the window the correct passphrase works and resets the count.
	time.Sleep(600 * time.Millisecond)
	if err := m.Unlock(ctx, "ada", "right"); err != nil {
		t.Fatalf("Unlock after backoff: %v", err)
	}
	m.mu.Lock()
	_, tracked := m.attempts["ada"]
	m.mu.Unlock()
	if tracked {
		t.Fatal("success did not reset the failure count")
	}
}

func TestDerivationConcurrencyIsBounded(t *testing.T) {
	ctx := context.Background()
	const limit = 2
	m := newTestManager(t, ModeIsolated, NewMemStore(),
		WithMaxConcurrentDerivations(limit),
		// Heavy enough that derivations overlap if allowed to.
		WithKDFParams(vault.KDFParams{Time: 2, MemoryKiB: 4096, Threads: 1}))

	const members = 8
	for i := 0; i < members; i++ {
		mustProvision(t, m, domain.MemberID(fmt.Sprintf("m%d", i)), "pw")
	}
	m.derivPeak.Store(0) // ignore provisioning; measure the burst below

	var wg sync.WaitGroup
	errs := make(chan error, members)
	for i := 0; i < members; i++ {
		wg.Add(1)
		go func(id domain.MemberID) {
			defer wg.Done()
			errs <- m.Unlock(ctx, id, "pw")
		}(domain.MemberID(fmt.Sprintf("m%d", i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Unlock: %v", err)
		}
	}
	peak := m.derivPeak.Load()
	if peak > limit {
		t.Fatalf("derivation concurrency reached %d, limit %d", peak, limit)
	}
	if peak < 1 {
		t.Fatal("no derivation observed; the gauge is broken")
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, ModeIsolated, NewMemStore(), WithIdleTimeout(5*time.Second))

	const members = 6
	ids := make([]domain.MemberID, members)
	expect := make(map[domain.MemberID][]byte, members)
	for i := range ids {
		ids[i] = domain.MemberID(fmt.Sprintf("m%d", i))
		mustProvision(t, m, ids[i], "pw")
		if err := m.Unlock(ctx, ids[i], "pw"); err != nil {
			t.Fatalf("Unlock: %v", err)
		}
		k, ok := m.Key(ids[i])
		if !ok {
			t.Fatal("no key after unlock")
		}
		expect[ids[i]] = k
	}

	var wg sync.WaitGroup
	fail := make(chan string, 128)
	for _, id := range ids {
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func(id domain.MemberID, g int) {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					switch (g + i) % 4 {
					case 0:
						if err := m.Unlock(ctx, id, "pw"); err != nil {
							select {
							case fail <- fmt.Sprintf("unlock %s: %v", id, err):
							default:
							}
							return
						}
					case 1:
						if k, ok := m.Key(id); ok && !bytes.Equal(k, expect[id]) {
							select {
							case fail <- fmt.Sprintf("key mismatch for %s", id):
							default:
							}
							return
						}
					case 2:
						m.Touch(id)
					case 3:
						m.Lock(id)
					}
				}
			}(id, g)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			m.LockAll()
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	select {
	case msg := <-fail:
		t.Fatal(msg)
	default:
	}
}

func TestCustodyReportsTruthfully(t *testing.T) {
	ctx := context.Background()

	simple := newTestManager(t, ModeSimple, NewMemStore())
	mustProvision(t, simple, "bea", "node pass")
	mustProvision(t, simple, "ada", "node pass")
	rpt, err := simple.Custody(ctx)
	if err != nil {
		t.Fatalf("Custody: %v", err)
	}
	if rpt.Mode != ModeSimple || len(rpt.Members) != 2 || rpt.Members[0] != "ada" || rpt.Members[1] != "bea" {
		t.Fatalf("simple report: %+v", rpt)
	}
	s := rpt.String()
	for _, want := range []string{"simple mode", "operator", "any member", "stated limitation", "ada", "bea"} {
		if !strings.Contains(s, want) {
			t.Fatalf("simple custody text missing %q: %s", want, s)
		}
	}

	iso := newTestManager(t, ModeIsolated, NewMemStore())
	mustProvision(t, iso, "ada", "ada's pass")
	rpt, err = iso.Custody(ctx)
	if err != nil {
		t.Fatalf("Custody: %v", err)
	}
	s = rpt.String()
	for _, want := range []string{"isolated mode", "own passphrase", "memory"} {
		if !strings.Contains(s, want) {
			t.Fatalf("isolated custody text missing %q: %s", want, s)
		}
	}

	empty := newTestManager(t, ModeIsolated, NewMemStore())
	rpt, _ = empty.Custody(ctx)
	if !strings.Contains(rpt.String(), "none yet") {
		t.Fatalf("empty custody text: %s", rpt.String())
	}
}

// TestPlaintextKeyNeverPersisted reads the raw bytes on disk and asserts the
// unwrapped key appears nowhere in them, raw or in the file's own encoding.
func TestPlaintextKeyNeverPersisted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")
	m := newTestManager(t, ModeIsolated, NewFileStore(path))
	mustProvision(t, m, "ada", "pw")
	if err := m.Unlock(ctx, "ada", "pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	key, ok := m.Key("ada")
	if !ok {
		t.Fatal("no key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(raw, key) {
		t.Fatal("unwrapped key present in the store file")
	}
	if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(key))) {
		t.Fatal("unwrapped key present base64-encoded in the store file")
	}
}

func TestNewManagerValidation(t *testing.T) {
	if _, err := NewManager(Mode(0), NewMemStore()); err == nil {
		t.Fatal("zero mode accepted")
	}
	if _, err := NewManager(ModeSimple, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewManager(ModeSimple, NewMemStore(), WithIdleTimeout(0)); err == nil {
		t.Fatal("zero idle timeout accepted")
	}
	if _, err := NewManager(ModeSimple, NewMemStore(), WithMaxConcurrentDerivations(0)); err == nil {
		t.Fatal("zero derivation concurrency accepted")
	}
	if _, err := NewManager(ModeSimple, NewMemStore(), WithUnlockRateLimit(0, time.Second, time.Second)); err == nil {
		t.Fatal("zero failure threshold accepted")
	}
	if _, err := NewManager(ModeSimple, NewMemStore(), WithUnlockRateLimit(3, time.Second, time.Millisecond)); err == nil {
		t.Fatal("cap below base accepted")
	}
}
