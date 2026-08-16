package routing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"
)

// fakeClock is a mutable clock injected as Pool.now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeDialer answers probes from a static reachability map and counts dials
// per address.
type fakeDialer struct {
	mu    sync.Mutex
	up    map[string]bool
	dials map[string]int
}

func newFakeDialer(up map[string]bool) *fakeDialer {
	return &fakeDialer{up: up, dials: make(map[string]int)}
}

func (d *fakeDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dials[addr]++
	up := d.up[addr]
	d.mu.Unlock()
	if !up {
		return nil, errors.New("dial refused")
	}
	c1, c2 := net.Pipe()
	c2.Close()
	return c1, nil
}

func (d *fakeDialer) count(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials[addr]
}

// fakeCompleter records call order and answers per endpoint name.
type fakeCompleter struct {
	mu    sync.Mutex
	calls []string
	fn    map[string]func() (Completion, error)
}

func newFakeCompleter() *fakeCompleter {
	return &fakeCompleter{fn: make(map[string]func() (Completion, error))}
}

func (f *fakeCompleter) Complete(_ context.Context, ep Endpoint, _ Request) (Completion, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ep.Name)
	fn := f.fn[ep.Name]
	f.mu.Unlock()
	if fn == nil {
		return Completion{Text: "ok"}, nil
	}
	return fn()
}

func (f *fakeCompleter) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func ep(name string, port int, tags ...string) Endpoint {
	return Endpoint{
		Name:    name,
		BaseURL: fmt.Sprintf("http://%s.test:%d/v1", name, port),
		Model:   "m",
		Tags:    tags,
		Timeout: time.Second,
	}
}

func addr(e Endpoint) string {
	a, err := hostPort(e.BaseURL)
	if err != nil {
		panic(err)
	}
	return a
}

func newTestPool(t *testing.T, endpoints []Endpoint, fc Completer, fd *fakeDialer, clock *fakeClock) *Pool {
	t.Helper()
	p := NewPool(endpoints, fc)
	p.dial = fd.dial
	p.now = clock.Now
	return p
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTierFallthrough(t *testing.T) {
	monster := ep("monster", 8000, "local")
	cloud := ep("cloud", 443, "cloud")
	fd := newFakeDialer(map[string]bool{addr(monster): false, addr(cloud): true})
	fc := newFakeCompleter()
	p := newTestPool(t, []Endpoint{monster, cloud}, fc, fd, newFakeClock())

	comp, err := p.Complete(context.Background(), []string{"local", "cloud"}, Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Endpoint != "cloud" || comp.Tier != "cloud" {
		t.Fatalf("got endpoint=%q tier=%q, want cloud/cloud", comp.Endpoint, comp.Tier)
	}
	if got := fc.callNames(); !equalStrings(got, []string{"cloud"}) {
		t.Fatalf("completer calls = %v, want [cloud]", got)
	}
}

// TestOutOfChainIsolation is the security test: a pool that contains a cloud
// endpoint, asked for chain ["local"] with no local endpoint reachable, must
// refuse — and the cloud endpoint's server must receive zero requests. Ditto
// for an empty chain and a chain naming a tier no endpoint carries. This test
// uses the real httpCompleter and real dialer so nothing is faked between the
// pool and the wire.
func TestOutOfChainIsolation(t *testing.T) {
	var hits int32
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"leaked"}}]}`)
	}))
	defer cloudSrv.Close()

	// A local endpoint whose port refuses connections: listen, note the
	// address, close.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localURL := "http://" + l.Addr().String()
	l.Close()

	p := NewPool([]Endpoint{
		{Name: "monster", BaseURL: localURL, Model: "m", Tags: []string{"local"}, Timeout: time.Second},
		{Name: "openrouter", BaseURL: cloudSrv.URL, Model: "m", Tags: []string{"cloud"}, Timeout: time.Second},
	}, NewHTTPCompleter(nil, nil, nil))

	req := Request{Messages: []Message{{Role: "user", Content: "private"}}}
	for _, chain := range [][]string{{"local"}, nil, {"gpu"}} {
		_, err := p.Complete(context.Background(), chain, req)
		var nbe *NoBackendError
		if !errors.As(err, &nbe) {
			t.Fatalf("chain %v: got %v, want *NoBackendError", chain, err)
		}
		if !equalStrings(nbe.Chain, chain) {
			t.Fatalf("chain %v: NoBackendError.Chain = %v", chain, nbe.Chain)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("cloud endpoint received %d requests, want 0", n)
	}
}

func TestNoBackendErrorContents(t *testing.T) {
	cooled := ep("cooled", 8001, "local")
	dead := ep("dead", 8002, "local")
	failing := ep("failing", 8003, "local")
	outside := ep("outside", 8004, "cloud")
	fd := newFakeDialer(map[string]bool{
		addr(cooled): true, addr(dead): false, addr(failing): true, addr(outside): true,
	})
	fc := newFakeCompleter()
	fc.fn["failing"] = func() (Completion, error) {
		return Completion{}, &llm.TransportError{Op: "connect", Endpoint: "failing", Err: errors.New("refused")}
	}
	p := newTestPool(t, []Endpoint{cooled, dead, failing, outside}, fc, fd, newFakeClock())
	p.markFailure("cooled") // pre-cool

	_, err := p.Complete(context.Background(), []string{"local"}, Request{})
	var nbe *NoBackendError
	if !errors.As(err, &nbe) {
		t.Fatalf("got %v, want *NoBackendError", err)
	}
	if !equalStrings(nbe.Chain, []string{"local"}) {
		t.Fatalf("Chain = %v, want [local]", nbe.Chain)
	}
	if !equalStrings(nbe.Tried, []string{"cooled", "dead", "failing"}) {
		t.Fatalf("Tried = %v, want [cooled dead failing]", nbe.Tried)
	}
	if got := fc.callNames(); !equalStrings(got, []string{"failing"}) {
		t.Fatalf("completer calls = %v, want [failing]", got)
	}
}

func TestCooldownGrowthCeilingAndReset(t *testing.T) {
	clock := newFakeClock()
	p := newTestPool(t, nil, newFakeCompleter(), newFakeDialer(nil), clock)

	want := []time.Duration{
		30 * time.Second, 60 * time.Second, 120 * time.Second,
		240 * time.Second, 300 * time.Second, 300 * time.Second,
	}
	for i, w := range want {
		p.markFailure("e")
		e := p.cooldown["e"]
		if got := e.until.Sub(clock.Now()); got != w {
			t.Fatalf("failure %d: cooldown %v, want %v", i+1, got, w)
		}
	}

	p.markSuccess("e")
	if _, ok := p.cooldown["e"]; ok {
		t.Fatal("cooldown not cleared on success")
	}
	p.markFailure("e")
	if got := p.cooldown["e"].until.Sub(clock.Now()); got != 30*time.Second {
		t.Fatalf("post-reset cooldown %v, want 30s", got)
	}
}

func TestCooldownSkipsAndReadmits(t *testing.T) {
	e := ep("flaky", 8000, "local")
	clock := newFakeClock()
	fd := newFakeDialer(map[string]bool{addr(e): true})
	fc := newFakeCompleter()
	fail := true
	fc.fn["flaky"] = func() (Completion, error) {
		if fail {
			return Completion{}, &llm.TransportError{Op: "connect", Endpoint: "flaky", Err: errors.New("boom")}
		}
		return Completion{Text: "ok"}, nil
	}
	p := newTestPool(t, []Endpoint{e}, fc, fd, clock)
	ctx := context.Background()

	if _, err := p.Complete(ctx, []string{"local"}, Request{}); err == nil {
		t.Fatal("want error from failing endpoint")
	}
	// Within the 30s cooldown the endpoint is skipped, not attempted.
	_, err := p.Complete(ctx, []string{"local"}, Request{})
	var nbe *NoBackendError
	if !errors.As(err, &nbe) || !equalStrings(nbe.Tried, []string{"flaky"}) {
		t.Fatalf("got %v, want NoBackendError trying [flaky]", err)
	}
	if got := len(fc.callNames()); got != 1 {
		t.Fatalf("completer called %d times, want 1 (cooldown must skip)", got)
	}

	clock.Advance(31 * time.Second)
	fail = false
	comp, err := p.Complete(ctx, []string{"local"}, Request{})
	if err != nil || comp.Endpoint != "flaky" {
		t.Fatalf("after cooldown: comp=%+v err=%v", comp, err)
	}
}

func TestProbeCache(t *testing.T) {
	up := ep("up", 8000, "local")
	down := ep("down", 8001, "slow")
	clock := newFakeClock()
	fd := newFakeDialer(map[string]bool{addr(up): true, addr(down): false})
	fc := newFakeCompleter()
	p := newTestPool(t, []Endpoint{up, down}, fc, fd, clock)
	ctx := context.Background()

	// Two completes inside the TTL: one dial.
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(ctx, []string{"local"}, Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := fd.count(addr(up)); got != 1 {
		t.Fatalf("dials within TTL = %d, want 1", got)
	}

	// Failure is cached too.
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(ctx, []string{"slow"}, Request{}); err == nil {
			t.Fatal("want NoBackendError for down endpoint")
		}
	}
	if got := fd.count(addr(down)); got != 1 {
		t.Fatalf("failure dials within TTL = %d, want 1", got)
	}

	// Past the TTL both are re-probed.
	clock.Advance(11 * time.Second)
	if _, err := p.Complete(ctx, []string{"local"}, Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, []string{"slow"}, Request{}); err == nil {
		t.Fatal("want NoBackendError for down endpoint")
	}
	if got := fd.count(addr(up)); got != 2 {
		t.Fatalf("dials after TTL = %d, want 2", got)
	}
	if got := fd.count(addr(down)); got != 2 {
		t.Fatalf("failure dials after TTL = %d, want 2", got)
	}
}

func TestLRURotation(t *testing.T) {
	a := ep("a", 8000, "local")
	b := ep("b", 8001, "local")
	fd := newFakeDialer(map[string]bool{addr(a): true, addr(b): true})
	fc := newFakeCompleter()
	p := newTestPool(t, []Endpoint{a, b}, fc, fd, newFakeClock())

	for i := 0; i < 4; i++ {
		if _, err := p.Complete(context.Background(), []string{"local"}, Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := fc.callNames(); !equalStrings(got, []string{"a", "b", "a", "b"}) {
		t.Fatalf("call order = %v, want [a b a b]", got)
	}
}

func TestAPIErrorDoesNotFailOver(t *testing.T) {
	a := ep("a", 8000, "local")
	b := ep("b", 8001, "local")
	newPool := func(t *testing.T, aErr error) (*Pool, *fakeCompleter) {
		fd := newFakeDialer(map[string]bool{addr(a): true, addr(b): true})
		fc := newFakeCompleter()
		fc.fn["a"] = func() (Completion, error) { return Completion{}, aErr }
		return newTestPool(t, []Endpoint{a, b}, fc, fd, newFakeClock()), fc
	}

	t.Run("400 stops", func(t *testing.T) {
		p, fc := newPool(t, &llm.APIError{StatusCode: 400, Endpoint: "a", Type: "invalid_request_error"})
		_, err := p.Complete(context.Background(), []string{"local"}, Request{})
		var ae *llm.APIError
		if !errors.As(err, &ae) || ae.StatusCode != 400 {
			t.Fatalf("got %v, want the 400 APIError back", err)
		}
		if got := fc.callNames(); !equalStrings(got, []string{"a"}) {
			t.Fatalf("calls = %v, want [a] only", got)
		}
		if p.inCooldown("a") {
			t.Fatal("a 400 must not cool the endpoint")
		}
	})

	t.Run("429 stops", func(t *testing.T) {
		p, fc := newPool(t, &llm.APIError{StatusCode: 429, Endpoint: "a"})
		_, err := p.Complete(context.Background(), []string{"local"}, Request{})
		var ae *llm.APIError
		if !errors.As(err, &ae) || ae.StatusCode != 429 {
			t.Fatalf("got %v, want the 429 APIError back", err)
		}
		if got := fc.callNames(); !equalStrings(got, []string{"a"}) {
			t.Fatalf("calls = %v, want [a] only", got)
		}
	})

	t.Run("connection refusal fails over", func(t *testing.T) {
		p, fc := newPool(t, &llm.TransportError{Op: "connect", Endpoint: "a", Err: errors.New("refused")})
		comp, err := p.Complete(context.Background(), []string{"local"}, Request{})
		if err != nil || comp.Endpoint != "b" {
			t.Fatalf("comp=%+v err=%v, want success via b", comp, err)
		}
		if got := fc.callNames(); !equalStrings(got, []string{"a", "b"}) {
			t.Fatalf("calls = %v, want [a b]", got)
		}
		if !p.inCooldown("a") {
			t.Fatal("connection failure must cool the endpoint")
		}
	})

	t.Run("5xx fails over", func(t *testing.T) {
		p, fc := newPool(t, &llm.APIError{StatusCode: 502, Endpoint: "a"})
		comp, err := p.Complete(context.Background(), []string{"local"}, Request{})
		if err != nil || comp.Endpoint != "b" {
			t.Fatalf("comp=%+v err=%v, want success via b", comp, err)
		}
		if got := fc.callNames(); !equalStrings(got, []string{"a", "b"}) {
			t.Fatalf("calls = %v, want [a b]", got)
		}
	})
}

// TestEndpointTimeoutCoolsAndFailsOver pins the timeout-attribution trap: the
// per-attempt deadline must ride llm.Endpoint.Timeout, not a
// context.WithTimeout wrapped around the call. Wrapped, the expiry would be
// attributed to the caller and come back as a bare context.DeadlineExceeded,
// and the pool would decline to cool down the endpoint that just timed out —
// silently disabling cooldown for exactly the machines that most deserve it.
// This test runs the real httpCompleter against a real slow server and
// asserts both halves: the turn fails over to the healthy endpoint, and the
// slow endpoint enters cooldown.
func TestEndpointTimeoutCoolsAndFailsOver(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"quick"}}]}`)
	}))
	defer fast.Close()

	p := NewPool([]Endpoint{
		{Name: "slow", BaseURL: slow.URL, Model: "m", Tags: []string{"local"}, Timeout: 50 * time.Millisecond},
		{Name: "fast", BaseURL: fast.URL, Model: "m", Tags: []string{"local"}, Timeout: time.Second},
	}, NewHTTPCompleter(nil, nil, nil))

	comp, err := p.Complete(context.Background(), []string{"local"},
		Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("endpoint timeout surfaced as caller cancellation: %v", err)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Endpoint != "fast" || comp.Text != "quick" {
		t.Fatalf("comp = %+v, want failover to fast", comp)
	}
	if !p.inCooldown("slow") {
		t.Fatal("the endpoint that timed out must be cooling")
	}
}

// emptyVsHealthyPool builds a pool over the real httpCompleter with an
// endpoint serving body verbatim and a healthy sibling in the same tier that
// counts the requests reaching it. Configuration order puts "empty" first, so
// LRU tries it first.
func emptyVsHealthyPool(t *testing.T, body string) (p *Pool, healthyHits *int32) {
	t.Helper()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(empty.Close)

	var hits int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(healthy.Close)

	p = NewPool([]Endpoint{
		{Name: "empty", BaseURL: empty.URL, Model: "m", Tags: []string{"local"}, Timeout: time.Second},
		{Name: "healthy", BaseURL: healthy.URL, Model: "m", Tags: []string{"local"}, Timeout: time.Second},
	}, NewHTTPCompleter(nil, nil, nil))
	return p, &hits
}

// TestEmptyResponseDoesNotFailOver pins the content-filter half of the
// empty-response rule, and the reasoning matters more than the mechanics: a
// model that declined to answer (finish_reason "content_filter") produced a
// final answer, not an availability failure. If the pool failed over on it,
// the refusal would be misread as an outage and the same content offered to
// every machine down the tier chain — for a chain ending in a cloud tier, that
// is routing handing a third-party provider content a local model just
// declined, the exact leak this product exists to prevent. So it returns to
// the caller unchanged, the healthy sibling is never consulted, and the
// endpoint is not cooled: nothing is wrong with the machine. This behaviour is
// permanent; its mirror for genuinely empty responses is
// TestEmptyResponseFailsOverWhenGenuinelyEmpty.
func TestEmptyResponseDoesNotFailOver(t *testing.T) {
	p, healthyHits := emptyVsHealthyPool(t,
		`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`)

	_, err := p.Complete(context.Background(), []string{"local"},
		Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, llm.ErrEmptyResponse) {
		t.Fatalf("got %v, want llm.ErrEmptyResponse returned to the caller", err)
	}
	var ee *llm.EmptyResponseError
	if !errors.As(err, &ee) || ee.FinishReason != llm.FinishContentFilter {
		t.Fatalf("got %v, want *llm.EmptyResponseError carrying FinishContentFilter for the layers above", err)
	}
	if n := atomic.LoadInt32(healthyHits); n != 0 {
		t.Fatalf("healthy sibling received %d requests, want 0 (a refusal must not walk the chain)", n)
	}
	if p.inCooldown("empty") {
		t.Fatal("a content-filter refusal must not cool the endpoint: nothing is wrong with the machine")
	}
}

// TestEmptyResponseFailsOverWhenGenuinelyEmpty is the mirror: an empty answer
// with no content-filter finish reason is a malfunction — no choices at all,
// or an empty choice from a model that claims it stopped normally — and a
// broken endpoint deserves failover and cooldown, exactly like a 5xx.
func TestEmptyResponseFailsOverWhenGenuinelyEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no choices", `{"choices":[]}`},
		{"empty choice claiming normal stop",
			`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, healthyHits := emptyVsHealthyPool(t, tc.body)

			comp, err := p.Complete(context.Background(), []string{"local"},
				Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil {
				t.Fatalf("Complete: %v, want failover to the healthy sibling", err)
			}
			if comp.Endpoint != "healthy" || comp.Text != "ok" {
				t.Fatalf("comp = %+v, want the healthy sibling's completion", comp)
			}
			if n := atomic.LoadInt32(healthyHits); n != 1 {
				t.Fatalf("healthy sibling received %d requests, want 1", n)
			}
			if !p.inCooldown("empty") {
				t.Fatal("a genuinely empty response must cool the endpoint")
			}
		})
	}
}

func TestConcurrentComplete(t *testing.T) {
	e1 := ep("e1", 8000, "local")
	e2 := ep("e2", 8001, "local", "cloud")
	e3 := ep("e3", 8002, "cloud")
	fd := newFakeDialer(map[string]bool{addr(e1): true, addr(e2): true, addr(e3): true})
	fc := newFakeCompleter()
	// e3 always fails with a connection-class error, exercising cooldown and
	// failover under contention; e2 always rescues the cloud tier.
	fc.fn["e3"] = func() (Completion, error) {
		return Completion{}, &llm.TransportError{Op: "connect", Endpoint: "e3", Err: errors.New("boom")}
	}
	p := newTestPool(t, []Endpoint{e1, e2, e3}, fc, fd, newFakeClock())

	chains := [][]string{{"local"}, {"cloud"}, {"local", "cloud"}, {"nope", "local"}}
	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 64; i++ {
		chain := chains[i%len(chains)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Complete(context.Background(), chain, Request{}); err != nil {
				errCh <- fmt.Errorf("chain %v: %w", chain, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
