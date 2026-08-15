package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ProbeTimeout is how long an endpoint is given to accept a connection.
//
// Three seconds, because of what the answer is used for. This is not a health check
// that gates anything: it exists so that somebody who typed monster.tial instead of
// monster.tail finds out while they are still looking at the line they typed it on.
// A machine on the household's own network accepts a connection in single-digit
// milliseconds or it is not there, and a provider's front door answers well inside
// three seconds from anywhere. Waiting longer would only lengthen the pause after
// every switched-off desktop, which is the common case rather than the exception.
const ProbeTimeout = 3 * time.Second

// Reachability is what a probe found.
type Reachability int

const (
	// Answered means the host accepted a TCP connection.
	Answered Reachability = iota
	// NoAnswer means nothing accepted a connection before the timeout. A desktop
	// that is switched off looks exactly like this, and so does a firewall.
	NoAnswer
	// Refused means something is running on that host but nothing is listening on
	// that port.
	Refused
	// Unresolved means the name does not resolve. It is the usual shape of a typo.
	Unresolved
	// BadURL means the address is not one kenward can dial at all.
	BadURL
)

// ProbeResult reports one probe.
//
// Only Answered is good news, and none of the others is an error: a household's GPU
// box is switched off most of the time, and a setup wizard that refused to record an
// endpoint it could not reach would be unusable in exactly the deployment this
// product is for. The result is shown to the operator and then accepted.
type ProbeResult struct {
	State   Reachability
	Elapsed time.Duration
	// Addr is the host:port that was dialled, for the operator to check against
	// what they meant to type.
	Addr string
	Err  error
}

// Probe reports whether an endpoint's base URL can be connected to.
type Probe func(ctx context.Context, baseURL string) ProbeResult

// Prober is the default Probe: a plain TCP connect, and nothing more.
//
// Deliberately not an HTTP request. A GET against an unknown server's /v1 is a
// request to something the operator has not agreed to talk to yet, it needs a
// credential the wizard may not have been given, and a 404 from a perfectly working
// llama.cpp would read as a failure. Whether the address is right and something is
// listening is the whole of what setup needs to know; whether the model answers is
// what `kenward doctor` is for.
type Prober struct {
	// Timeout bounds one probe. Zero means ProbeTimeout.
	Timeout time.Duration
	// Dial is the dialler. Zero means a net.Dialer, and it is injectable so the
	// timeout and failure paths can be tested without a network or a wait.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// DefaultProbe probes an endpoint with a default Prober.
func DefaultProbe(ctx context.Context, baseURL string) ProbeResult {
	return (&Prober{}).Probe(ctx, baseURL)
}

// Probe connects to the host named by a base URL.
func (p *Prober) Probe(ctx context.Context, baseURL string) ProbeResult {
	addr, err := dialAddress(baseURL)
	if err != nil {
		return ProbeResult{State: BadURL, Err: err}
	}

	timeout := p.Timeout
	if timeout == 0 {
		timeout = ProbeTimeout
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := dial(ctx, "tcp", addr)
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		return ProbeResult{State: Answered, Elapsed: elapsed, Addr: addr}
	}
	return ProbeResult{State: classify(err), Elapsed: elapsed, Addr: addr, Err: err}
}

// classify decides which of the four kinds of silence this was, because they mean
// different things to the person who just typed the address: a name that does not
// resolve is nearly always a typo, a refused connection is a machine that is up with
// the wrong port, and a timeout is usually a machine that is off.
func classify(err error) Reachability {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Unresolved
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return NoAnswer
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NoAnswer
	}
	return Refused
}

// dialAddress turns a base URL into the host:port to connect to, defaulting the
// port from the scheme.
//
// The parsing here is not a second opinion on config.Validate — that package has the
// only vote on whether a base URL is acceptable, and it gets it on the finished
// document. This is the address the dialler needs, and reporting that it cannot be
// derived is how a mistyped URL gets caught during the question rather than after
// the file is written.
func dialAddress(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL", baseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q needs to start with http:// or https://", baseURL)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%q has no host in it", baseURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// describe renders a probe result for the operator.
//
// Every line but the first says, in the same breath, that the endpoint was still
// recorded. Somebody setting this up on a laptop while the GPU machine is asleep
// downstairs must not be left thinking they have to go and switch it on before they
// can finish.
func (r ProbeResult) describe() string {
	switch r.State {
	case Answered:
		return fmt.Sprintf("  answered in %dms.", r.Elapsed.Milliseconds())
	case Unresolved:
		return fmt.Sprintf("  the name in %s could not be looked up. That is usually a typo, so it\n"+
			"  is worth a second look — but if it is a name that only resolves once the\n"+
			"  machine is up, this is fine and it has been recorded.", r.Addr)
	case Refused:
		return fmt.Sprintf("  %s is there but nothing is listening on that port. Recorded anyway;\n"+
			"  kenward will use it when it comes up.", r.Addr)
	default:
		return fmt.Sprintf("  no answer from %s within %s. If the machine is simply switched off,\n"+
			"  that is the normal case and it has been recorded; kenward will use it when\n"+
			"  it is on, and refuse rather than go elsewhere while it is not.",
			r.Addr, ProbeTimeout)
	}
}

// isLocal reports whether a base URL names a machine on the household's own
// network.
//
// It decides one thing only: which tier the wizard suggests, and whether a chain
// counts as staying in the house when the operator is asked to opt into a provider.
// It is a suggestion in the first case and a warning in the second, so a wrong guess
// costs a correction rather than a leak — the tier chain in the file is what
// actually binds, and the operator sees it before anything is written.
func isLocal(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	// A bare name with no dots in it can only be resolved by something on the local
	// network — a hosts file, mDNS, a router's DNS — so it is in the house by
	// construction. The suffixes are the ones a household actually ends up with:
	// mDNS, a Tailscale tailnet, and the conventions home routers use.
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// hostOf returns the host part of a base URL, for naming where a tier's traffic
// would actually go.
func hostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// localSuffixes are the DNS suffixes that only ever name a machine the household
// controls.
var localSuffixes = []string{".local", ".lan", ".home", ".internal", ".ts.net", ".tail", ".home.arpa"}
