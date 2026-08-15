package routing

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/BlueHeisenberg/keel/llm"
)

// Tuning for the pool. These are product decisions, not knobs: the probe
// timeout turns a powered-off machine into a ~2s skip rather than an OS-level
// connect hang, the probe TTL keeps a burst of turns from re-dialling the same
// host, and the cooldown curve keeps a flapping endpoint from being hammered
// while letting it back in within minutes.
const (
	probeTimeout    = 2 * time.Second
	probeTTL        = 10 * time.Second
	cooldownBase    = 30 * time.Second
	cooldownCeiling = 5 * time.Minute
)

// Pool routes completion requests across a fixed set of endpoints, walking the
// caller's tier chain in order. It implements Router and honours its contract
// exactly: candidates come only from tiers named in the chain, an exhausted
// chain is a *NoBackendError, and there is no code path that widens the chain.
//
// A Pool is safe for concurrent use by many conversations at once. Probe
// results, cooldown state and least-recently-used ordering are shared across
// callers; no lock is held across a network call.
type Pool struct {
	endpoints []Endpoint
	completer Completer

	// dial and now are seams for tests; production uses net.Dialer and
	// time.Now.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	now  func() time.Time

	mu       sync.Mutex
	probes   map[string]probeEntry    // keyed by host:port
	cooldown map[string]cooldownEntry // keyed by endpoint name
	lastUsed map[string]uint64        // keyed by endpoint name; 0 = never used
	seq      uint64
}

var _ Router = (*Pool)(nil)

type probeEntry struct {
	ok      bool
	expires time.Time
}

type cooldownEntry struct {
	failures int
	until    time.Time
}

// NewPool builds a Pool over the given endpoints, which are the complete set
// this pool may ever consult. Completions are produced by c; production wires
// in NewHTTPCompleter, tests inject fakes.
func NewPool(endpoints []Endpoint, c Completer) *Pool {
	d := &net.Dialer{}
	return &Pool{
		endpoints: append([]Endpoint(nil), endpoints...),
		completer: c,
		dial:      d.DialContext,
		now:       time.Now,
		probes:    make(map[string]probeEntry),
		cooldown:  make(map[string]cooldownEntry),
		lastUsed:  make(map[string]uint64),
	}
}

// Complete walks chain in order and returns the first completion produced by
// an endpoint tagged with a tier in the chain.
//
// Within a tier, endpoints in cooldown are skipped, the rest are
// connect-probed so a powered-off machine is a fast skip, and survivors are
// tried in least-recently-used order. An endpoint failure that justifies
// trying another machine (see shouldFailover) cools the endpoint and moves on;
// any other error is returned to the caller unchanged, because a rejected
// request is not a reason to try a different machine. Failover happens before
// the first token only — the wrapped Completer is non-streaming, so every
// failure it reports precedes any output.
//
// When the chain is exhausted — including an empty chain, or a chain naming
// tiers no endpoint carries — Complete returns a *NoBackendError listing every
// endpoint attempted or skipped. It never consults an endpoint outside the
// chain.
func (p *Pool) Complete(ctx context.Context, chain []string, req Request) (Completion, error) {
	var tried []string
	seen := make(map[string]bool)
	record := func(name string) {
		if !seen[name] {
			seen[name] = true
			tried = append(tried, name)
		}
	}

	for _, tier := range chain {
		if err := ctx.Err(); err != nil {
			return Completion{}, err
		}

		candidates := p.tierCandidates(ctx, tier, record)
		attempted := make(map[string]bool)
		for {
			ep, ok := p.takeLRU(candidates, attempted)
			if !ok {
				break // tier yielded nothing; fall through to the next
			}
			attempted[ep.Name] = true
			record(ep.Name)

			start := time.Now()
			comp, err := p.completer.Complete(ctx, ep, req)
			if err == nil {
				p.markSuccess(ep.Name)
				comp.Endpoint = ep.Name
				comp.Tier = tier
				comp.Latency = time.Since(start)
				return comp, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Completion{}, ctxErr
			}
			if !shouldFailover(err) {
				return Completion{}, err
			}
			p.markFailure(ep.Name)
		}
	}

	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	return Completion{}, &NoBackendError{
		Chain: append([]string(nil), chain...),
		Tried: tried,
	}
}

// shouldFailover reports whether err is a reason to try another machine,
// classified in keel/llm's error vocabulary. A *llm.TransportError qualifies —
// the bytes never completed a round trip, including a fired per-attempt
// timeout. So does a *llm.APIError with a 5xx status: the machine answered but
// the service behind it is broken. A 4xx does not — the request itself was
// rejected, and a different machine would reject it the same way — and that
// includes 429. llm.ErrInvalidRequest was never sent anywhere and falls
// through to false.
//
// An empty response (*llm.EmptyResponseError) splits on its finish reason, and
// the split is a privacy decision, not an inconsistency — do not tidy it away.
// A genuinely empty answer (no choices, an empty choice, no finish reason at
// all) is a broken endpoint and fails over with cooldown, exactly like a 5xx.
// But llm.FinishContentFilter means the model declined to answer, and a
// refusal is a final answer, not an availability problem: if routing treated
// it as an outage it would walk the rest of the tier chain offering the same
// content to every machine in turn — and for a space whose chain ends in a
// cloud tier, content a local model just declined would be handed to a
// third-party provider because a refusal was misread as a malfunction. That is
// the exact leak this package exists to prevent, so a content-filter refusal
// returns to the caller unchanged and the endpoint is not cooled: nothing is
// wrong with the machine.
//
// The check uses errors.As on the concrete type rather than reading the
// Response llm.Chat returns alongside the sentinel, so the same path works
// where no Response exists — the streaming API reports the finish reason only
// through *llm.EmptyResponseError.
func shouldFailover(err error) bool {
	if llm.IsTransport(err) {
		return true
	}
	var ee *llm.EmptyResponseError
	if errors.As(err, &ee) {
		return ee.FinishReason != llm.FinishContentFilter
	}
	var ae *llm.APIError
	if errors.As(err, &ae) {
		return ae.StatusCode >= 500
	}
	return false
}

// tierCandidates returns the endpoints eligible for tier, in configuration
// order: tagged with the tier, not in cooldown, and answering a connect probe.
// Skipped endpoints are recorded so the eventual refusal can name them.
func (p *Pool) tierCandidates(ctx context.Context, tier string, record func(string)) []Endpoint {
	var out []Endpoint
	for _, ep := range p.endpoints {
		if !hasTag(ep, tier) {
			continue
		}
		if p.inCooldown(ep.Name) {
			record(ep.Name)
			continue
		}
		if !p.probe(ctx, ep.BaseURL) {
			record(ep.Name)
			continue
		}
		out = append(out, ep)
	}
	return out
}

// takeLRU atomically picks the least-recently-used candidate not yet attempted
// in this call and marks it used, so concurrent calls rotate across endpoints
// instead of piling onto the same one. Ties (never-used endpoints) resolve in
// configuration order.
func (p *Pool) takeLRU(candidates []Endpoint, attempted map[string]bool) (Endpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var (
		best    Endpoint
		bestSeq uint64
		found   bool
	)
	for _, ep := range candidates {
		if attempted[ep.Name] {
			continue
		}
		s := p.lastUsed[ep.Name]
		if !found || s < bestSeq {
			best, bestSeq, found = ep, s, true
		}
	}
	if !found {
		return Endpoint{}, false
	}
	p.seq++
	p.lastUsed[best.Name] = p.seq
	return best, true
}

// probe reports whether the host:port behind baseURL accepts a TCP connection.
// Results — success and failure alike — are cached for probeTTL per host:port.
// The dial itself happens outside the pool lock. A dial that failed because
// ctx was cancelled is not cached: a caller giving up must not mark a healthy
// machine down for everyone else.
func (p *Pool) probe(ctx context.Context, baseURL string) bool {
	addr, err := hostPort(baseURL)
	if err != nil {
		return false
	}

	p.mu.Lock()
	if e, ok := p.probes[addr]; ok && p.now().Before(e.expires) {
		p.mu.Unlock()
		return e.ok
	}
	p.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, probeTimeout)
	conn, derr := p.dial(dctx, "tcp", addr)
	cancel()
	if conn != nil {
		conn.Close()
	}
	if derr != nil && ctx.Err() != nil {
		return false
	}
	ok := derr == nil

	p.mu.Lock()
	p.probes[addr] = probeEntry{ok: ok, expires: p.now().Add(probeTTL)}
	p.mu.Unlock()
	return ok
}

// inCooldown reports whether the named endpoint is currently cooling.
func (p *Pool) inCooldown(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cooldown[name]
	return ok && p.now().Before(e.until)
}

// markFailure records a failed attempt: the endpoint cools for cooldownBase,
// doubling per consecutive failure up to cooldownCeiling.
func (p *Pool) markFailure(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.cooldown[name]
	e.failures++
	n := e.failures
	if n > 5 {
		n = 5 // beyond this the shift exceeds the ceiling anyway
	}
	d := cooldownBase << (n - 1)
	if d > cooldownCeiling {
		d = cooldownCeiling
	}
	e.until = p.now().Add(d)
	p.cooldown[name] = e
}

// markSuccess resets the endpoint's cooldown state entirely.
func (p *Pool) markSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldown, name)
}

// hasTag reports whether ep carries the tier tag.
func hasTag(ep Endpoint, tier string) bool {
	for _, t := range ep.Tags {
		if t == tier {
			return true
		}
	}
	return false
}

// hostPort extracts the dialable host:port from an endpoint base URL, filling
// in the scheme's default port when the URL names none.
func hostPort(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", errors.New("routing: base URL has no host")
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	switch u.Scheme {
	case "http":
		return net.JoinHostPort(u.Hostname(), "80"), nil
	case "https":
		return net.JoinHostPort(u.Hostname(), "443"), nil
	default:
		return "", errors.New("routing: base URL has no port and no known scheme")
	}
}
