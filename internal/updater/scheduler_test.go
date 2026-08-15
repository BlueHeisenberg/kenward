package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keelupdate "github.com/BlueHeisenberg/keel/update"
)

const (
	manifestURL = "https://updates.test/manifest.json"
	artifactURL = "https://updates.test/kenward-next"
)

var (
	oldBinary = []byte("kenward old binary contents\n")
	newBinary = []byte("kenward new binary contents\n")
)

// countingRT is the fake HTTP layer: every request the updater would put on
// the network lands here and is counted, per URL and in total.
type countingRT struct {
	mu     sync.Mutex
	total  int
	byURL  map[string]int
	handle func(req *http.Request) *http.Response
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.total++
	if c.byURL == nil {
		c.byURL = make(map[string]int)
	}
	c.byURL[req.URL.String()]++
	handle := c.handle
	c.mu.Unlock()
	if handle == nil {
		return httpResponse(req, http.StatusNotFound, nil), nil
	}
	return handle(req), nil
}

func (c *countingRT) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *countingRT) requestsTo(url string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byURL[url]
}

func httpResponse(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

// fakeConsent records every ask and answers with a fixed decision.
type fakeConsent struct {
	decision keelupdate.Decision
	err      error

	calls int
	req   keelupdate.ConsentRequest
}

func (f *fakeConsent) RequestConsent(_ context.Context, req keelupdate.ConsentRequest) (keelupdate.Decision, error) {
	f.calls++
	f.req = req
	return f.decision, f.err
}

// fakeDrain records when it was called and what the target binary held at
// that moment, so tests can prove the drain happened before the swap.
type fakeDrain struct {
	target string
	err    error

	calls         int
	targetAtDrain []byte
}

func (f *fakeDrain) Drain(context.Context) error {
	f.calls++
	if b, err := os.ReadFile(f.target); err == nil {
		f.targetAtDrain = b
	}
	return f.err
}

// grantTicks is the fake clock: it lets the Run loop through n cycles and
// then reports cancellation, exactly as a cancelled context would.
func grantTicks(n int) waitFunc {
	return func(context.Context, time.Duration) error {
		if n == 0 {
			return context.Canceled
		}
		n--
		return nil
	}
}

type fixture struct {
	t      *testing.T
	target string
	priv   ed25519.PrivateKey
	rt     *countingRT
	opts   Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	target := filepath.Join(t.TempDir(), "kenward.exe")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	rt := &countingRT{}
	return &fixture{
		t:      t,
		target: target,
		priv:   priv,
		rt:     rt,
		opts: Options{
			Channel:        keelupdate.ChannelEdge,
			CheckInterval:  time.Hour,
			ManifestURL:    manifestURL,
			Keys:           []ed25519.PublicKey{pub},
			CurrentVersion: "v1.0.0",
			ConsentTimeout: time.Second,
			HTTPClient:     &http.Client{Transport: rt},
			targetPath:     target,
			skipPreflight:  true, // tests swap text fixtures, which cannot be executed
		},
	}
}

func (f *fixture) build() *Scheduler {
	f.t.Helper()
	s, err := New(f.opts)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return s
}

// serve wires the fake HTTP layer to answer the manifest URL with a signed
// manifest advertising rel on the edge channel, and the artifact URL with
// newBinary.
func (f *fixture) serve(rel keelupdate.Release) {
	f.t.Helper()
	body := f.signedManifest(rel)
	f.rt.handle = func(req *http.Request) *http.Response {
		switch req.URL.String() {
		case manifestURL:
			return httpResponse(req, http.StatusOK, body)
		case artifactURL:
			return httpResponse(req, http.StatusOK, newBinary)
		default:
			return httpResponse(req, http.StatusNotFound, nil)
		}
	}
}

func (f *fixture) signedManifest(rel keelupdate.Release) []byte {
	f.t.Helper()
	m := keelupdate.Manifest{
		Schema:      keelupdate.ManifestSchema,
		GeneratedAt: time.Now().UTC(),
		Channels:    map[string]keelupdate.Release{"edge": rel, "stable": rel},
	}
	body, err := keelupdate.SignManifest(m, keelupdate.Signer{KeyID: "test", Key: f.priv})
	if err != nil {
		f.t.Fatalf("sign manifest: %v", err)
	}
	return body
}

func release(version string) keelupdate.Release {
	sum := sha256.Sum256(newBinary)
	return keelupdate.Release{
		Version:     version,
		Notes:       "release notes for " + version,
		PublishedAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
		Artifacts: map[string]keelupdate.Artifact{
			keelupdate.Platform(): {URL: artifactURL, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(newBinary))},
		},
	}
}

func (f *fixture) targetContents() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.target)
	if err != nil {
		f.t.Fatalf("read target: %v", err)
	}
	return b
}

func (f *fixture) journalExists() bool {
	_, err := os.Stat(f.target + ".update.json")
	return err == nil
}

func TestOffChannelIsGenuinelyOff(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v1.0.1")) // a tempting update is available; off must not even look
	f.opts.Channel = keelupdate.ChannelOff
	f.opts.wait = grantTicks(5)
	s := f.build()

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run on an off channel: %v, want nil", err)
	}
	if got := f.rt.requests(); got != 0 {
		t.Fatalf("off channel made %d HTTP requests, want exactly 0", got)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("off channel modified the target binary")
	}
}

func TestPatchAppliesWithoutConsent(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v1.0.1"))
	consent := &fakeConsent{decision: keelupdate.DecisionDeclined} // would refuse if asked
	f.opts.Consent = consent
	f.opts.wait = grantTicks(1)
	s := f.build()

	err := s.Run(context.Background())
	if !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("Run = %v, want ErrRestartPending (no Restart hook wired)", err)
	}
	if consent.calls != 0 {
		t.Fatalf("a patch release asked for consent %d times, want 0", consent.calls)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("target was not swapped to the new binary")
	}
	if !f.journalExists() {
		t.Fatalf("no journal after the swap; Resume would have nothing to finish")
	}
}

func TestMajorVersionRequiresConsent(t *testing.T) {
	t.Run("declined is not applied and not re-asked", func(t *testing.T) {
		f := newFixture(t)
		f.serve(release("v2.0.0"))
		consent := &fakeConsent{decision: keelupdate.DecisionDeclined}
		f.opts.Consent = consent
		f.opts.wait = grantTicks(3)
		s := f.build()

		if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled after the granted ticks", err)
		}
		if consent.calls != 1 {
			t.Fatalf("household was asked %d times about the same declined version, want exactly 1", consent.calls)
		}
		if from, to := consent.req.From.String(), consent.req.To.String(); from != "v1.0.0" || to != "v2.0.0" {
			t.Fatalf("consent asked about %s -> %s, want v1.0.0 -> v2.0.0", from, to)
		}
		if consent.req.Notes == "" {
			t.Fatalf("consent request carried no release notes")
		}
		if consent.req.SecuritySensitive {
			t.Fatalf("a plain major bump was flagged security-sensitive in the consent request")
		}
		if !bytes.Equal(f.targetContents(), oldBinary) {
			t.Fatalf("a declined major version was applied anyway")
		}
		if got := f.rt.requestsTo(artifactURL); got != 0 {
			t.Fatalf("artifact was downloaded %d times before consent, want 0 — consent gates the download", got)
		}
	})

	t.Run("approved applies", func(t *testing.T) {
		f := newFixture(t)
		f.serve(release("v2.0.0"))
		consent := &fakeConsent{decision: keelupdate.DecisionApproved}
		f.opts.Consent = consent
		f.opts.wait = grantTicks(1)
		s := f.build()

		if err := s.Run(context.Background()); !errors.Is(err, keelupdate.ErrRestartPending) {
			t.Fatalf("Run = %v, want ErrRestartPending", err)
		}
		if consent.calls != 1 {
			t.Fatalf("consent asked %d times, want 1", consent.calls)
		}
		if !bytes.Equal(f.targetContents(), newBinary) {
			t.Fatalf("an approved major version was not applied")
		}
	})

	t.Run("no consent path wired refuses and does not spin", func(t *testing.T) {
		f := newFixture(t)
		f.serve(release("v2.0.0"))
		f.opts.Consent = nil
		f.opts.wait = grantTicks(3)
		s := f.build()

		if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
		if !bytes.Equal(f.targetContents(), oldBinary) {
			t.Fatalf("a consent-requiring release applied with no consent path wired")
		}
		if got := f.rt.requestsTo(artifactURL); got != 0 {
			t.Fatalf("artifact fetched %d times for a release that can never apply, want 0", got)
		}
	})
}

func TestSecuritySensitiveRequiresConsentRegardlessOfVersion(t *testing.T) {
	f := newFixture(t)
	rel := release("v1.0.1") // a patch bump — the flag alone must gate it
	rel.SecuritySensitive = true
	f.serve(rel)
	consent := &fakeConsent{decision: keelupdate.DecisionDeclined}
	f.opts.Consent = consent
	f.opts.wait = grantTicks(2)
	s := f.build()

	if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if consent.calls != 1 {
		t.Fatalf("security-flagged patch asked for consent %d times, want exactly 1 (declined is remembered)", consent.calls)
	}
	// The entire point of the flag: the consent prompt must be able to say
	// "this release changes security-relevant behaviour", so the request
	// must carry the fact to the Consenter.
	if !consent.req.SecuritySensitive {
		t.Fatalf("consent request did not carry SecuritySensitive; the household would be asked without being told why")
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("a declined security-sensitive release was applied anyway")
	}
}

func TestUnansweredConsentIsRefusedAndRetriedNextCycle(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v2.0.0"))
	consent := &fakeConsent{decision: keelupdate.DecisionUnanswered}
	f.opts.Consent = consent
	f.opts.wait = grantTicks(3)
	s := f.build()

	if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	// Silence is a refusal but not a decision: unlike a decline, the
	// household is asked again on every cycle.
	if consent.calls != 3 {
		t.Fatalf("unanswered consent was asked %d times over 3 cycles, want 3", consent.calls)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("an unanswered consent request applied the update")
	}
}

func TestFailedCheckRetriesNextTickAndDoesNotStopScheduler(t *testing.T) {
	f := newFixture(t)
	good := f.signedManifest(release("v1.0.1"))
	var manifestServed atomic.Int32
	f.rt.handle = func(req *http.Request) *http.Response {
		switch req.URL.String() {
		case manifestURL:
			if manifestServed.Add(1) == 1 {
				return httpResponse(req, http.StatusInternalServerError, nil)
			}
			return httpResponse(req, http.StatusOK, good)
		case artifactURL:
			return httpResponse(req, http.StatusOK, newBinary)
		default:
			return httpResponse(req, http.StatusNotFound, nil)
		}
	}
	f.opts.wait = grantTicks(2)
	s := f.build()

	// The first cycle's check fails; the scheduler must survive it and apply
	// on the second cycle.
	if err := s.Run(context.Background()); !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("Run = %v, want ErrRestartPending on the cycle after a failed check", err)
	}
	if got := f.rt.requestsTo(manifestURL); got != 2 {
		t.Fatalf("manifest fetched %d times, want 2 (one failure, one retry)", got)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("update was not applied after the check recovered")
	}
}

func TestDrainIsAwaitedBeforeTheSwap(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v1.0.1"))
	drain := &fakeDrain{target: f.target}
	f.opts.Drain = drain
	f.opts.wait = grantTicks(1)
	s := f.build()

	if err := s.Run(context.Background()); !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("Run = %v, want ErrRestartPending", err)
	}
	if drain.calls != 1 {
		t.Fatalf("drain called %d times, want 1", drain.calls)
	}
	if !bytes.Equal(drain.targetAtDrain, oldBinary) {
		t.Fatalf("at drain time the target already held %q; the swap must wait for the drain", drain.targetAtDrain)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("swap did not follow the drain")
	}
}

func TestDrainFailureAbortsWithNothingChanged(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v1.0.1"))
	drain := &fakeDrain{target: f.target, err: errors.New("turns still in flight")}
	f.opts.Drain = drain
	f.opts.wait = grantTicks(2)
	s := f.build()

	if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled — a failed drain must not stop the scheduler", err)
	}
	if drain.calls != 2 {
		t.Fatalf("drain attempted %d times over 2 cycles, want 2 (retried, not abandoned)", drain.calls)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("a failed drain still changed the binary on disk")
	}
}

func TestDrainedButFailedApplyRestartsTheHousehold(t *testing.T) {
	f := newFixture(t)
	f.serve(release("v1.0.1"))
	drain := &fakeDrain{target: f.target}
	var restarts int
	f.opts.Drain = drain
	f.opts.Restart = func(context.Context) error { restarts++; return nil }
	f.opts.wait = grantTicks(1)
	s := f.build()

	// Make the swap itself fail AFTER the drain: keel retains the previous
	// binary via an atomic copy through "<target>.prev.tmp", and a directory
	// squatting on that path fails the copy. By then the household has
	// already been drained for an update that will not happen.
	if err := os.Mkdir(f.target+".prev.tmp", 0o755); err != nil {
		t.Fatalf("plant swap obstruction: %v", err)
	}

	if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if drain.calls != 1 {
		t.Fatalf("drain called %d times, want 1", drain.calls)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("a failed swap still changed the binary")
	}
	if restarts != 1 {
		t.Fatalf("household was drained and then restarted %d times, want 1 — a drained household with no restart is silent until a human notices", restarts)
	}
}

func TestLosingTheLockSkipsQuietlyWithoutDraining(t *testing.T) {
	// Since keel v0.4.0 the cross-process lock is taken before the drain:
	// losing the race to a sibling must cost nothing — no drain, no restart,
	// no silence.
	f := newFixture(t)
	f.serve(release("v1.0.1"))
	drain := &fakeDrain{target: f.target}
	var restarts int
	f.opts.Drain = drain
	f.opts.Restart = func(context.Context) error { restarts++; return nil }
	f.opts.wait = grantTicks(1)
	s := f.build()

	// A fresh sibling lock.
	if err := os.WriteFile(f.target+".update.lock", []byte("{}"), 0o644); err != nil {
		t.Fatalf("plant lock: %v", err)
	}

	if err := s.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if drain.calls != 0 {
		t.Fatalf("household was drained %d times while a sibling held the lock, want 0", drain.calls)
	}
	if restarts != 0 {
		t.Fatalf("household was restarted %d times for a cycle that was skipped, want 0", restarts)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("a locked-out apply still changed the binary")
	}
}

func TestStableDelayIsJudgedByTheInjectedClock(t *testing.T) {
	// Exercises keel's published clock seam (Config.Now): a release younger
	// than the stable delay is refused, and the same release applies once
	// the clock passes the threshold — no wall-clock sleeping involved.
	f := newFixture(t)
	now := time.Now().UTC()
	rel := release("v1.0.1")
	rel.PublishedAt = now // just published
	f.serve(rel)

	current := now
	f.opts.Channel = keelupdate.ChannelStable
	f.opts.StableDelay = 72 * time.Hour
	f.opts.now = func() time.Time { return current }
	ticks := 0
	f.opts.wait = func(context.Context, time.Duration) error {
		ticks++
		switch ticks {
		case 1: // first cycle: the release is brand new, so it must wait
			return nil
		case 2: // second cycle: the delay has elapsed
			current = now.Add(80 * time.Hour)
			return nil
		default:
			return context.Canceled
		}
	}
	s := f.build()

	if err := s.Run(context.Background()); !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("Run = %v, want ErrRestartPending once the stable delay elapsed", err)
	}
	if got := f.rt.requestsTo(artifactURL); got != 1 {
		t.Fatalf("artifact fetched %d times, want 1 — the first cycle must refuse the too-young release", got)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("update was not applied after the stable delay elapsed")
	}
}

// applyPatch swaps the fixture's target to v1.0.1 and leaves the journal in
// place, exactly as a scheduled apply followed by a process exit would.
func applyPatch(t *testing.T, f *fixture) {
	t.Helper()
	f.serve(release("v1.0.1"))
	f.opts.wait = grantTicks(1)
	s := f.build()
	if err := s.Run(context.Background()); !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("apply: Run = %v, want ErrRestartPending", err)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("apply: target not swapped")
	}
}

func TestHealthCheckFailureTriggersRollback(t *testing.T) {
	f := newFixture(t)
	applyPatch(t, f)

	// "Restart": the new binary comes up and resumes the pending update, but
	// its lore probe fails — the new build cannot serve, so keel must roll
	// back to the retained previous binary.
	f.opts.CurrentVersion = "v1.0.1"
	f.opts.Health = HealthProbes{
		Lore:     func(context.Context) error { return errors.New("mcp handshake failed") },
		Telegram: func(context.Context) error { return nil },
	}
	fresh := f.build()

	rep, err := fresh.Resume(context.Background())
	if !errors.Is(err, keelupdate.ErrRestartPending) {
		t.Fatalf("Resume = %v, want ErrRestartPending (rollback needs a restart onto the old binary)", err)
	}
	if rep.Outcome != keelupdate.OutcomeRolledBack {
		t.Fatalf("Resume outcome = %v, want OutcomeRolledBack", rep.Outcome)
	}
	if !bytes.Equal(f.targetContents(), oldBinary) {
		t.Fatalf("target was not restored to the previous binary after a failed health check")
	}

	// The old binary starts again and finishes the bookkeeping.
	f.opts.CurrentVersion = "v1.0.0"
	f.opts.Health = HealthProbes{}
	old := f.build()
	rep, err = old.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume on the restored binary: %v", err)
	}
	if rep.Outcome != keelupdate.OutcomeRolledBack {
		t.Fatalf("restored binary's Resume outcome = %v, want OutcomeRolledBack", rep.Outcome)
	}
	if f.journalExists() {
		t.Fatalf("journal not cleaned up after a completed rollback")
	}
}

func TestHealthCheckSuccessCommits(t *testing.T) {
	f := newFixture(t)
	applyPatch(t, f)

	var loreProbed, telegramProbed bool
	f.opts.CurrentVersion = "v1.0.1"
	f.opts.Health = HealthProbes{
		Lore:     func(context.Context) error { loreProbed = true; return nil },
		Telegram: func(context.Context) error { telegramProbed = true; return nil },
	}
	fresh := f.build()

	rep, err := fresh.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if rep.Outcome != keelupdate.OutcomeCommitted {
		t.Fatalf("Resume outcome = %v, want OutcomeCommitted", rep.Outcome)
	}
	if !loreProbed || !telegramProbed {
		t.Fatalf("health decided without its probes: lore=%t telegram=%t", loreProbed, telegramProbed)
	}
	if !bytes.Equal(f.targetContents(), newBinary) {
		t.Fatalf("committed update lost the new binary")
	}
	if f.journalExists() {
		t.Fatalf("journal not removed after commit")
	}
}

// TestHealthNeverConsultsEndpoints guards the invariant hooks.go states:
// health is the process's own condition — lore responds, Telegram authorises
// — and NEVER the reachability of the household's inference endpoints, which
// are legitimately powered off much of the time. If a future change makes
// health probe endpoints, the listener below sees a connection and this test
// fails; that change would put every installation one sleeping machine away
// from an endless rollback loop, so do not "fix" the test.
func TestHealthNeverConsultsEndpoints(t *testing.T) {
	// A live stand-in for a household inference endpoint, counting every
	// connection it receives.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()

	f := newFixture(t)
	var loreProbed, telegramProbed bool
	f.opts.Health = HealthProbes{
		Lore:     func(context.Context) error { loreProbed = true; return nil },
		Telegram: func(context.Context) error { telegramProbed = true; return nil },
	}
	s := f.build()

	// Note there is no way to hand the endpoint's address (ln.Addr()) to the
	// scheduler at all: HealthProbes has no field for it. That absence is the
	// design, and this test is its tripwire.
	if err := s.health(context.Background()); err != nil {
		t.Fatalf("health = %v, want nil: both of the process's own probes pass", err)
	}
	if !loreProbed || !telegramProbed {
		t.Fatalf("health skipped its own probes: lore=%t telegram=%t", loreProbed, telegramProbed)
	}
	if got := conns.Load(); got != 0 {
		t.Fatalf("health opened %d connections to an inference endpoint, want 0", got)
	}
	if got := f.rt.requests(); got != 0 {
		t.Fatalf("health made %d HTTP requests, want 0 — health is local judgement, not a network survey", got)
	}
}

func TestNilSchedulerIsSafe(t *testing.T) {
	// Construction failure is a warning, never a refusal to start; the nil
	// scheduler the caller is left with must be inert, not explosive.
	var s *Scheduler
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("nil Run = %v, want nil", err)
	}
	rep, err := s.Resume(context.Background())
	if err != nil || rep.Outcome != keelupdate.OutcomeNone {
		t.Fatalf("nil Resume = (%v, %v), want (OutcomeNone, nil)", rep.Outcome, err)
	}
}

func TestConstructionRefusesAnUnverifiableConfiguration(t *testing.T) {
	// No trusted keys on a live channel: New must refuse — an updater that
	// cannot verify a signature must not exist — and the caller runs on
	// without auto-update.
	_, err := New(Options{Channel: keelupdate.ChannelStable, ManifestURL: manifestURL})
	if err == nil {
		t.Fatalf("New with no trusted keys on a live channel succeeded; it must refuse")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "updater:") {
		t.Fatalf("construction error %q does not carry the package's prefix", msg)
	}
}
