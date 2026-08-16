package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The tests in this file are written from the outside, by somebody who can reach the
// port and wants what is behind it. They are not a coverage exercise: this box holds
// every member's private memory, and the dashboard is the only part of kenward that
// answers a stranger at all.

// TestUnauthenticatedCallerReachesNothing walks every route with no session and asserts
// that not one of them renders a page.
//
// It walks a list rather than naming routes one by one so that a route added without a
// guard fails here, which is the only way this stays true as the surface grows.
func TestUnauthenticatedCallerReachesNothing(t *testing.T) {
	h := newHarness(t)
	h.createAdmin() // An account exists, so the setup routes should be gone too.
	h.writeConfig()

	for _, path := range readRoutes {
		t.Run("GET "+path, func(t *testing.T) {
			resp := h.get(path)
			defer resp.Body.Close()
			b := body(t, resp)
			switch resp.StatusCode {
			case http.StatusSeeOther, http.StatusNotFound:
			default:
				t.Fatalf("status = %d, want 303 or 404; body:\n%s", resp.StatusCode, b)
			}
			if looksLikeAPage(b) {
				t.Fatalf("an unauthenticated GET rendered a page:\n%s", b)
			}
		})
	}

	for _, path := range mutatingRoutes {
		if path == "/login" {
			continue // The login form is the one POST a stranger is allowed to make.
		}
		t.Run("POST "+path, func(t *testing.T) {
			resp := h.post(path, url.Values{"anything": {"1"}})
			defer resp.Body.Close()
			b := body(t, resp)
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			default:
				t.Fatalf("status = %d, want 401, 403 or 404; body:\n%s", resp.StatusCode, b)
			}
			if looksLikeAPage(b) {
				t.Fatalf("an unauthenticated POST rendered a page:\n%s", b)
			}
		})
	}
}

// TestSetupTokenIsSingleUse presents a valid token twice.
//
// The first exchange must work and the second must not, whatever the second one does
// with cookies. A token that can be replayed is a token an attacker who sees a terminal
// once can use after the operator has finished with it.
func TestSetupTokenIsSingleUse(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()

	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first exchange: status = %d, want 303", resp.StatusCode)
	}
	if h.cookie(setupCookieName) == "" {
		t.Fatal("first exchange produced no setup session")
	}

	h.clearCookies()
	resp = h.post("/setup", url.Values{"token": {token}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replay: status = %d, want 403", resp.StatusCode)
	}
	if h.cookie(setupCookieName) != "" {
		t.Fatal("a replayed setup token produced a session")
	}
}

// TestWrongSetupTokenDoesNotConsumeTheRealOne.
//
// If a wrong guess destroyed the outstanding token, anybody who can reach the port could
// lock the household out of its own first run by guessing once — a denial of service
// that costs the attacker one request and the household a trip to the terminal.
func TestWrongSetupTokenDoesNotConsumeTheRealOne(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()

	for _, guess := range []string{"", "x", strings.Repeat("A", 43), token + "x", token[:len(token)-1]} {
		resp := h.post("/setup", url.Values{"token": {guess}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("guess %q: status = %d, want 403", guess, resp.StatusCode)
		}
	}

	resp := h.post("/setup", url.Values{"token": {token}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the real token stopped working after wrong guesses: status = %d", resp.StatusCode)
	}
}

// TestExpiredSetupTokenIsRefused.
func TestExpiredSetupTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	h.now = h.now.Add(SetupTokenTTL + time.Second)

	resp := h.post("/setup", url.Values{"token": {token}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if h.srv.tokens.Outstanding(h.now) {
		t.Fatal("an expired token is still reported outstanding")
	}
}

// TestSetupRoutesVanishOnceTheAccountExists, and a token minted before it does not open
// them.
//
// This is the requirement stated the other way round: the token is invalidated the
// instant the admin account exists. Presenting it afterwards must not merely fail to
// authenticate — the page it was for must not be there at all, because a route that says
// "already set up" tells a stranger this box is worth coming back to.
func TestSetupRoutesVanishOnceTheAccountExists(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	h.createAdmin()

	for _, path := range []string{"/setup"} {
		resp := h.get(path)
		b := body(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s after the account exists: status = %d, want 404\n%s", path, resp.StatusCode, b)
		}
	}

	resp := h.post("/setup", url.Values{"token": {token}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("presenting a token after setup: status = %d, want 404", resp.StatusCode)
	}
	if h.cookie(setupCookieName) != "" {
		t.Fatal("a token presented after setup produced a session")
	}
}

// TestSetupTokenFileIsGoneOnceTheAccountExists.
//
// The route check above is the door; this is the credential. A token file that outlived
// the account would be a live secret for a page that no longer exists — harmless today,
// and exactly the sort of thing that becomes a way in when somebody adds a second setup
// route later.
func TestSetupTokenFileIsGoneOnceTheAccountExists(t *testing.T) {
	h := newHarness(t)
	h.issueToken()
	if !h.srv.tokens.Outstanding(h.now) {
		t.Fatal("no token outstanding after issuing one")
	}

	h.createAdmin()
	got, err := h.srv.SetupTokenIfNeeded()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("a token was issued for a household that already has an account: %q", got)
	}
	if h.srv.tokens.Outstanding(h.now) {
		t.Fatal("the setup token survived the creation of the admin account")
	}
}

// TestCSRFIsRequiredOnEveryMutatingRoute.
//
// With a real session and no token, every one of them must refuse. This is walked as a
// list for the same reason the unauthenticated sweep is.
func TestCSRFIsRequiredOnEveryMutatingRoute(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	for _, path := range mutatingRoutes {
		switch path {
		case "/login", "/setup":
			// /login is the unauthenticated entry point and is protected by the
			// session it creates being fresh, not by a token; /setup is gone.
			continue
		}
		t.Run(path, func(t *testing.T) {
			resp := h.post(path, url.Values{"id": {"someone"}})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("POST %s with no CSRF token: status = %d, want 403\n%s",
					path, resp.StatusCode, body(t, resp))
			}
		})
	}
}

// TestForgedCSRFTokenIsRefused: another session's token is not this session's.
//
// A double-submit scheme would accept anything the attacker could also set as a cookie.
// This one compares against server-side state, so a token that is real — just not
// yours — has to fail, and that is what is asserted rather than the easy case of a
// token made of noise.
func TestForgedCSRFTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()

	// A second, legitimate session, whose token the attacker has somehow learned.
	other, err := h.srv.sess.create(kindAdmin)
	if err != nil {
		t.Fatal(err)
	}

	h.signIn()
	mine := h.csrf()
	if mine == other.csrf {
		t.Fatal("two sessions were given the same CSRF token")
	}

	for _, token := range []string{other.csrf, "", "not-a-token", strings.Repeat("x", len(mine))} {
		resp := h.post("/settings", url.Values{"csrf": {token}, "household_name": {"Taken Over"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("CSRF %q: status = %d, want 403", token, resp.StatusCode)
		}
	}

	// And the real one still works, so the check above is not simply refusing
	// everything.
	resp := h.postCSRF("/settings", url.Values{
		"household_name":  {"Home"},
		"shared_space":    {"00000000-0000-4000-8000-000000000099"},
		"household_tiers": {"local"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a correct CSRF token was refused: status = %d\n%s", resp.StatusCode, body(t, resp))
	}
}

// TestLoginBruteForceLocksOut.
//
// Five wrong passwords inside the window and the account is locked, and the lock holds
// even for the right password — otherwise it is a delay, not a lockout, and an attacker
// who has just guessed correctly is exactly who it has to stop.
func TestLoginBruteForceLocksOut(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()

	for i := 0; i < LoginAttemptLimit; i++ {
		resp := h.post("/login", url.Values{"password": {"wrong"}})
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, code)
		}
	}

	resp := h.post("/login", url.Values{"password": {testPassword}})
	b := body(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the correct password was accepted while locked out: status = %d", resp.StatusCode)
	}
	if !strings.Contains(b, "Too many failed attempts") {
		t.Fatalf("lockout page does not say what happened:\n%s", b)
	}
	if h.cookie(adminCookieName) != "" {
		t.Fatal("a locked-out login produced a session cookie")
	}

	// And it lifts.
	h.now = h.now.Add(LoginLockout + time.Second)
	resp = h.post("/login", url.Values{"password": {testPassword}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the lockout expired: status = %d, want 303", resp.StatusCode)
	}
}

// TestFailuresOutsideTheWindowDoNotAccumulate: four failures an hour apart is somebody
// with a bad memory, not an attack, and locking them out would be the wrong answer.
func TestFailuresOutsideTheWindowDoNotAccumulate(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()

	for i := 0; i < LoginAttemptLimit+2; i++ {
		resp := h.post("/login", url.Values{"password": {"wrong"}})
		resp.Body.Close()
		h.now = h.now.Add(LoginWindow + time.Minute)
	}
	resp := h.post("/login", url.Values{"password": {testPassword}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: spaced-out failures should not lock the account", resp.StatusCode)
	}
}

// TestNoSessionFixation.
//
// An attacker who can set a cookie in the victim's browser — a sibling app on the same
// host, an XSS somewhere else on localhost — must not be able to choose the id the
// victim ends up authenticated under. So the id after signing in must differ from the id
// before, and the one the attacker planted must be dead.
func TestNoSessionFixation(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()

	// The attacker obtains a real, unauthenticated session id and plants it.
	planted, err := h.srv.sess.create(kindAdmin)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(h.http.URL)
	h.client.Jar.SetCookies(u, []*http.Cookie{{Name: adminCookieName, Value: planted.id}})

	h.signIn()

	got := h.cookie(adminCookieName)
	if got == planted.id {
		t.Fatal("signing in kept the session id the browser arrived with")
	}
	if _, ok := h.srv.sess.get(planted.id, kindAdmin); ok {
		t.Fatal("the planted session is still live after the victim signed in")
	}
}

// TestSessionCookieFlags.
func TestSessionCookieFlags(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	resp := h.post("/login", url.Values{"password": {testPassword}})
	defer resp.Body.Close()

	var found bool
	for _, c := range resp.Cookies() {
		if c.Name != adminCookieName {
			continue
		}
		found = true
		if !c.HttpOnly {
			t.Error("the session cookie is not HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.MaxAge != 0 || !c.Expires.IsZero() {
			t.Error("the session cookie outlives the browser session")
		}
	}
	if !found {
		t.Fatal("no session cookie was set")
	}
}

// TestSecureFlagFollowsTLS: over a TLS listener the cookie must be Secure, or it is a
// credential a downgrade attack can read.
func TestSecureFlagFollowsTLS(t *testing.T) {
	h := newHarness(t)
	h.srv.dash.TLSCertFile, h.srv.dash.TLSKeyFile = "cert.pem", "key.pem"
	h.createAdmin()

	resp := h.post("/login", url.Values{"password": {testPassword}})
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == adminCookieName && !c.Secure {
			t.Fatal("the session cookie is not Secure on a TLS listener")
		}
	}
}

// TestSignedOutSessionIsDead: the cookie the browser keeps must stop working, not merely
// be cleared client-side.
func TestSignedOutSessionIsDead(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()
	id := h.cookie(adminCookieName)

	resp := h.postCSRF("/logout", nil)
	resp.Body.Close()

	// Put the cookie back, the way a stolen one would be.
	u, _ := url.Parse(h.http.URL)
	h.client.Jar.SetCookies(u, []*http.Cookie{{Name: adminCookieName, Value: id}})
	resp = h.get("/overview")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a signed-out session still works: status = %d", resp.StatusCode)
	}
}

// TestIdleSessionExpires.
func TestIdleSessionExpires(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	h.now = h.now.Add(SessionIdleTimeout + time.Minute)
	resp := h.get("/overview")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("an idle session still works: status = %d", resp.StatusCode)
	}
}

// TestWizardStepsPastTheAccountAreRefusedBeforeItExists.
//
// Ordering the questions is not a control; a URL is a way of skipping to one. Somebody
// holding a setup token must not be able to jump to the step that writes the
// configuration, because everything after the account step is a question an
// unauthenticated caller must not be able to answer.
func TestWizardStepsPastTheAccountAreRefusedBeforeItExists(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()

	for _, step := range []string{"telegram", "endpoints", "trust", "advanced", "review"} {
		got := h.get("/setup/" + step)
		loc := got.Header.Get("Location")
		code := got.StatusCode
		got.Body.Close()
		if code != http.StatusSeeOther || loc != "/setup/admin" {
			t.Fatalf("GET /setup/%s: status = %d, location = %q; want a 303 to /setup/admin", step, code, loc)
		}

		posted := h.postCSRF("/setup/"+step, url.Values{"mode": {"simple"}})
		code = posted.StatusCode
		posted.Body.Close()
		if code != http.StatusForbidden {
			t.Fatalf("POST /setup/%s before the account exists: status = %d, want 403", step, code)
		}
	}
}

// TestTheAccountCannotBeCreatedTwice: the account step is not a reset flow, and replaying
// its form must not be one either.
func TestTheAccountCannotBeCreatedTwice(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()
	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()

	form := url.Values{"password": {testPassword}, "password2": {testPassword}}
	resp = h.postCSRF("/setup/admin", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("creating the account: status = %d, want 303", resp.StatusCode)
	}

	// The browser now holds an admin session; replay the same form on it.
	replay := h.postCSRF("/setup/admin", url.Values{"password": {"a different one entirely"}, "password2": {"a different one entirely"}})
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusConflict {
		t.Fatalf("replaying the account form: status = %d, want 409", replay.StatusCode)
	}
	// The original password still works, which is the thing that matters.
	h.clearCookies()
	h.signIn()
}

// TestCreatingTheAccountDestroysEverySetupSession.
//
// Not only the one that made it. "How many setup sessions are open" is not a question
// this package should have to be right about, so the answer after an account exists is
// none.
func TestCreatingTheAccountDestroysEverySetupSession(t *testing.T) {
	h := newHarness(t)
	token := h.issueToken()

	// A second setup session, as an attacker who exchanged an earlier token would
	// hold. (There is only ever one token, so this is generous.)
	stray, err := h.srv.sess.create(kindSetup)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.post("/setup", url.Values{"token": {token}})
	resp.Body.Close()
	resp = h.postCSRF("/setup/admin", url.Values{"password": {testPassword}, "password2": {testPassword}})
	resp.Body.Close()

	if _, ok := h.srv.sess.get(stray.id, kindSetup); ok {
		t.Fatal("a setup session survived the creation of the admin account")
	}
	if n := h.srv.sess.count(kindSetup); n != 0 {
		t.Fatalf("%d setup sessions are still live", n)
	}
	if h.cookie(setupCookieName) != "" {
		t.Fatal("the setup cookie was not cleared")
	}
	if h.cookie(adminCookieName) == "" {
		t.Fatal("no admin session was issued in its place")
	}
}

// TestChangingThePasswordSignsEverybodyOut, including the browser that changed it.
func TestChangingThePasswordSignsEverybodyOut(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	other, err := h.srv.sess.create(kindAdmin)
	if err != nil {
		t.Fatal(err)
	}

	const next = "a much longer replacement password"
	resp := h.postCSRF("/password", url.Values{
		"current": {testPassword}, "password": {next}, "password2": {next},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("changing the password: status = %d, want 303", resp.StatusCode)
	}
	if _, ok := h.srv.sess.get(other.id, kindAdmin); ok {
		t.Fatal("another browser is still signed in after a password change")
	}
	if n := h.srv.sess.count(kindAdmin); n != 0 {
		t.Fatalf("%d admin sessions survived the password change", n)
	}

	h.clearCookies()
	resp = h.post("/login", url.Values{"password": {testPassword}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old password still works: status = %d", resp.StatusCode)
	}
	resp = h.post("/login", url.Values{"password": {next}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the new password does not work: status = %d", resp.StatusCode)
	}
}

// TestNoPageIsCacheable and the content security policy is on everything.
//
// Both are one header, and both matter for the same reason: this dashboard is opened on
// shared machines, and a back button that re-renders the members list after a sign-out
// is a leak that needs no attacker at all.
func TestSecurityHeadersAreOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	for _, path := range []string{"/overview", "/members", "/settings", "/exposure", "/password", "/login"} {
		resp := h.get(path)
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: Content-Security-Policy = %q", path, csp)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

// TestGETCannotMutate: a GET to a mutating route is a 405, not a mutation.
//
// A GET that changes something is a change any <img> tag on any page in the browser can
// make, whatever the CSRF token says, because the browser will not send a form body but
// will send the cookie.
func TestGETCannotMutate(t *testing.T) {
	h := newHarness(t)
	h.createAdmin()
	h.writeConfig()
	h.signIn()

	for _, path := range []string{"/logout", "/members/add", "/members/invite", "/members/revoke"} {
		resp := h.get(path)
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", path, code)
		}
	}
}
