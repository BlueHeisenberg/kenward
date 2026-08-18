package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// dashboardFile is the only part of kenward.yaml this binary reads.
//
// The block is decoded into internal/config's own type. It used to be decoded into a
// struct of this binary's own, spelling the keys `addr` and `port` — key names the
// real schema has never had. Two consequences, both of which shipped: dashboardURL
// returned "" for every valid configuration, so the menu item this binary largely
// exists for was greyed out permanently; and a household that wrote `dashboard.addr`
// to satisfy it got a `kenward run` that refused the entire file, because
// config.Decode sets KnownFields(true). One schema, defined in one place, is the only
// arrangement in which that cannot happen again — and it is free here, internal/config
// already being in this binary's dependency graph by way of internal/setup.
//
// What is deliberately not borrowed is validation. Judging a household is `kenward
// run`'s job and `kenward doctor`'s, and a second validator here would be a second
// opinion to disagree with. So this decodes the one block it needs and asks nothing
// about the rest: no KnownFields, no Validate. yaml.v3 ignores every key it is not
// asked about, so the file may grow freely.
type dashboardFile struct {
	Dashboard config.DashboardConfig `yaml:"dashboard"`
}

// dashboardURL returns the address to open, or "" when this household has no
// dashboard.
//
// Empty is a normal answer, not an error. The dashboard is optional, headless is
// first-class, and the menu item is disabled rather than launching a browser at a
// port nothing is listening on — a blank error page is a worse answer than a greyed
// item that says why.
func dashboardURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	var c dashboardFile
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		return ""
	}
	d := c.Dashboard
	// The zero value is off, and off is what `kenward run` acts on: with enabled
	// unset it opens no socket at all. An address configured against the day
	// somebody turns it on is not a dashboard to send a browser to.
	if !d.Enabled {
		return ""
	}
	// BindAddr, not Bind: an enabled dashboard that names no address is listening on
	// config.DefaultDashboardBind, and that is the commonest way to enable one.
	addr := loopback(d.BindAddr())
	if addr == "" {
		return ""
	}
	scheme := "http"
	if d.TLS() {
		scheme = "https"
	}
	return scheme + "://" + addr
}

// loopback rewrites a listen address into one a browser on this machine can reach,
// or "" when it is not an address the daemon could serve on.
//
// A server configured to listen on 0.0.0.0:8770 or on :8770 is listening on every
// interface, and "http://0.0.0.0:8770" is not a thing a browser can usefully open;
// loopback is where it answers. An address that names a real host is left alone. One
// that is not host:port at all is one `kenward run` refuses to start on — see
// config.validateDashboard — so there is no URL to offer and the item stays greyed.
//
// It is spelled the way internal/dashboard's URLFor and Server.URL spell it, down to
// the ParseIP test, because it is the same rule; the three should collapse into one
// method on config.DashboardConfig, which is where the scheme and the address already
// come from. Importing internal/dashboard for it is not the way: it costs this binary
// ten dependencies and ~600 KB of embedded admin templates, argon2 and html/template,
// to link an HTTP server into a menu bar icon.
func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// resolveConfigPath mirrors the rule in cmd/kenward/paths.go: the environment, then
// kenward.yaml in the working directory, then the per-OS configuration location.
//
// It is a copy, and that is the lesser of two evils. The original lives in the daemon's
// own `main` package where nothing can import it, and the shared home for it would be
// internal/config — which the daemon must keep building without cgo and which is not
// this change's to reshape. The flag form is absent here because this binary takes no
// flags, and the daemon's copy takes its environment through *env because it is
// injectable in tests; those two differences are deliberate.
//
// This comment used to claim the copies were "checked against each other by a test".
// No such test existed, and none of the shape described can: both functions live in
// `package main`, in two different commands, so no third package can see either of
// them. The claim outlived the truth long enough for the signatures to diverge
// unnoticed — the daemon's is resolveConfigPath(e *env, flagValue string) — which is
// exactly the drift the imagined test was supposed to catch. Deleted rather than
// written, because a guarantee that cannot be written is worse than an admitted copy:
// it stops the next reader looking.
//
// What actually ties the two together is setup.DefaultConfigFileName, which both
// import, and which is the part most likely to move. The environment variable name is
// not tied: the daemon has it as envConfigPath in cmd/kenward/paths.go and this repeats
// the literal. Both are eight lines; if a third copy ever appears, move the rule into
// internal/setup beside the constant it already shares.
func resolveConfigPath() string {
	if v := os.Getenv("KENWARD_CONFIG"); v != "" {
		return v
	}
	if _, err := os.Stat(setup.DefaultConfigFileName); err == nil {
		return setup.DefaultConfigFileName
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "kenward", setup.DefaultConfigFileName)
	}
	return setup.DefaultConfigFileName
}
