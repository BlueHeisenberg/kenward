package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// Cookie names. Two of them rather than one, so that a setup session and an admin
// session cannot be confused for each other by a bug in this package: the setup cookie
// is cleared the moment the account exists, and nothing that reads the admin cookie has
// ever seen the other one.
const (
	adminCookieName = "kenward_admin"
	setupCookieName = "kenward_setup"
)

// SessionIdleTimeout is how long a session survives without a request.
//
// Two hours. This is a configuration surface somebody works through in one sitting, and
// a session that outlives the sitting is a browser tab left open on a machine in a
// kitchen. There is no "remember me": the whole point of the account is that it guards
// everything, and a persistent one guards nothing.
const SessionIdleTimeout = 2 * time.Hour

// SetupSessionTTL bounds a first-run session from when the token was exchanged.
//
// It is an absolute life rather than an idle timeout, because this one is not a session
// somebody returns to — it is a wizard, and a wizard that has been open for an hour has
// been abandoned. The token that opened it is already gone, so the recovery is
// `kenward setup-token` and a fresh start, which is the correct cost.
const SetupSessionTTL = time.Hour

// kind distinguishes the two things a cookie can hold.
type kind int

const (
	// kindSetup is a first-run session: it exists only before an admin account does,
	// reaches only the wizard, and is destroyed the instant the account is created.
	kindSetup kind = iota
	// kindAdmin is an authenticated session.
	kindAdmin
)

// session is one browser.
type session struct {
	id   string
	kind kind
	// csrf is this session's token, minted with the session and never rotated
	// within it. Per-session rather than per-form: a per-form token buys protection
	// against replay within one session, which is not a threat here — the attacker
	// this defends against cannot read the token at all — and costs server state
	// keyed on forms nobody submitted.
	csrf     string
	created  time.Time
	lastSeen time.Time
	// wizard is the first-run answers collected so far. Nil on an admin session.
	wizard *wizardState
}

// expired reports whether this session is past its life at now.
func (s *session) expired(now time.Time) bool {
	if s.kind == kindSetup {
		return now.Sub(s.created) > SetupSessionTTL
	}
	return now.Sub(s.lastSeen) > SessionIdleTimeout
}

// sessions is the in-memory session table.
//
// In memory, so a restart logs everybody out. That is the right default for a service
// whose sessions guard every member's private memory: persisting them would mean a
// second store of live credentials on the same disk as the thing they unlock, to save
// an operator one password entry after an event that happens on updates and reboots.
type sessions struct {
	mu   sync.Mutex
	byID map[string]*session
	now  func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{byID: map[string]*session{}, now: now}
}

// create mints a session and returns it. The id is fresh every time, which is the whole
// of the defence against session fixation: nothing in this package ever adopts an id
// that arrived from a client, or promotes a session in place.
func (s *sessions) create(k kind) (*session, error) {
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	sess := &session{id: id, kind: k, csrf: csrf, created: now, lastSeen: now}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = sess
	return sess, nil
}

// get returns the live session with this id, touching its last-seen time. An expired
// session is removed and reported absent, so an idle browser is logged out by the next
// request rather than by a sweeper that may not have run.
func (s *sessions) get(id string, k kind) (*session, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || sess.kind != k {
		return nil, false
	}
	now := s.now()
	if sess.expired(now) {
		delete(s.byID, id)
		return nil, false
	}
	sess.lastSeen = now
	return sess, true
}

// destroy removes one session.
func (s *sessions) destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// destroyKind removes every session of one kind.
//
// It is how the setup sessions end: the instant an admin account exists, every session
// that was allowed to reach the wizard without one stops existing. Not the one that
// created the account — all of them, because "how many setup sessions are open" is not
// a question this package should have to be right about.
func (s *sessions) destroyKind(k kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.byID {
		if sess.kind == k {
			delete(s.byID, id)
		}
	}
}

// count reports how many live sessions of a kind exist. For tests and the overview.
func (s *sessions) count(k kind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for id, sess := range s.byID {
		if sess.expired(now) {
			delete(s.byID, id)
			continue
		}
		if sess.kind == k {
			n++
		}
	}
	return n
}

// validCSRF compares a submitted token against this session's, in constant time.
func (s *session) validCSRF(submitted string) bool {
	if submitted == "" || s.csrf == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(s.csrf)) == 1
}

// randomToken is 256 bits, base64url, no padding. Used for session ids, CSRF tokens and
// setup tokens: all three are opaque values compared for equality, and one generator is
// one thing to get right.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setCookie writes a session cookie.
//
// HttpOnly so a script cannot read it, SameSite=Lax so a cross-site form post never
// carries it, Secure whenever the listener is TLS, MaxAge unset so it dies with the
// browser session. Lax rather than Strict: every mutation here is a POST carrying a
// CSRF token, which Lax already blocks cross-site on its own, and Strict additionally
// breaks arriving from a bookmark manager for no gain.
func setCookie(w http.ResponseWriter, name, value string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie expires a session cookie.
func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Login attempt limits.
const (
	// LoginAttemptLimit is how many failures are allowed inside LoginWindow before
	// the account locks.
	LoginAttemptLimit = 5
	// LoginWindow is the sliding window failures are counted over.
	LoginWindow = 15 * time.Minute
	// LoginLockout is how long the account stays locked once it locks.
	LoginLockout = 15 * time.Minute
)

// loginGuard rate limits and locks out the login path.
//
// It counts against the account rather than against the client address, and that is the
// deliberate choice rather than the lazy one: there is one account, an attacker with a
// botnet has as many addresses as they like, and a per-address counter would rate limit
// the household and nobody else. The cost is that anyone who can reach the port can lock
// the admin out for fifteen minutes — which is a nuisance where a stolen password is
// not, and is bounded by the fact that reaching the port is the thing loopback-by-default
// prevents.
//
// ponytail: one global counter for one account. If the dashboard ever grows a second
// account, this becomes a map keyed on the account and the reasoning above has to be
// redone, because per-account lockout across many accounts is a denial-of-service knob.
//
// It is on whatever the bind address is, not only when the bind is not loopback. The
// requirement is that it exists the moment the listener is reachable; making it
// conditional would mean a branch whose false arm is only ever exercised in the
// configuration nobody attacks, which is how the true arm rots.
type loginGuard struct {
	mu          sync.Mutex
	failures    []time.Time
	lockedUntil time.Time
	now         func() time.Time
}

func newLoginGuard(now func() time.Time) *loginGuard { return &loginGuard{now: now} }

// locked reports whether login is refused, and until when.
func (g *loginGuard) locked() (bool, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Before(g.lockedUntil) {
		return true, g.lockedUntil
	}
	return false, time.Time{}
}

// fail records a rejected attempt and locks the account if it was one too many.
func (g *loginGuard) fail() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	cutoff := now.Add(-LoginWindow)
	kept := g.failures[:0]
	for _, t := range g.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.failures = append(kept, now)
	if len(g.failures) >= LoginAttemptLimit {
		g.lockedUntil = now.Add(LoginLockout)
		// Cleared so the lockout is served once rather than re-armed by every
		// attempt made during it. An attempt while locked never reaches here.
		g.failures = nil
	}
}

// succeed clears the failure history.
func (g *loginGuard) succeed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = nil
	g.lockedUntil = time.Time{}
}
