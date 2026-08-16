package config

import (
	"fmt"
	"net"
	"strings"
)

// Exposure is how far the admin dashboard's listener reaches. It is stated separately
// from the bind address because the two answer different questions and both are worth
// asking: bind is what the socket does, exposure is what the household was told.
//
// They are checked against each other rather than derived from one another. A bind of
// 0.0.0.0 under exposure: loopback is not a configuration kenward will start with —
// silently believing a dashboard is unreachable when it is on every interface is the
// exact failure this pair exists to prevent.
type Exposure string

const (
	// ExposureLoopback is the default and the only exposure that needs no further
	// decision: the socket is on 127.0.0.1 and nothing off this machine can reach it.
	ExposureLoopback Exposure = "loopback"
	// ExposureTailnet is a listener on one interface of a tailnet or VPN — an
	// address that is already authenticated and encrypted by the network under it.
	// It is the recommended way to reach the dashboard from another machine.
	ExposureTailnet Exposure = "tailnet"
	// ExposureLAN is a listener on the household's own network. It requires TLS:
	// a LAN is not a trust boundary, and an admin password crossing one in the clear
	// is an admin password anyone on the wifi has.
	ExposureLAN Exposure = "lan"
)

// DefaultDashboardBind is where the dashboard listens when nothing says otherwise.
//
// Loopback, and a port high enough not to collide with anything a household is likely
// to be running. The rule the whole feature rests on is that the server does not exist
// unless somebody configured it, and the second rule is that when it does exist it is
// on this machine only until somebody says otherwise in as many words.
const DefaultDashboardBind = "127.0.0.1:8770"

// DashboardConfig configures the admin dashboard's HTTP server.
//
// The zero value is off, which is what a household that has never opened the dashboard
// gets and what every configuration written before it existed means.
type DashboardConfig struct {
	// Enabled turns the listener on. Off, `kenward run` starts no server at all and
	// no port is opened.
	Enabled bool `yaml:"enabled"`
	// Bind is the host:port the listener takes. Empty means DefaultDashboardBind.
	Bind string `yaml:"bind"`
	// Exposure is what the operator chose, and what the privacy statement and
	// `kenward doctor` report. Empty means ExposureLoopback.
	Exposure Exposure `yaml:"exposure"`
	// TLSCertFile and TLSKeyFile are the PEM files the listener serves TLS from.
	// Both are required under ExposureLAN and optional otherwise. The dashboard
	// generates a self-signed pair at the moment LAN exposure is chosen, and shows
	// its fingerprint once so it can be checked against the browser's warning.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
}

// BindAddr is the address the listener will actually take.
func (d DashboardConfig) BindAddr() string {
	if strings.TrimSpace(d.Bind) == "" {
		return DefaultDashboardBind
	}
	return strings.TrimSpace(d.Bind)
}

// ExposureOrDefault is the exposure this configuration means.
func (d DashboardConfig) ExposureOrDefault() Exposure {
	if d.Exposure == "" {
		return ExposureLoopback
	}
	return d.Exposure
}

// TLS reports whether the listener serves HTTPS.
func (d DashboardConfig) TLS() bool { return d.TLSCertFile != "" && d.TLSKeyFile != "" }

// Loopback reports whether the bind address can only be reached from this machine.
//
// It is the question the login rate limiter and the privacy statement both ask, and it
// is answered from the address rather than from the declared exposure: a wrong
// exposure is a claim, and this is the fact.
func (d DashboardConfig) Loopback() bool {
	host, _, err := net.SplitHostPort(d.BindAddr())
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// A name. "localhost" is the only one worth trusting without a lookup, and
		// a lookup here would make a configuration's meaning depend on DNS.
		return strings.EqualFold(host, "localhost")
	}
	return ip.IsLoopback()
}

// validateDashboard checks the block against itself. It runs whether or not the
// dashboard is enabled: a wrong value stored against the day somebody turns it on is a
// wrong value that will be turned on.
func (c *Config) validateDashboard(p *problems) {
	d := c.Dashboard

	switch d.ExposureOrDefault() {
	case ExposureLoopback, ExposureTailnet, ExposureLAN:
	default:
		p.addf("dashboard.exposure: %q is not an exposure; use %q, %q or %q",
			d.Exposure, ExposureLoopback, ExposureTailnet, ExposureLAN)
		return
	}

	host, port, err := net.SplitHostPort(d.BindAddr())
	if err != nil {
		p.addf("dashboard.bind: %q is not a host:port address (%v); write it as %s",
			d.BindAddr(), err, DefaultDashboardBind)
		return
	}
	if strings.TrimSpace(port) == "" {
		p.addf("dashboard.bind: %q names no port; write it as %s", d.BindAddr(), DefaultDashboardBind)
	}

	ip := net.ParseIP(strings.Trim(host, "[]"))
	switch d.ExposureOrDefault() {
	case ExposureLoopback:
		if !d.Loopback() {
			// The two disagree, and the disagreement is the whole reason both are
			// written down. Refusing is the only safe reading: a household told
			// its dashboard is on this machine only, listening on every interface,
			// is a household with an admin login on the internet and no idea.
			p.addf("dashboard.exposure is %q but dashboard.bind is %q, which is not a loopback address; "+
				"either bind to 127.0.0.1 or say which exposure this really is (%q or %q)",
				ExposureLoopback, d.BindAddr(), ExposureTailnet, ExposureLAN)
		}
	case ExposureTailnet:
		if ip != nil && ip.IsUnspecified() {
			p.addf("dashboard.exposure is %q but dashboard.bind is %q, which is every interface on this "+
				"machine rather than the tailnet's one; bind to the tailnet address itself",
				ExposureTailnet, d.BindAddr())
		}
	case ExposureLAN:
		if !d.TLS() {
			// Not a warning. A LAN is not a trust boundary, and the password that
			// unlocks every member's private memory is the one thing that must not
			// cross one in the clear.
			p.addf("dashboard.exposure is %q, which requires TLS: set dashboard.tls_cert_file and "+
				"dashboard.tls_key_file, or let the dashboard generate a self-signed pair when you "+
				"choose this exposure", ExposureLAN)
		}
	}

	if (d.TLSCertFile == "") != (d.TLSKeyFile == "") {
		p.addf("dashboard: tls_cert_file and tls_key_file must be given together; one without the other serves nothing")
	}
}

// DashboardSummary is the one line `kenward doctor` and the dashboard's own overview
// both print. It is here rather than in either of them because it is the same claim
// twice, and two copies of a claim about where a port is listening is how one of them
// becomes wrong.
func (d DashboardConfig) DashboardSummary() string {
	if !d.Enabled {
		return "admin dashboard: off — no port is opened"
	}
	scheme := "http"
	if d.TLS() {
		scheme = "https"
	}
	return fmt.Sprintf("admin dashboard: %s on %s://%s", d.ExposureOrDefault(), scheme, d.BindAddr())
}
