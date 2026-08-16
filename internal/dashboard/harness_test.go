package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// testPassword is used everywhere an account is created. It is over
// MinPasswordLength; a shorter one would be refused and the test would be testing the
// length check rather than whatever it meant to.
const testPassword = "correct horse battery staple"

// fakeLore stands in for a lore store.
//
// It is a fake rather than a mock and the distinction is load-bearing here: it holds
// real state, CreateSpace really adds a space with a real id, and Spaces really returns
// what has been created. A double that returned a canned id would pass every test in
// this file while the code under test wrote a member against a space that did not exist
// — which is the exact production failure this feature was built to remove.
type fakeLore struct {
	mu      sync.Mutex
	spaces  []memory.Space
	created []string
	// createErr, when set, is returned by CreateSpace. lore genuinely fails: it is a
	// subprocess over a SQLite store and it is not always there.
	createErr error
	// listErr, when set, is returned by Spaces.
	listErr error
	closed  bool
	next    int
}

func newFakeLore(existing ...memory.Space) *fakeLore {
	return &fakeLore{spaces: append([]memory.Space(nil), existing...)}
}

func (f *fakeLore) Spaces(context.Context) ([]memory.Space, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]memory.Space(nil), f.spaces...), nil
}

func (f *fakeLore) CreateSpace(_ context.Context, name string) (memory.Space, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return memory.Space{}, f.createErr
	}
	f.next++
	s := memory.Space{
		ID:   fmt.Sprintf("00000000-0000-4000-8000-%012d", f.next),
		Name: name,
		Kind: "shared",
	}
	f.spaces = append(f.spaces, s)
	f.created = append(f.created, name)
	return s, nil
}

func (f *fakeLore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// harness is a running dashboard with a real HTTP server in front of it.
//
// httptest rather than calling handlers directly, because half of what is being asserted
// is cookie behaviour and status codes on redirects, and neither survives a handler
// called by hand.
type harness struct {
	t          *testing.T
	srv        *Server
	http       *httptest.Server
	client     *http.Client
	dataDir    string
	configPath string
	lore       *fakeLore
	now        time.Time

	// bot overrides what Telegram says about the token, for the tests about a bot
	// that cannot hear a group chat.
	bot *setup.BotInfo

	// minted records every claim code handed out, so a test can assert one was
	// produced without the production code having to expose it.
	minted  []string
	revoked []domain.MemberID
	mintErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:          t,
		dataDir:    dir,
		configPath: filepath.Join(dir, "kenward.yaml"),
		lore:       newFakeLore(),
		now:        time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}

	deps := Deps{
		ConfigPath: h.configPath,
		DataDir:    dir,
		Now:        func() time.Time { return h.now },
		Lore:       func(context.Context) (SpaceClient, error) { return h.lore, nil },
		Probe: func(_ context.Context, baseURL string) setup.ProbeResult {
			return setup.ProbeResult{State: setup.Answered, Addr: baseURL, Elapsed: time.Millisecond}
		},
		Models: func(context.Context, string, string) ([]setup.ModelInfo, error) {
			return []setup.ModelInfo{{ID: "test-model", ContextWindow: 262144}}, nil
		},
		// A bot Telegram accepts, with privacy mode already off. Injected rather
		// than left nil for two reasons: nil is the real api.telegram.org, and a
		// test suite that reaches it is a test suite that fails on a train; and
		// false here is the defect, so the healthy default has to be stated.
		Telegram: func(context.Context, string) (setup.BotInfo, error) {
			if h.bot != nil {
				return *h.bot, nil
			}
			return setup.BotInfo{Username: "casa_household_bot", ReadsGroupMessages: true}, nil
		},
		MintInvite: func(_ context.Context, _ *config.Config, id domain.MemberID, _ string, _ time.Duration) (string, error) {
			if h.mintErr != nil {
				return "", h.mintErr
			}
			code := "CODE-" + string(id)
			h.minted = append(h.minted, code)
			return code, nil
		},
		Revoke: func(_ context.Context, _ *config.Config, id domain.MemberID) error {
			h.revoked = append(h.revoked, id)
			return nil
		},
	}

	srv, err := New(deps, config.DashboardConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.http = httptest.NewServer(srv.Handler())
	t.Cleanup(h.http.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{
		Jar: jar,
		// Redirects are not followed. Every assertion in this file is about the
		// status and the Location a handler chose, and a client that follows them
		// turns a 303 to /login into a 200 that looks like success.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return h
}

// get issues a GET.
func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	resp, err := h.client.Get(h.http.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// post issues a form POST with no CSRF token unless the form carries one.
func (h *harness) post(path string, form url.Values) *http.Response {
	h.t.Helper()
	resp, err := h.client.PostForm(h.http.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// postCSRF issues a form POST carrying the current session's CSRF token.
func (h *harness) postCSRF(path string, form url.Values) *http.Response {
	h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", h.csrf())
	return h.post(path, form)
}

// csrf returns the token of whatever session this client currently holds.
//
// It is read out of the session table rather than scraped from a page, because a test
// that scrapes would silently pass on a page that stopped rendering the field — and
// "the form has no token in it" is a bug this suite must catch, not accommodate.
func (h *harness) csrf() string {
	h.t.Helper()
	u, _ := url.Parse(h.http.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name != adminCookieName && c.Name != setupCookieName {
			continue
		}
		k := kindAdmin
		if c.Name == setupCookieName {
			k = kindSetup
		}
		if sess, ok := h.srv.sess.get(c.Value, k); ok {
			return sess.csrf
		}
	}
	return ""
}

// cookie returns a cookie the client currently holds, or "".
func (h *harness) cookie(name string) string {
	u, _ := url.Parse(h.http.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// clearCookies forgets every cookie, which is what a fresh browser is.
func (h *harness) clearCookies() {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.client.Jar = jar
}

// issueToken mints a setup token the way the process does on the way up.
func (h *harness) issueToken() string {
	h.t.Helper()
	token, err := h.srv.SetupTokenIfNeeded()
	if err != nil {
		h.t.Fatal(err)
	}
	if token == "" {
		h.t.Fatal("no setup token was issued, but there is no admin account")
	}
	return token
}

// createAdmin makes the account directly, for the tests that are about what happens
// afterwards rather than about the wizard.
func (h *harness) createAdmin() {
	h.t.Helper()
	if err := h.srv.admin.Create(context.Background(), testPassword); err != nil {
		h.t.Fatal(err)
	}
}

// signIn authenticates this client.
func (h *harness) signIn() {
	h.t.Helper()
	resp := h.post("/login", url.Values{"password": {testPassword}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("sign in: status = %d, want 303\n%s", resp.StatusCode, body(h.t, resp))
	}
	if h.cookie(adminCookieName) == "" {
		h.t.Fatal("sign in produced no session cookie")
	}
}

// writeConfig puts a minimal, valid household on disk.
func (h *harness) writeConfig() *config.Config {
	h.t.Helper()
	cfg := &config.Config{
		Mode:    config.ModeSimple,
		DataDir: h.dataDir,
		Household: config.HouseholdConfig{
			Name:        "Home",
			SharedSpace: "00000000-0000-4000-8000-000000000099",
			Tiers:       []string{"local"},
		},
		Telegram: config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Endpoints: []config.EndpointConfig{{
			Name: "monster", BaseURL: "http://monster.tail:8000/v1",
			Model: "qwen", Tags: []string{"local"},
		}},
		Dashboard: config.DashboardConfig{Enabled: true, Exposure: config.ExposureLoopback},
	}
	cfg.ApplyDefaults()
	if err := setup.WriteConfig(h.configPath, cfg, true); err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mutatingRoutes is every route that changes something. The adversarial tests walk it
// rather than naming routes one at a time, so a route added without protection fails
// this suite instead of being noticed later.
var mutatingRoutes = []string{
	"/setup",
	"/setup/install",
	"/setup/admin",
	"/setup/telegram",
	"/setup/endpoints",
	"/setup/trust",
	"/setup/advanced",
	"/setup/review",
	"/login",
	"/logout",
	"/members/add",
	"/members/invite",
	"/members/revoke",
	"/settings",
	"/exposure",
	"/password",
}

// readRoutes is every route that shows something.
var readRoutes = []string{
	"/",
	"/overview",
	"/members",
	"/settings",
	"/exposure",
	"/password",
	"/setup",
	"/setup/install",
	"/setup/admin",
	"/setup/telegram",
	"/setup/endpoints",
	"/setup/trust",
	"/setup/advanced",
	"/setup/review",
}

// looksLikeAPage reports whether a response body is one of the dashboard's own rendered
// pages rather than a status line. It is how "did anything leak" is asserted.
func looksLikeAPage(b string) bool {
	return strings.Contains(b, "<!DOCTYPE html>")
}
