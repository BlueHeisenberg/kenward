// Package dashboard is kenward's admin dashboard: one HTTP server, one account, and
// no port at all unless a household said otherwise.
//
// # What this is for
//
// Configuring kenward used to mean hand-editing YAML and creating lore spaces from a
// command line. That is a wall, and it is a wall in front of the only person the product
// is for — the operator stopped being assumed to be the author. This package is the door
// through it: a first-run wizard, member management that creates the lore space and mints
// the claim code in one action, and an editor for every setting the wizard writes.
//
// # The rules it is built to
//
//   - One account. Household members are unchanged: a Telegram id bound by a claim code,
//     no login, no password, no reset flow. Nothing here issues a member a credential.
//   - Loopback by default. The server does not exist unless configured, and when it does
//     it is on 127.0.0.1 until somebody chose otherwise in as many words.
//   - Setup happens before an account exists, on loopback, behind a single-use token the
//     process prints. The token is destroyed the instant it is exchanged, and the setup
//     session it produced is destroyed the instant the admin account exists.
//   - Exposure is chosen after the account exists, never before. LAN requires TLS.
//   - Server-rendered HTML. No SPA, no build step, no Node toolchain to fix a form.
//
// # Threat model
//
// This box holds every member's private memory, and the admin password is the whole of
// what stands between a browser and the setting that decides which conversations may
// leave the house. So the server assumes an attacker who can reach the port: every route
// but the login and setup pages is refused without a session, every mutating request
// carries a per-session CSRF token compared in constant time, the login path is rate
// limited and locks out, and a session id is never reused across an authentication
// boundary.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BlueHeisenberg/keel/vault"
)

// DirName is the directory under the data directory where the dashboard keeps what it
// owns: the admin key record, the outstanding setup token, and any self-signed
// certificate it generated.
const DirName = "dashboard"

const (
	adminFileName      = "admin.json"
	setupTokenFileName = "setup-token.json"
)

// fileMode is what everything this package writes is created with. The admin record is
// a wrapped key and the setup token file is a live credential digest; no other account
// on the machine has any business reading either.
const fileMode = 0o600

// ErrNoAdmin is returned when no admin account has been created yet. It is the state
// every fresh install is in, and the only state in which the setup routes exist.
var ErrNoAdmin = errors.New("dashboard: no admin account")

// ErrBadPassword is returned for a password that does not open the admin record.
//
// It is one value for a wrong password and for an absent record together, because
// keel/vault returns one value for them: distinguishing the two here would hand an
// unauthenticated caller the answer to "is this install configured yet", which is the
// first thing worth knowing about a box you have just found.
var ErrBadPassword = errors.New("dashboard: password rejected")

// MinPasswordLength is the shortest admin password the dashboard will set.
//
// Twelve, and no composition rules. Argon2id at keel/vault's default cost makes an
// offline guess expensive; what it cannot repair is a short password, and what
// composition rules reliably produce is Password1! written on a sticky note. Length is
// the only requirement that buys anything.
const MinPasswordLength = 12

// adminRecord is the admin account on disk: keel/vault's key record and nothing else.
//
// There is no password hash here, because there is no password hash. The account is a
// keel/vault vault whose data key is wrapped under the admin's passphrase, exactly as
// every member's session key already is (internal/session). Checking a password is
// unwrapping it; a wrong one produces vault.ErrBadPassphrase and no distinguishable
// timing, and the Argon2id parameters travel in the record so raising the cost later is
// a rotation rather than a migration. Writing a second password-hashing scheme beside
// the one this product already depends on would be two things to get right.
type adminRecord struct {
	// Version is the record's own schema version, so that a future change is a code
	// path rather than a guess about what these fields mean.
	Version int `json:"version"`
	// ID, Salt, Params and WrappedKey are keel/vault's KeyRecord, base64 where the
	// field is bytes. They are opaque here: this package never interprets them.
	ID         string `json:"id"`
	Salt       string `json:"salt"`
	Params     string `json:"params"`
	WrappedKey string `json:"wrapped_key"`
	// CreatedAt is when the account was made. It is shown on the overview so an
	// operator can tell an account they made from one somebody else did.
	CreatedAt time.Time `json:"created_at"`
}

const adminRecordVersion = 1

// AdminStore is the admin account, persisted under the data directory.
//
// It is a value rather than an interface: there is one implementation, and a second one
// would be a file in a different place.
type AdminStore struct{ dir string }

// NewAdminStore returns the store rooted at the dashboard directory under dataDir.
func NewAdminStore(dataDir string) *AdminStore {
	return &AdminStore{dir: filepath.Join(dataDir, DirName)}
}

func (s *AdminStore) path() string { return filepath.Join(s.dir, adminFileName) }

// Exists reports whether an admin account has been created.
//
// It is the question the setup routes turn on, and it is answered from the filesystem
// on every call rather than cached. A cached answer is a window in which the setup
// wizard is still reachable after an account exists, and that window is the one thing
// the single-use token is for.
func (s *AdminStore) Exists() bool {
	_, err := os.Stat(s.path())
	return err == nil
}

// Create makes the admin account. It refuses to replace one that already exists: there
// is no reset flow, deliberately, and an endpoint that could overwrite the account would
// be one.
func (s *AdminStore) Create(ctx context.Context, password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return fmt.Errorf("dashboard: an admin password needs at least %d characters", MinPasswordLength)
	}
	if s.Exists() {
		return errors.New("dashboard: an admin account already exists")
	}

	var kr captureKeyring
	v, err := vault.Init(ctx, &kr, password)
	if err != nil {
		return fmt.Errorf("dashboard: creating the admin account: %w", err)
	}
	// Nothing is sealed under this vault. It exists for its key record, which is the
	// password verifier; holding it open afterwards would keep a data key in memory
	// for no purpose.
	v.Close()

	rec := adminRecord{
		Version:    adminRecordVersion,
		ID:         kr.rec.ID,
		Salt:       base64.StdEncoding.EncodeToString(kr.rec.Salt),
		Params:     base64.StdEncoding.EncodeToString(kr.rec.Params),
		WrappedKey: base64.StdEncoding.EncodeToString(kr.rec.WrappedKey),
		CreatedAt:  time.Now().UTC(),
	}
	return writeJSON(s.path(), rec)
}

// Verify reports whether password opens the admin account.
//
// Every failure is ErrBadPassword, including "there is no account". keel/vault burns a
// decoy derivation on the absent-record path so the two take comparable time, and
// nothing here adds a distinguishing branch after it.
func (s *AdminStore) Verify(ctx context.Context, password string) error {
	rec, err := s.load()
	switch {
	case errors.Is(err, ErrNoAdmin):
		// Straight through to vault.Open with an empty keyring, so the decoy
		// derivation happens and the timing of "no account" resembles the timing of
		// "wrong password". Returning early here would make the two trivially
		// distinguishable to anyone with a stopwatch.
		if _, err := vault.Open(ctx, absentKeyring{}, password); err != nil {
			return ErrBadPassword
		}
		return ErrBadPassword
	case err != nil:
		return err
	}

	v, err := vault.Open(ctx, recordKeyring{rec: rec}, password)
	if err != nil {
		return ErrBadPassword
	}
	v.Close()
	return nil
}

// CreatedAt reports when the account was made.
func (s *AdminStore) CreatedAt() (time.Time, error) {
	var rec adminRecord
	if err := readJSON(s.path(), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, ErrNoAdmin
		}
		return time.Time{}, err
	}
	return rec.CreatedAt, nil
}

// ChangePassword re-wraps the account's data key under a new passphrase, after checking
// the old one. It is a rotation rather than a delete-and-recreate for the reason
// keel/vault offers one: the record keeps its identity and the old passphrase stops
// working in the same write.
func (s *AdminStore) ChangePassword(ctx context.Context, old, next string) error {
	if len([]rune(next)) < MinPasswordLength {
		return fmt.Errorf("dashboard: an admin password needs at least %d characters", MinPasswordLength)
	}
	rec, err := s.load()
	if err != nil {
		return err
	}
	kr := &rotatingKeyring{rec: rec}
	v, err := vault.Open(ctx, kr, old)
	if err != nil {
		return ErrBadPassword
	}
	defer v.Close()
	if err := v.Rotate(ctx, old, next); err != nil {
		return fmt.Errorf("dashboard: changing the admin password: %w", err)
	}
	return writeJSON(s.path(), adminRecord{
		Version:    adminRecordVersion,
		ID:         kr.rec.ID,
		Salt:       base64.StdEncoding.EncodeToString(kr.rec.Salt),
		Params:     base64.StdEncoding.EncodeToString(kr.rec.Params),
		WrappedKey: base64.StdEncoding.EncodeToString(kr.rec.WrappedKey),
		CreatedAt:  time.Now().UTC(),
	})
}

func (s *AdminStore) load() (vault.KeyRecord, error) {
	var rec adminRecord
	if err := readJSON(s.path(), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vault.KeyRecord{}, ErrNoAdmin
		}
		return vault.KeyRecord{}, err
	}
	if rec.Version != adminRecordVersion {
		return vault.KeyRecord{}, fmt.Errorf("dashboard: %s is version %d and this build understands version %d", s.path(), rec.Version, adminRecordVersion)
	}
	salt, err := base64.StdEncoding.DecodeString(rec.Salt)
	if err != nil {
		return vault.KeyRecord{}, fmt.Errorf("dashboard: %s: salt: %w", s.path(), err)
	}
	params, err := base64.StdEncoding.DecodeString(rec.Params)
	if err != nil {
		return vault.KeyRecord{}, fmt.Errorf("dashboard: %s: params: %w", s.path(), err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(rec.WrappedKey)
	if err != nil {
		return vault.KeyRecord{}, fmt.Errorf("dashboard: %s: wrapped key: %w", s.path(), err)
	}
	return vault.KeyRecord{ID: rec.ID, Salt: salt, Params: params, WrappedKey: wrapped}, nil
}

// captureKeyring collects the KeyRecord vault.Init produces. Load reports empty so Init
// proceeds. It mirrors internal/session's helper of the same shape, deliberately: two
// packages holding a keel/vault record should hold it the same way.
type captureKeyring struct{ rec vault.KeyRecord }

func (k *captureKeyring) Load(context.Context) (vault.KeyRecord, error) {
	return vault.KeyRecord{}, vault.ErrNoKey
}

func (k *captureKeyring) Save(_ context.Context, rec vault.KeyRecord) error {
	k.rec = rec
	return nil
}

// recordKeyring hands keel/vault a record already read from disk. Read-only: Verify
// never rotates, and a Save reaching it would be a bug worth failing on.
type recordKeyring struct{ rec vault.KeyRecord }

func (k recordKeyring) Load(context.Context) (vault.KeyRecord, error) { return k.rec, nil }

func (k recordKeyring) Save(context.Context, vault.KeyRecord) error {
	return errors.New("dashboard: the admin key record is read-only on this path")
}

// rotatingKeyring is recordKeyring that accepts the rewrite Rotate performs, so the new
// record can be persisted afterwards.
type rotatingKeyring struct{ rec vault.KeyRecord }

func (k *rotatingKeyring) Load(context.Context) (vault.KeyRecord, error) { return k.rec, nil }

func (k *rotatingKeyring) Save(_ context.Context, rec vault.KeyRecord) error {
	k.rec = rec
	return nil
}

// absentKeyring reports no record, which steers vault.Open onto its decoy-derivation
// path so an absent account costs the same as a wrong password.
type absentKeyring struct{}

func (absentKeyring) Load(context.Context) (vault.KeyRecord, error) {
	return vault.KeyRecord{}, vault.ErrNoKey
}

func (absentKeyring) Save(context.Context, vault.KeyRecord) error {
	return errors.New("dashboard: no keyring")
}

// SetupTokenTTL is how long a setup token stays redeemable.
//
// Thirty minutes: long enough to walk to another room and open a browser, short enough
// that a token left in a terminal's scrollback over a weekend is worth nothing. It is
// reissued with `kenward setup-token`, which costs one command, so there is no reason
// for it to be generous.
const SetupTokenTTL = 30 * time.Minute

// setupTokenRecord is the outstanding setup token: its digest and when it stops working.
//
// The token itself is never stored. It exists in the process's output and in whatever
// the operator pasted it into, and the file holds only enough to recognise it — which is
// what makes a stolen copy of this file useless for getting in.
type setupTokenRecord struct {
	// Digest is the SHA-256 of the token, hex. A digest and not a slow hash, and
	// deliberately: this is a 256-bit random value with a thirty-minute life, not a
	// human-chosen password, so there is nothing for a slow KDF to defend.
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expires_at"`
	IssuedAt  time.Time `json:"issued_at"`
}

// SetupTokenStore holds the one outstanding setup token.
//
// It is on disk rather than in the server's memory because `kenward setup-token` is a
// different process from the one serving the dashboard: a token only the server knew
// could not be reissued without restarting the household.
type SetupTokenStore struct{ dir string }

// NewSetupTokenStore returns the store under dataDir.
func NewSetupTokenStore(dataDir string) *SetupTokenStore {
	return &SetupTokenStore{dir: filepath.Join(dataDir, DirName)}
}

func (s *SetupTokenStore) path() string { return filepath.Join(s.dir, setupTokenFileName) }

// Issue mints a token, records its digest, and returns the token itself. Whatever token
// was outstanding stops working: there is one, and reissuing replaces it.
func (s *SetupTokenStore) Issue(now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("dashboard: minting a setup token: %w", err)
	}
	// base64url with no padding: it goes in a URL bar and a terminal, and a token an
	// operator has to escape is a token they will mistype.
	token := base64.RawURLEncoding.EncodeToString(raw)
	rec := setupTokenRecord{
		Digest:    digestOf(token),
		IssuedAt:  now.UTC(),
		ExpiresAt: now.Add(SetupTokenTTL).UTC(),
	}
	if err := writeJSON(s.path(), rec); err != nil {
		return "", err
	}
	return token, nil
}

// Redeem checks a token and destroys it. It is single-use in the strongest sense
// available: the file is removed before this returns, so a second presentation of the
// same token — and every other token, since there is only ever one — finds nothing.
//
// A failure removes nothing. A wrong guess must not be able to invalidate the real
// operator's token, or an attacker who can reach the port can lock the household out of
// its own first run by guessing once.
func (s *SetupTokenStore) Redeem(token string, now time.Time) error {
	var rec setupTokenRecord
	if err := readJSON(s.path(), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("dashboard: there is no setup token outstanding; reissue one with `kenward setup-token`")
		}
		return err
	}
	if now.After(rec.ExpiresAt) {
		// Expired tokens are cleaned up: this one is not going to be used, and
		// leaving it invites the next reader to think one is outstanding.
		_ = os.Remove(s.path())
		return errors.New("dashboard: that setup token has expired; reissue one with `kenward setup-token`")
	}
	if subtle.ConstantTimeCompare([]byte(digestOf(token)), []byte(rec.Digest)) != 1 {
		return errors.New("dashboard: that is not the setup token")
	}
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// The token would still be redeemable, so this is a refusal rather than a
		// warning: single-use that is sometimes twice-use is not single-use.
		return fmt.Errorf("dashboard: the setup token could not be consumed, so it has not been accepted: %w", err)
	}
	return nil
}

// Outstanding reports whether a live token exists, for the overview and for `doctor`.
// It says nothing about what the token is.
func (s *SetupTokenStore) Outstanding(now time.Time) bool {
	var rec setupTokenRecord
	if err := readJSON(s.path(), &rec); err != nil {
		return false
	}
	return now.Before(rec.ExpiresAt)
}

// Discard removes any outstanding token. It is called the instant the admin account
// exists: from that moment the setup routes do not exist either, and a token that
// outlived them would be a credential for a door that has been bricked up — harmless
// today and exactly the kind of thing that grows a second door later.
func (s *SetupTokenStore) Discard() error {
	err := os.Remove(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// digestOf is the token digest: SHA-256, hex.
func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// writeJSON writes v to path at 0600, creating the directory at 0700.
//
// Not atomic, deliberately: both files here are written once and rarely, a torn write
// is recognised as unparseable rather than as a valid different value, and the recovery
// for either is the same one command. A rename dance would be more code for a failure
// mode that produces the same outcome.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("dashboard: creating %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), fileMode); err != nil {
		return fmt.Errorf("dashboard: writing %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("dashboard: parsing %s: %w", path, err)
	}
	return nil
}
