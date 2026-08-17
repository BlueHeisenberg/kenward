package dashboard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// SpaceClient is the part of the memory client the dashboard needs: listing spaces so
// an operator can choose one, and creating one so they do not have to.
//
// It is an interface rather than *memory.Client because every test in this package must
// be able to run without a lore on the machine, and a fake that returns spaces is a
// faithful stand-in for one that does — listing and creating are the whole contract.
type SpaceClient interface {
	Spaces(ctx context.Context) ([]memory.Space, error)
	CreateSpace(ctx context.Context, name string) (memory.Space, error)
	Close() error
}

// Deps is everything the dashboard needs from outside itself.
//
// The two function fields are seams onto `cmd/kenward`'s own enrolment wiring rather
// than reimplementations of it. Minting a claim code in isolated mode is not one call:
// the digest has to be exported to the member's seed file or the code reaches a pod that
// has never heard of it and fails silently in the member's chat. That logic exists,
// works and is tested; a second copy of it here is how the dashboard grows a subtly
// different enrolment.
type Deps struct {
	// ConfigPath is the kenward.yaml this dashboard reads and writes.
	ConfigPath string
	// DataDir is where the admin record, the setup token and any generated
	// certificate live, under DirName.
	DataDir string

	// Lore opens a client onto the household's lore. It is a factory rather than a
	// value because the real one holds a subprocess: one is started for the handler
	// that needs it and closed when that handler is done.
	Lore func(ctx context.Context) (SpaceClient, error)

	// MintInvite mints a claim code for a member the configuration already declares,
	// and delivers the digest wherever that household's mode needs it.
	MintInvite func(ctx context.Context, cfg *config.Config, id domain.MemberID, name string, ttl time.Duration) (string, error)
	// Revoke unbinds a member's Telegram account.
	Revoke func(ctx context.Context, cfg *config.Config, id domain.MemberID) error

	// Probe reports whether an endpoint's address answers. Nil means
	// setup.DefaultProbe.
	Probe setup.Probe
	// Models reads an endpoint's model list and its published context window. Nil
	// means setup.DefaultModelsProbe.
	Models setup.ModelsProbe
	// Telegram asks Telegram which bot a token belongs to, and whether it can hear a
	// group chat. Nil means setup.DefaultTelegramProbe.
	Telegram setup.TelegramProbe

	// GOOS overrides the operating system the wizard writes for. Empty means
	// runtime.GOOS, which is what production wants; it is a seam because isolated
	// mode is Linux-only and the flow through it is otherwise untestable anywhere
	// else.
	GOOS string

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Logger receives request-level events. Nil means discard.
	Logger *slog.Logger
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d Deps) probe() setup.Probe {
	if d.Probe == nil {
		return setup.DefaultProbe
	}
	return d.Probe
}

func (d Deps) models() setup.ModelsProbe {
	if d.Models == nil {
		return setup.DefaultModelsProbe
	}
	return d.Models
}

func (d Deps) telegram() setup.TelegramProbe {
	if d.Telegram == nil {
		return setup.DefaultTelegramProbe
	}
	return d.Telegram
}

func (d Deps) goos() string {
	if d.GOOS == "" {
		return runtime.GOOS
	}
	return d.GOOS
}

func (d Deps) logger() *slog.Logger {
	if d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Logger
}

// Server is the dashboard's HTTP server.
type Server struct {
	deps   Deps
	dash   config.DashboardConfig
	admin  *AdminStore
	tokens *SetupTokenStore
	sess   *sessions
	guard  *loginGuard
	mux    *http.ServeMux

	mu   sync.Mutex
	http *http.Server
	ln   net.Listener
}

// New builds a server. It opens no socket: the listener is taken by Start, so that
// building one in a test costs nothing and so that a configuration error is reported
// before a port is claimed.
func New(d Deps, dash config.DashboardConfig) (*Server, error) {
	if strings.TrimSpace(d.DataDir) == "" {
		return nil, errors.New("dashboard: no data directory, and the admin account has nowhere to live")
	}
	if strings.TrimSpace(d.ConfigPath) == "" {
		return nil, errors.New("dashboard: no configuration path, and there is nothing to configure")
	}
	s := &Server{
		deps:   d,
		dash:   dash,
		admin:  NewAdminStore(d.DataDir),
		tokens: NewSetupTokenStore(d.DataDir),
		sess:   newSessions(d.now),
		guard:  newLoginGuard(d.now),
	}
	s.routes()
	return s, nil
}

// Handler is the server's routing, for tests and for anything that wants to serve it
// itself. It is the same value Start serves; there is no second wiring.
func (s *Server) Handler() http.Handler { return s.securityHeaders(s.mux) }

// Addr is the address the listener actually took, empty before Start. It is read after
// Start rather than assumed from the configuration because a bind of :0 is how a test
// gets a port, and because "what did it actually bind to" is the question `doctor` and
// the startup line both want answered.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL is the address to type into a browser.
func (s *Server) URL() string {
	addr := s.Addr()
	if addr == "" {
		addr = s.dash.BindAddr()
	}
	scheme := "http"
	if s.dash.TLS() {
		scheme = "https"
	}
	// A bind of 0.0.0.0 or :: is not an address anybody can visit, so the printed
	// URL names loopback instead of repeating a wildcard back at somebody who then
	// pastes it into a browser and gets nothing.
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return scheme + "://" + addr
}

// Listen takes the socket. It is separate from Serve so that the address is known — and
// the port is claimed, and any failure to claim it reported — before anything is told
// where to point a browser.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("dashboard: already listening")
	}
	ln, err := net.Listen("tcp", s.dash.BindAddr())
	if err != nil {
		return fmt.Errorf("dashboard: binding %s: %w", s.dash.BindAddr(), err)
	}
	if s.dash.TLS() {
		cert, err := tls.LoadX509KeyPair(s.dash.TLSCertFile, s.dash.TLSKeyFile)
		if err != nil {
			ln.Close()
			return fmt.Errorf("dashboard: reading the certificate: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}
	s.ln = ln
	s.http = &http.Server{
		Handler: s.Handler(),
		// Bounded on purpose. This listener may be on a LAN, and an unbounded
		// header read is a connection an attacker holds open for free.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return nil
}

// Serve runs until Shutdown. It returns nil on a clean shutdown, so a caller can treat
// any error as a real one.
func (s *Server) Serve() error {
	s.mu.Lock()
	srv, ln := s.http, s.ln
	s.mu.Unlock()
	if srv == nil || ln == nil {
		return errors.New("dashboard: Serve called before Listen")
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting connections and lets in-flight requests finish, bounded by
// ctx. It is idempotent, and safe on a server that never listened — which is the state
// of every dashboard in a household that has not enabled one.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.http
	s.http, s.ln = nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// SetupTokenIfNeeded issues and returns a setup token when no admin account exists, and
// returns an empty string when one does.
//
// It is called on the way up, which is what makes first run reachable at all: there is
// no account, so there is no password, so the only way in is a secret the process prints
// where the person who started it can see it. Once an account exists this issues
// nothing, because there is nothing a token could be for.
func (s *Server) SetupTokenIfNeeded() (string, error) {
	if s.admin.Exists() {
		// Belt and braces against a token left over from before the account was
		// made — Register already discards one, and a second discard costs nothing
		// next to a live credential for a route that no longer exists.
		if err := s.tokens.Discard(); err != nil {
			return "", err
		}
		return "", nil
	}
	return s.tokens.Issue(s.deps.now())
}

// routes wires the whole surface.
//
// Every pattern is method-qualified, so a GET to a mutating route is a 405 rather than a
// mutation performed without a form. That matters more than it looks: a GET that changes
// something is a change any image tag on any page can make, whatever the CSRF token
// says.
func (s *Server) routes() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /static/style.css", s.handleCSS)

	// First run. Both of these 404 once an admin account exists — not redirect, not
	// "already set up": a route that does not exist should not confirm that it once
	// did to somebody who has no session.
	mux.HandleFunc("GET /setup", s.setupOnly(s.handleSetupToken))
	mux.HandleFunc("POST /setup", s.setupOnly(s.handleSetupTokenSubmit))
	// The wizard's own steps are not setupOnly: the account is created halfway
	// through it, and every step after that runs on the admin session it produced.
	// See withWizardSession.
	mux.HandleFunc("GET /setup/{step}", s.withWizardSession(s.handleWizardStep))
	mux.HandleFunc("POST /setup/{step}", s.withWizardSession(s.csrf(s.handleWizardSubmit)))

	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.withAdmin(s.csrf(s.handleLogout)))

	mux.HandleFunc("GET /overview", s.withAdmin(s.handleOverview))
	mux.HandleFunc("GET /members", s.withAdmin(s.handleMembers))
	mux.HandleFunc("POST /members/add", s.withAdmin(s.csrf(s.handleMemberAdd)))
	mux.HandleFunc("POST /members/revoke", s.withAdmin(s.csrf(s.handleMemberRevoke)))
	mux.HandleFunc("POST /members/invite", s.withAdmin(s.csrf(s.handleMemberInvite)))
	mux.HandleFunc("GET /settings", s.withAdmin(s.handleSettings))
	mux.HandleFunc("POST /settings", s.withAdmin(s.csrf(s.handleSettingsSubmit)))
	mux.HandleFunc("GET /exposure", s.withAdmin(s.handleExposure))
	mux.HandleFunc("POST /exposure", s.withAdmin(s.csrf(s.handleExposureSubmit)))
	mux.HandleFunc("GET /password", s.withAdmin(s.handlePassword))
	mux.HandleFunc("POST /password", s.withAdmin(s.csrf(s.handlePasswordSubmit)))

	// A GET of a POST-only route is a bookmark or a back button, not an attack: nobody
	// reaches /members/add except by having been there once, when the form posted to
	// it. Go's mux answers one with a bare 405 and no page at all — a blank white
	// screen in the middle of a dashboard where everything else is styled — so each of
	// these sends the browser to the page its form lives on. Nothing mutates: this is a
	// redirect, and the POST above still carries the CSRF check.
	for from, to := range map[string]string{
		"/members/add":    "/members",
		"/members/revoke": "/members",
		"/members/invite": "/members",
		"/logout":         "/overview",
	} {
		mux.HandleFunc("GET "+from, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, to, http.StatusSeeOther)
		})
	}

	s.mux = mux
}

// securityHeaders wraps every response.
//
// The content security policy is the load-bearing one: this package inlines no script
// and loads nothing from anywhere, so 'none' for everything except its own styles is a
// policy it can actually keep, and one that turns a future injected <script> into a
// blocked request rather than a session theft.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// No caching anywhere. Every page here is either a form with a CSRF token in
		// it or a list of who lives in the house; neither belongs in a shared
		// browser's back button after a logout.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// setupOnly gates the first-run routes on there being no admin account.
//
// It is checked from the filesystem on every request rather than remembered, so the
// window between the account being created and these routes disappearing is one
// request, not one restart.
func (s *Server) setupOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.admin.Exists() {
			s.notFound(w, r)
			return
		}
		next(w, r)
	}
}

// withAdmin requires an authenticated session. Everything that is not the login page or
// the first-run flow goes through it.
func (s *Server) withAdmin(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionFrom(r, adminCookieName, kindAdmin)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			// A POST with no session gets a status and no page. Redirecting a form
			// post to the login page would look like the form was accepted.
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		next(w, r, sess)
	}
}

// csrf refuses a mutating request whose token is absent, wrong, or another session's.
func (s *Server) csrf(next func(http.ResponseWriter, *http.Request, *session)) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, r *http.Request, sess *session) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "malformed form", http.StatusBadRequest)
			return
		}
		if !sess.validCSRF(r.PostFormValue("csrf")) {
			s.deps.logger().Warn("dashboard", "event", "csrf_rejected", "path", r.URL.Path)
			http.Error(w, "this form has expired or did not come from here; go back and try again", http.StatusForbidden)
			return
		}
		next(w, r, sess)
	}
}

// sessionFrom reads and validates the session named by a cookie.
func (s *Server) sessionFrom(r *http.Request, cookie string, k kind) (*session, bool) {
	c, err := r.Cookie(cookie)
	if err != nil {
		return nil, false
	}
	return s.sess.get(c.Value, k)
}

// secure reports whether cookies should carry the Secure attribute.
func (s *Server) secure() bool { return s.dash.TLS() }

// handleRoot sends somebody at the front door wherever they actually belong.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !s.admin.Exists() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
}

// notFound is the one 404. It says nothing about why.
func (s *Server) notFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}

// certPaths are where a generated self-signed pair is written.
func (s *Server) certPaths() (certFile, keyFile string) {
	dir := filepath.Join(s.deps.DataDir, DirName)
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// ensureDir creates the dashboard's own directory.
func (s *Server) ensureDir() error {
	return os.MkdirAll(filepath.Join(s.deps.DataDir, DirName), 0o700)
}
