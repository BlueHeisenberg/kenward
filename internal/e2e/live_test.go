//go:build integration

package e2e

// The one test in this module that has no fake below Telegram.
//
// Every other test here — and every test in every other package — stops at a
// seam: a scripted memory.Memory, an httptest server pretending to be a model.
// That structurally cannot see what a real dependency does differently from the
// fake written to stand in for it. This file therefore builds the production
// wiring with a real memory.Client over a real embedded lore store and a real
// routing.Pool over a real OpenAI-compatible endpoint, and fakes only Telegram,
// for which no bot token exists.
//
// Two observers sit in the path and neither replaces a dependency:
//
//   - spyMemory wraps the real client and records which spaces each call named,
//     then delegates. Every answer comes from lore.
//   - loreCLI reads and writes the same store through the `lore` binary, out of
//     process. It is how this suite checks kenward's writes with something other
//     than the library kenward wrote them with.
//   - recordingProxy is an HTTP relay in front of the real endpoint: it records
//     the request body and forwards it unchanged. Every completion comes from
//     the model.
//
// Assertions about "what reached the model" are therefore assertions about real
// bytes on a real wire, and assertions about "which space was searched" are
// about real calls to a real store.
//
// Run it with:
//
//	KENWARD_E2E_ENDPOINT=http://192.168.1.20:8000/v1 \
//	KENWARD_E2E_MODEL=monster \
//	go test -tags integration -run TestLive -v ./internal/e2e/
//
// The store is not configured, because the suite creates its own and throws it
// away again; see newLoreStore. Only the model endpoint is external. KENWARD_LORE_BIN
// names the `lore` binary if it is not on PATH.
//
// **Without an endpoint this fails; it does not skip.** See liveEnv for why, and for
// the one way to waive it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// liveTimeout bounds every poll here. A real 0.5b model on a real machine and a
// real lore subprocess are both slower than loopback httptest, and a tool-call
// turn is two model round trips plus a button.
const liveTimeout = 2 * time.Minute

// The environment this suite reads, named once because the same two variables are
// quoted in the failure below, in the file header and in docs/TESTING.md.
const (
	endpointEnv = "KENWARD_E2E_ENDPOINT"
	modelEnv    = "KENWARD_E2E_MODEL"
	loreBinEnv  = "KENWARD_LORE_BIN"
	// liveSkipEnv is the only way to run this package's tagged tests without an
	// endpoint. It has to be typed out, and that is the whole point of it.
	liveSkipEnv = "KENWARD_E2E_SKIP"
)

// live gathers what this suite runs against: the endpoint and model come from the
// environment, the store from newLoreStore. The build tag is what keeps `go test
// ./...` and CI away from a model; nothing in CI passes `-tags integration` at all.
type live struct {
	loreBin  string
	loreHome string
	private  domain.SpaceID
	shared   domain.SpaceID
	endpoint string
	model    string
}

// liveEnv reads the endpoint under test, and **fails rather than skips** when there
// is not one.
//
// This is the opposite of what the file used to do and the reversal is deliberate.
// `go test` prints nothing at all for a package whose tests skip — no reason, no
// count, just `ok` and a duration — so a live suite that skips on a missing endpoint
// reports exactly what a live suite that passed reports, having reached no model, no
// store and no wire. That is not a hypothetical: it is how two runs were believed on
// the day this was written.
//
// Nothing automated pays for the change. The `integration` tag already keeps this file
// out of `go test ./...`, and no CI workflow in this repository passes the tag, so the
// only thing that can reach this line is a person who asked for it. Somebody running
// the whole tagged suite for a different piece of equipment — the real-Podman test,
// say — waives it with KENWARD_E2E_SKIP, which is loud in the other direction: they
// have to have typed the words.
func liveEnv(t *testing.T) live {
	t.Helper()
	l := live{
		loreBin:  os.Getenv(loreBinEnv),
		endpoint: os.Getenv(endpointEnv),
		model:    os.Getenv(modelEnv),
	}
	if l.loreBin == "" {
		l.loreBin = "lore"
	}
	if l.endpoint != "" && l.model != "" {
		t.Logf("live suite: %s against %s (lore store created and destroyed by the test)", l.model, l.endpoint)
		return l
	}
	if v := os.Getenv(liveSkipEnv); v != "" {
		t.Skipf("%s=%s: the live suite was waived. No model, no store and no wire were exercised by it.", liveSkipEnv, v)
	}
	t.Fatalf("the live suite has no endpoint: set %s (e.g. http://192.168.1.20:8000/v1) and %s (e.g. monster).\n"+
		"This is a failure and not a skip on purpose — `go test` prints nothing for a package that skips, so skipping here reports `ok` having tested nothing. "+
		"Set %s=1 to waive it deliberately.", endpointEnv, modelEnv, liveSkipEnv)
	return live{}
}

// newLoreStore gives this run a lore store it owns outright: a fresh LORE_HOME
// under t.TempDir(), holding two spaces created here and nothing else. The
// temporary directory takes it away again, so no state survives a run and no run
// can see another's.
//
// This replaces pointing the suite at spaces kept for the purpose in a persistent
// store, which does not work. lore has a delete now — D-040 exposed the propagating
// tombstone `internal/store.DeleteEntry` always wrote, and Undo calls it — but it is
// not tidy-up machinery for a suite: it deletes one entry, there is still no way to
// remove a space, and a run that cleaned up after itself would be one crash away from
// leaving the store dirtier than it found it. Before it existed, every run added
// entries no later run could remove, and after eight runs the store held eight
// near-identical greenhouse entries that retrieval could not tell apart — the suite
// had made itself fail. A test whose store degrades every time it runs is measuring
// its own history, and owning the store is what stops that rather than deleting.
//
// Owning the store costs this file nothing it was built to have. The SQLite
// database and the full-text search are still the real ones, which is the whole
// claim in the comment at the top; only somebody's accumulated personal entries
// are absent, and those were never the subject.
//
// It is built with lore's Go API rather than its command line. It used to be
// `lore init` and two `lore space create`s with the new id scraped out of the
// line each one printed — the last place in this repository that recovered a
// value from lore's prose. The ids come back typed now, and the suite still
// watches the store from outside the process through loreCLI, which is the part
// that was ever load-bearing here.
func newLoreStore(t *testing.T, l live) live {
	t.Helper()
	l.loreHome = t.TempDir()
	if _, err := lore.Init(l.loreHome, "kenward-e2e"); err != nil {
		t.Fatalf("lore.Init: %v", err)
	}
	// Opened, used and closed before anything else touches this home: one store
	// per home per process, and the client under test opens its own below.
	st, err := lore.Open(lore.Options{Home: l.loreHome})
	if err != nil {
		t.Fatalf("lore.Open: %v", err)
	}
	defer st.Close()
	mk := func(name string) domain.SpaceID {
		sp, err := st.CreateSpace(t.Context(), name, lore.Shared)
		if err != nil {
			t.Fatalf("creating space %s: %v", name, err)
		}
		return domain.SpaceID(sp.ID)
	}
	l.private = mk("kenward-e2e-private")
	l.shared = mk("kenward-e2e-shared")
	return l
}

// testWriter sends a logger's output to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// -----------------------------------------------------------------------------
// observers
// -----------------------------------------------------------------------------

// spyMemory records what was asked of the real store and answers from it.
type spyMemory struct {
	inner memory.Memory
	mu    sync.Mutex
	calls []memCall
	wrote []writeRecord
}

func (s *spyMemory) record(c memCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *spyMemory) recorded() []memCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *spyMemory) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	s.record(memCall{Op: "search", Spaces: append([]domain.SpaceID(nil), q.Spaces...), Text: q.Text})
	return s.inner.Search(ctx, q)
}

func (s *spyMemory) Get(ctx context.Context, space domain.SpaceID, id string) (memory.Entry, error) {
	s.record(memCall{Op: "get", Spaces: []domain.SpaceID{space}, Title: id})
	return s.inner.Get(ctx, space, id)
}

func (s *spyMemory) Put(ctx context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	s.record(memCall{Op: "put", Spaces: []domain.SpaceID{space}, Title: d.Title})
	e, err := s.inner.Put(ctx, space, d)
	s.mu.Lock()
	s.wrote = append(s.wrote, writeRecord{draft: d, entry: e, err: err})
	s.mu.Unlock()
	return e, err
}

// writeRecord is one completed Put: what kenward asked to store, what lore
// answered, and whether it failed. The call record alone cannot tell a write
// that landed from one that was attempted — which is the whole question when the
// store is real.
type writeRecord struct {
	draft memory.Draft
	entry memory.Entry
	err   error
}

func (s *spyMemory) writes() []writeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]writeRecord, len(s.wrote))
	copy(out, s.wrote)
	return out
}

func (s *spyMemory) Share(ctx context.Context, from, to domain.SpaceID, id string) (memory.Entry, error) {
	s.record(memCall{Op: "share", Spaces: []domain.SpaceID{from, to}, Title: id})
	return s.inner.Share(ctx, from, to, id)
}

func (s *spyMemory) Delete(ctx context.Context, space domain.SpaceID, id string) error {
	s.record(memCall{Op: "delete", Spaces: []domain.SpaceID{space}, Title: id})
	return s.inner.Delete(ctx, space, id)
}

func (s *spyMemory) Close() error { return s.inner.Close() }

var _ memory.Memory = (*spyMemory)(nil)

// searchedIn lists, in call order, every space a search named.
func searchedIn(calls []memCall) []domain.SpaceID {
	var out []domain.SpaceID
	for _, c := range calls {
		if c.Op == "search" {
			out = append(out, c.Spaces...)
		}
	}
	return distinctSpaces(out)
}

// touchedIn lists every space named by any call of any kind.
func touchedIn(calls []memCall) []domain.SpaceID {
	var out []domain.SpaceID
	for _, c := range calls {
		out = append(out, c.Spaces...)
	}
	return out
}

// recordingProxy relays /chat/completions to the real endpoint, keeping a copy
// of every request body and of every response body. It is not a fake provider: it
// adds nothing and answers nothing, so the completion the pool reads is the model's
// own.
//
// The response copy is what makes an assertion about rendering possible at all. What
// the member reads is transport.Markdown applied to the model's text, and a test that
// only sees the member's end cannot tell "the converter ran" from "the model happened
// to write no Markdown this time" — which is the difference between an assertion and
// a coin toss against a model nobody controls.
type recordingProxy struct {
	srv    *httptest.Server
	target string

	mu       sync.Mutex
	requests []wireRequest
	raw      [][]byte
	replies  [][]byte
}

func newRecordingProxy(t *testing.T, target string) *recordingProxy {
	t.Helper()
	p := &recordingProxy{target: strings.TrimSuffix(target, "/")}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

// baseURL is what goes into endpoints[].base_url. The proxy mirrors the target's
// path, so the /v1 the caller configured is preserved.
func (p *recordingProxy) baseURL() string { return p.srv.URL }

func (p *recordingProxy) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req wireRequest
	// A body that does not decode is still recorded raw; the decode is for
	// convenience, not for gatekeeping.
	_ = json.Unmarshal(body, &req)
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.raw = append(p.raw, body)
	p.mu.Unlock()

	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.target+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Accept-Encoding")
	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// Buffered rather than streamed, which costs nothing here: kenward sets
	// stream=false, so the whole completion arrives as one JSON document anyway.
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	p.mu.Lock()
	p.replies = append(p.replies, answer)
	p.mu.Unlock()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(answer)
}

// lastReplyText is the assistant text of the most recent completion, exactly as the
// endpoint wrote it and before anything in kenward has touched it.
func (p *recordingProxy) lastReplyText(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.replies) == 0 {
		t.Fatalf("the endpoint returned no completions")
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	raw := p.replies[len(p.replies)-1]
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding the endpoint's completion: %v\n%s", err, raw)
	}
	if len(body.Choices) == 0 {
		t.Fatalf("the endpoint's completion has no choices:\n%s", raw)
	}
	return body.Choices[0].Message.Content
}

func (p *recordingProxy) all() []wireRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]wireRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

func (p *recordingProxy) rawAll() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.raw))
	copy(out, p.raw)
	return out
}

func (p *recordingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *recordingProxy) last(t *testing.T) wireRequest {
	t.Helper()
	all := p.all()
	if len(all) == 0 {
		t.Fatalf("the endpoint received no requests")
	}
	return all[len(all)-1]
}

// -----------------------------------------------------------------------------
// the live household
// -----------------------------------------------------------------------------

type liveOptions struct {
	tiers []string
	// endpointDown points the configured endpoint at a closed port, modelling
	// the household machine being asleep. The proxy is still built, so a test can
	// prove nothing reached the real model.
	endpointDown bool
	// persona is the household's, written into the configuration as an admin
	// would have written it in the wizard. Empty is the default and renders the
	// prompt this suite has always sent.
	persona config.PersonaConfig
}

type liveHarness struct {
	t     *testing.T
	l     live
	dir   string
	tr    *transport.Fake
	mem   *spyMemory
	proxy *recordingProxy
	sup   supervisor.Supervisor

	startErr chan error
	stopOnce sync.Once
}

func newLiveHarness(t *testing.T, l live, opts liveOptions) *liveHarness {
	t.Helper()
	if len(opts.tiers) == 0 {
		opts.tiers = []string{"local"}
	}

	dir := t.TempDir()
	proxy := newRecordingProxy(t, l.endpoint)
	url := proxy.baseURL()
	if opts.endpointDown {
		url = deadEndpointURL(t)
	}

	cfgPath := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(cfgPath, []byte(liveConfigYAML(l, dir, url, opts)), 0o600); err != nil {
		t.Fatalf("writing configuration: %v", err)
	}
	lookupEnv := fakeEnv(map[string]string{botTokenEnv: "123456:telegram-token"})
	cfg, err := config.LoadWithEnv(cfgPath, lookupEnv)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}

	// The real client, over a real lore store — the embedded library, not a
	// subprocess.
	client, err := memory.NewClient(memory.Config{LoreHome: l.loreHome})
	if err != nil {
		t.Fatalf("building the lore client: %v", err)
	}
	mem := &spyMemory{inner: client}

	sessions, err := session.NewManager(session.ModeSimple, session.NewMemStore(),
		session.WithKDFParams(vault.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}))
	if err != nil {
		t.Fatalf("building session manager: %v", err)
	}
	t.Cleanup(sessions.Close)
	ctx := context.Background()
	if err := sessions.Provision(ctx, "david", passphrase); err != nil {
		t.Fatalf("provisioning david: %v", err)
	}
	if err := sessions.Unlock(ctx, "david", passphrase); err != nil {
		t.Fatalf("unlocking david: %v", err)
	}

	tr := transport.NewFake()
	// Greedy sampling. Production leaves Temperature unset and takes the
	// provider's default, which for ollama is 0.8, and a 0.5b model sampling at
	// 0.8 decides whether to emit a tool call slightly differently every run —
	// measured, the capture scenario failed about one run in five that way, with
	// nothing in kenward varying between them. Nothing here is faked or relaxed by
	// pinning it: the model still makes the decision on its own, it just makes the
	// same one twice. A model-in-the-loop test that samples is a coin toss with a
	// two-minute timeout attached.
	greedy := 0.0
	sup, err := supervisor.NewSimple(cfg, supervisor.SimpleOptions{
		Transport: tr,
		Memory:    mem,
		Sessions:  sessions,
		LookupEnv: lookupEnv,
		Unit:      assistant.Options{Temperature: &greedy},
		// Real dependencies degrade in ways a fake never does, and the paths that
		// swallow a failure into a polite sentence for the member log it and
		// nothing else. Without this the interesting failure is invisible.
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("building supervisor: %v", err)
	}

	h := &liveHarness{t: t, l: l, dir: dir, tr: tr, mem: mem, proxy: proxy, sup: sup, startErr: make(chan error, 1)}
	t.Cleanup(func() {
		h.stop()
		_ = tr.Close()
		_ = client.Close()
		// What the model actually said, but only when something went wrong. A failure
		// here is nearly always the model having decided differently — no tool call, a
		// call with invented arguments, an empty completion — and none of that is
		// visible from the member's end, which is where every assertion in this file
		// looks. Printing it unconditionally would bury the run in JSON.
		if t.Failed() {
			for i, raw := range proxy.replies {
				t.Logf("completion %d from the endpoint:\n%s", i+1, raw)
			}
		}
	})
	return h
}

func (h *liveHarness) start() {
	h.t.Helper()
	go func() { h.startErr <- h.sup.Start(context.Background()) }()
}

func (h *liveHarness) stop() {
	h.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
		defer cancel()
		_ = h.sup.Stop(ctx)
		select {
		case <-h.startErr:
		case <-time.After(liveTimeout):
			h.t.Error("supervisor Start did not return after Stop")
		}
	})
}

func (h *liveHarness) sentTo(chatID int64) []transport.Outbound {
	var out []transport.Outbound
	for _, o := range h.tr.Sent() {
		if o.ChatID == chatID {
			out = append(out, o)
		}
	}
	return out
}

// waitForLive polls cond until it holds or liveTimeout elapses. It is waitFor
// with the patience a real model and a real subprocess need.
func waitForLive(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !waitUpTo(liveTimeout, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitUpTo is waitForLive without the fatal, reporting whether cond held inside d.
//
// It exists for the one thing in this file that a real model decides and a fake never
// did: whether a turn produces a tool call at all. "It did not" is a finding to print
// with the completion that shows why, not a harness fault to die on — and dying on it
// costs the full liveTimeout of nothing happening before anybody is told anything.
func waitUpTo(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func (h *liveHarness) waitForReply(chatID int64, n int) []transport.Outbound {
	h.t.Helper()
	waitForLive(h.t, fmt.Sprintf("%d message(s) to chat %d", n, chatID), func() bool {
		return len(h.sentTo(chatID)) >= n
	})
	return h.sentTo(chatID)
}

func liveConfigYAML(l live, dataDir, url string, opts liveOptions) string {
	var b strings.Builder
	tiers := strings.Join(opts.tiers, ", ")
	fmt.Fprintf(&b, "mode: simple\n")
	fmt.Fprintf(&b, "data_dir: '%s'\n", dataDir)
	fmt.Fprintf(&b, "household:\n  name: Ashfield\n")
	fmt.Fprintf(&b, "  shared_space: %s\n", l.shared)
	fmt.Fprintf(&b, "  group_chat_id: %d\n", groupChatID)
	fmt.Fprintf(&b, "  tiers: [%s]\n", tiers)
	if !opts.persona.IsZero() {
		// Written as YAML rather than set on the decoded configuration, so the
		// persona takes the same path an admin's answers do: through the file,
		// through config.Load, through validation.
		b.WriteString("  persona:\n")
		if opts.persona.Language != "" {
			fmt.Fprintf(&b, "    language: %q\n", opts.persona.Language)
		}
		if opts.persona.Tone != "" {
			fmt.Fprintf(&b, "    tone: %q\n", opts.persona.Tone)
		}
		if opts.persona.Character != "" {
			fmt.Fprintf(&b, "    character: %q\n", opts.persona.Character)
		}
	}
	fmt.Fprintf(&b, "telegram:\n  bot_token_env: %s\n", botTokenEnv)
	fmt.Fprintf(&b, "members:\n  - id: david\n    name: David\n")
	fmt.Fprintf(&b, "    telegram_id: %d\n", davidTelegramID)
	fmt.Fprintf(&b, "    private_space: %s\n    tiers: [%s]\n", l.private, tiers)
	fmt.Fprintf(&b, "endpoints:\n  - name: attic\n    base_url: '%s'\n    model: '%s'\n    tags: [local]\n    timeout: 120s\n", url, l.model)
	fmt.Fprintf(&b, "memory:\n  search_limit: 8\n")
	fmt.Fprintf(&b, "session:\n  idle_timeout: 30m\n")
	fmt.Fprintf(&b, "capture:\n  max_proposals_per_turn: 1\n")
	fmt.Fprintf(&b, "update:\n  channel: stable\n  check_interval: 6h\n")
	return b.String()
}

// looksSpanish is a deliberately crude check, and crude is the right shape for it.
//
// The question it has to answer is "did the household get their own language", not
// "is this grammatical Spanish", and the second question cannot be asked of a
// quantised model without the test becoming a test of the endpoint. So it looks for
// the marks and function words that separate Spanish from English at a glance —
// accents, inverted punctuation, and the handful of words every Spanish sentence has
// — and accepts anything with a couple of them.
func looksSpanish(s string) bool {
	lower := strings.ToLower(s)
	hits := 0
	for _, mark := range []string{"á", "é", "í", "ó", "ú", "ñ", "¿", "¡"} {
		if strings.Contains(lower, mark) {
			hits++
		}
	}
	for _, word := range []string{" el ", " la ", " los ", " las ", " de ", " que ", " es ", " no ", " se ", " para "} {
		if strings.Contains(" "+lower+" ", word) {
			hits++
		}
	}
	return hits >= 2
}

// loreCLI runs the lore command-line tool against the same store, in a fresh
// process with no connection to the client under test. It is the independent
// witness: what it reports, kenward did not tell it.
func loreCLI(t *testing.T, l live, stdin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.loreBin, args...)
	cmd.Env = append(os.Environ(), "LORE_HOME="+l.loreHome)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lore %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// spaceName resolves a space id to the display name lore prints, from `lore
// spaces`. It fails the test if the id is not listed at all — a space that is not
// there is a configuration fault worth hearing about, not a skipped assertion.
func spaceName(t *testing.T, l live, id domain.SpaceID) string {
	t.Helper()
	for _, line := range strings.Split(loreCLI(t, l, "", "spaces"), "\n") {
		if !strings.Contains(line, string(id)) {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			return f[0]
		}
	}
	t.Fatalf("space %s is not listed by `lore spaces`", id)
	return ""
}

// stamp is a token unique to this run. The store is fresh, so it is no longer
// needed to tell this run's entries from an earlier run's; it is kept because a
// token that appears nowhere else is what makes "this reached the model" an
// assertion about retrieval rather than about a word the prompt was always going
// to contain.
func stamp() string { return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000) }

// -----------------------------------------------------------------------------
// the scenarios
// -----------------------------------------------------------------------------

func TestLive(t *testing.T) {
	l := newLoreStore(t, liveEnv(t))

	// 1. A direct message goes out to a real model and comes back, having
	// searched the real store in the member's private space and the household's
	// shared one.
	t.Run("DirectMessageRoundTrips", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{})
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID, "Say hello in one short sentence.", false)
		sent := h.waitForReply(davidChatID, 1)

		if strings.TrimSpace(sent[0].Text) == "" {
			t.Fatal("the model's reply was empty")
		}
		t.Logf("model reply: %q", sent[0].Text)

		searched := searchedIn(h.mem.recorded())
		if len(searched) != 2 {
			t.Fatalf("searched %v, want exactly the member's two spaces", searched)
		}
		if !containsSpace(searched, l.private) || !containsSpace(searched, l.shared) {
			t.Errorf("searched %v, want %s and %s", searched, l.private, l.shared)
		}
		// Order is asserted where it is guaranteed: the prompt. Retrieval itself
		// is concurrent, one goroutine per space.
		system := h.proxy.last(t).System()
		privateAt := strings.Index(system, "David's private memory")
		sharedAt := strings.Index(system, "the household's shared memory")
		switch {
		case privateAt < 0:
			t.Error("system prompt has no private memory section")
		case sharedAt < 0:
			t.Error("system prompt has no shared memory section")
		case privateAt > sharedAt:
			t.Error("shared memory is rendered before private memory; scope order is primary first")
		}
		if got := h.proxy.last(t).Model; got != l.model {
			t.Errorf("endpoint was asked for model %q, want %q", got, l.model)
		}
	})

	// 1b. A household that chose Spanish gets a conversation in Spanish, from a
	// real model, over two turns, with the memory boundary intact.
	//
	// This is the one scenario in the suite where the assertion has to be about
	// the reply rather than about the wire, because "the persona reached the
	// model" is already proved by a unit test and is not what anybody doubts. What
	// is doubted is whether a household can actually hold a conversation in their
	// own language, and only a real model can answer that.
	//
	// It is deliberately tolerant about *how* Spanish the answer is and strict
	// about what surrounds it. A quantised 27B will occasionally drop an English
	// word; that is a fact about the model and not a defect in kenward, and a test
	// that failed on it would be a test of the endpoint. What is not tolerated is
	// the persona having moved anything it may not move: the scope disclosure and
	// the capture rules are checked on the wire, in English, unchanged.
	t.Run("SpanishPersonaHoldsAConversation", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{persona: config.PersonaConfig{
			Language: "Spanish",
			Tone:     "cálido pero breve",
		}})
		h.start()

		h.tr.InjectText(davidChatID, davidTelegramID, "¿Qué día sacamos la basura?", false)
		first := h.waitForReply(davidChatID, 1)
		t.Logf("turn 1 (¿Qué día sacamos la basura?): %q", first[0].Text)

		system := h.proxy.last(t).System()
		if !strings.Contains(system, "Language:\n  Spanish") {
			t.Errorf("the household's language never reached the prompt:\n%s", system)
		}
		// The persona is rendered, so the anti-persona paragraph is not; and the
		// rules the persona may not touch are still there, in English, verbatim.
		if strings.Contains(system, "not a personality") {
			t.Error("the prompt asks for a tone and denies having one in the same breath")
		}
		if !strings.Contains(system, "This is a private conversation with David.") {
			t.Error("a persona displaced the scope disclosure")
		}
		if !strings.Contains(system, "propose storing it by calling the remember tool") {
			t.Error("a persona displaced the capture instructions")
		}

		// Second turn, so this is a conversation rather than a single completion:
		// the history ring carries the first exchange back into the prompt.
		h.tr.InjectText(davidChatID, davidTelegramID, "¿Y el reciclaje?", false)
		second := h.waitForReply(davidChatID, 2)
		t.Logf("turn 2 (¿Y el reciclaje?): %q", second[1].Text)

		if strings.TrimSpace(second[1].Text) == "" {
			t.Fatal("the second turn came back empty")
		}
		carried := false
		for _, m := range h.proxy.last(t).Messages {
			if strings.Contains(m.Content, "¿Qué día sacamos la basura?") {
				carried = true
			}
		}
		if !carried {
			t.Error("the second turn did not carry the first one; this was two completions, not a conversation")
		}
		if !looksSpanish(second[1].Text) {
			t.Errorf("the second reply does not look like Spanish, and the household asked for Spanish: %q", second[1].Text)
		}
	})

	// 2. A fact written to the real store by an independent `lore put` is found
	// by the real search and reaches the real model.
	//
	// The assertion is on the request that went to the endpoint, not on the
	// reply. A 0.5b model paraphrases, refuses and hallucinates freely, so
	// "the reply contains the fact" would be a test of the model. "The fact was
	// on the wire" is a test of kenward.
	//
	// The question is asked the way a member would ask it, filler words and all,
	// and that is the point of asking it here. lore's search is a conjunctive
	// full-text match, so a query term absent from an entry excludes it: passing
	// the member's raw message through as the query — which kenward used to do —
	// retrieved nothing from a store holding exactly that sentence, because "what"
	// is not in it. The assistant now searches each content word and unions the
	// hits, and this is where that meets the real store rather than a fake.
	//
	// The token appears in the entry and nowhere else. The question does not name
	// it, so the only way it can reach the system prompt is retrieval: the member
	// asks for a thing they do not know, in the words they would use, and the
	// answer has to be found. An earlier version of this test named the token in
	// the question as well, to outrank the identically worded entries every
	// previous run had left in the store — a workaround for a store that is now
	// created fresh, and one that weakened the assertion while it lasted.
	t.Run("RetrievalReachesThePrompt", func(t *testing.T) {
		token := "quillfeather-" + stamp()
		fact := "The greenhouse thermostat override phrase is " + token + "."
		loreCLI(t, l, fact,
			"put", "-space", string(l.private), "-domain", "kenward/e2e",
			"-title", "Greenhouse override phrase",
			"-confidence", "validated", "-body-file", "-")

		h := newLiveHarness(t, l, liveOptions{})
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID, "hey, what is the greenhouse thermostat override phrase?", false)
		h.waitForReply(davidChatID, 1)

		system := h.proxy.last(t).System()
		if !strings.Contains(system, token) {
			t.Errorf("the fact written to %s never reached the model; system prompt was:\n%s", l.private, system)
		}
		if !strings.Contains(system, "David's private memory") {
			t.Error("system prompt has no private memory section")
		}
	})

	// 3. A group message may reach the shared space and nothing else. The check
	// is on which spaces were named, not on what came back: a group turn that
	// searched a private space and happened to find nothing has already broken
	// the invariant.
	t.Run("GroupMessageIsScopedToShared", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{})
		h.start()
		h.tr.InjectText(groupChatID, davidTelegramID, "Say hello to the household in one short sentence.", true)
		h.waitForReply(groupChatID, 1)

		for _, sp := range touchedIn(h.mem.recorded()) {
			if sp != l.shared {
				t.Errorf("group turn touched space %s; a group scope may only ever reach %s", sp, l.shared)
			}
		}
		if got := searchedIn(h.mem.recorded()); len(got) != 1 {
			t.Errorf("group turn made %d space searches (%v), want exactly one", len(got), got)
		}
		// Not just the prompt: the whole body of every request that left the
		// process. A private space id anywhere on that wire is a leak.
		for i, raw := range h.proxy.rawAll() {
			if bytes.Contains(raw, []byte(l.private)) {
				t.Errorf("request %d to the endpoint carries the private space id %s", i, l.private)
			}
		}
		for _, o := range h.tr.Sent() {
			if strings.Contains(o.Text, string(l.private)) {
				t.Errorf("an outbound message carries the private space id %s: %q", l.private, o.Text)
			}
		}
	})

	// 3b. The household group answers when it is spoken to and listens the rest of
	// the time.
	//
	// Telegram's bot privacy mode is off in a kenward household — with it on the bot
	// receives nothing and ignores the family in silence — so every sentence the
	// household says to each other arrives here, and internal/transport rebuilds the
	// gate Telegram used to apply (addressedTo). The unit tests own the three wire
	// shapes that set the flag; what only a live run can price is the other half of
	// the claim: that an unaddressed message costs no completion. A gate that merely
	// swallowed the reply would still have spent a model call, its latency and its
	// electricity on every word of a family conversation, and the member would never
	// see the difference.
	//
	// No sleep and no negative wait. The Fake delivers in order and a Unit runs one
	// turn at a time, so the aside is finished with by the time the answer to the
	// question exists — which makes "exactly one request" an assertion rather than a
	// race.
	t.Run("GroupAsideIsHeardAndNotAnswered", func(t *testing.T) {
		const aside = "we said we'd replace the boiler in the spring, not now"
		h := newLiveHarness(t, l, liveOptions{})
		h.start()
		// Built by hand rather than with InjectText, which sets Addressed: that
		// field is the entire subject here.
		h.tr.Inject(transport.Inbound{
			ChatID: groupChatID, UserID: davidTelegramID, Text: aside,
			MessageID: 1, IsGroup: true, At: time.Now().UTC(),
		})
		h.tr.InjectText(groupChatID, davidTelegramID,
			"@kenward_bot what did we decide about the boiler? One short sentence.", true)
		sent := h.waitForReply(groupChatID, 1)

		if n := h.proxy.count(); n != 1 {
			t.Errorf("the endpoint saw %d requests for one aside and one question, want 1: an unaddressed message must not cost a completion", n)
		}
		if len(sent) != 1 {
			t.Errorf("the group chat got %d messages for one question: %q", len(sent), sent)
		}
		// Whether the model also proposed keeping the decision is not this test's
		// business, and it used to be asserted here as `Asked() == 0`. It failed
		// deterministically against a 27B that answered the question and proposed
		// storing the answer, saying so in its own reasoning trace: "I should propose
		// remembering this since it's a durable household decision." That is a settled
		// household decision going to shared memory, which under D-038 is a question
		// and not a write — the product working. A structural test that fails on the
		// model exercising judgement is testing the model, and the rate at which it
		// judges well is scored in internal/assistant's TestCaptureJudgement, which is
		// where a number that moves with the sampler belongs.
		//
		// It stays as a line on the log because a run where the aside alone provoked a
		// question would be a real finding, and the one completion asserted above is
		// already the proof that it did not: a question can only have come off the turn
		// that produced it.
		if n := len(h.tr.Asked()); n != 0 {
			t.Logf("the answered turn also proposed %d capture(s) — judgement, scored in TestCaptureJudgement, not here", n)
		}
		// Heard, though. The aside is the context the question needs, so it has to be
		// in the ring when the question is assembled — and the ring is only
		// interesting once the reply is gone, because a turn that answered the aside
		// would put it in history as a side effect and prove nothing about listening.
		var carried bool
		for _, m := range h.proxy.last(t).Messages {
			if strings.Contains(m.Content, aside) {
				carried = true
			}
		}
		if !carried {
			t.Errorf("the overheard message never reached the prompt; the messages sent were %+v", h.proxy.last(t).Messages)
		}
		t.Logf("group reply: %q", sent[0].Text)
	})

	// 3b-ii. The handle a member types to address the household node is not part of
	// what they asked it.
	//
	// This is the group gate's own bill, and it was paid live. Once an @mention became
	// the ordinary way to speak to kenward in a group, the handle started arriving
	// inside the text, and to lore's tokeniser "@kenward_hearth_e2e_bot" is four words.
	// A turn searches six, first six win, so a real household asked for its garage gate
	// code and the shared store was searched for "kenward hearth e2e bot cuál es" — no
	// retrieval line at all, and "I have not got that written down" over an entry that
	// held it. The same question by reply found it in one.
	//
	// Everything below the transport is real: a real lore store searched by a real
	// client, and the entry seeded by the `lore` binary out of process. The Inbound is
	// built by hand because transport.Fake is not Telegram and does not know the bot's
	// own name — the field this is about is set by internal/transport, from the mention
	// entity, and internal/transport's own tests own that half.
	t.Run("GroupMentionDoesNotEatTheQuestion", func(t *testing.T) {
		const handle = "@kenward_hearth_e2e_bot"
		token := "membrillo-" + stamp()
		loreCLI(t, l, "El código del portón del garaje es "+token+".",
			"put", "-space", string(l.shared), "-domain", "kenward/e2e",
			"-title", "Código del portón del garaje",
			"-confidence", "validated", "-body-file", "-")

		h := newLiveHarness(t, l, liveOptions{persona: config.PersonaConfig{Language: "Spanish"}})
		h.start()
		h.tr.Inject(transport.Inbound{
			ChatID: groupChatID, UserID: davidTelegramID,
			Text:      handle + " ¿cuál es el código del portón del garaje?",
			Mention:   handle,
			MessageID: 1, IsGroup: true, Addressed: true, At: time.Now().UTC(),
		})
		sent := h.waitForReply(groupChatID, 1)
		t.Logf("group reply: %q", sent[0].Text)

		if system := h.proxy.last(t).System(); !strings.Contains(system, token) {
			t.Errorf("an @mentioned question did not retrieve the entry that answers it; the system prompt was:\n%s", system)
		}
		for _, c := range h.mem.recorded() {
			if c.Op == "search" && strings.Contains(handle, strings.ToLower(c.Text)) {
				t.Errorf("the shared store was searched for %q, which is a word of the bot's own handle", c.Text)
			}
		}
	})

	// 3c. A Spanish household reads Spanish and the model still reads English.
	//
	// The two halves are one rule (internal/lang's package comment): everything a
	// member reads is translated, everything the model reads is not, because
	// docs/PROMPT.md is checked against those strings verbatim and translating an
	// instruction changes what is being asked rather than who is being told.
	// TestNodeStringsAreNotInThePrompt asserts it over the strings; this asserts it
	// over the bytes that left the process, with a real persona in a real
	// configuration and a real conversation on top.
	//
	// The member's half is asserted where it is unambiguous — the refusal, in
	// LocalOnlyChainRefuses, which is node text and not the model's. Here the model
	// answers, so the reply is the model's own Spanish and says nothing about the
	// catalogue.
	t.Run("SpanishConversationSendsAnEnglishPrompt", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{persona: config.PersonaConfig{Language: "Spanish"}})
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID, "¿Puedes recordarme algo sobre la casa?", false)
		h.waitForReply(davidChatID, 1)

		// The whole body, not just the system message: the tool descriptions and the
		// schema descriptions ride there too, and they are the half a persona could
		// most plausibly have been allowed to reach.
		raw := h.proxy.rawAll()
		body := raw[len(raw)-1]
		es := lang.For(lang.Spanish)
		// Strings from the member's catalogue, taken from the table rather than
		// written out, so a retranslation moves this with it. Each is a sentence the
		// node says to a person and has no business in a model's instructions.
		for name, s := range map[string]string{
			"ProposalNoDest":      es.ProposalNoDest,
			"EnrolMemoryHeading":  es.EnrolMemoryHeading,
			"TurnFailed":          es.TurnFailed,
			"NoAnswer":            es.NoAnswer,
			"PublishNoShared":     es.PublishNoShared,
			"EnrolPrivateHeading": es.EnrolPrivateHeading,
		} {
			if bytes.Contains(body, []byte(s)) {
				t.Errorf("the Spanish catalogue's %s reached the model: %q", name, s)
			}
		}
		// And the English instructions are still there. These are asserted in the
		// sibling test above as well; both are here because "no Spanish" and "the
		// English is intact" fail differently — a prompt that lost its capture block
		// entirely would satisfy the first.
		system := h.proxy.last(t).System()
		for _, want := range []string{
			"This is a private conversation with David.",
			"propose storing it by calling the remember tool",
			"Language:\n  Spanish",
		} {
			if !strings.Contains(system, want) {
				t.Errorf("the prompt is missing %q:\n%s", want, system)
			}
		}
	})

	// 3d. Cross-language retrieval, across a restart.
	//
	// A household that chose Spanish says something in Spanish; kenward stores it with
	// an English title and body and the member's own words alongside, because lore's
	// index is a conjunctive lexical match with no stemming and no translation and
	// English is the one language every entry is guaranteed to hold. Forty seconds
	// later the member asked for it in Spanish and was told it had never been said.
	// internal/capture/xlang_test.go owns the write half against a real store; this
	// owns the half that only a live run can show — that the words survive into a
	// *later process* and are found by a real search from a real turn.
	//
	// The restart is the whole point of the second harness. A single harness would
	// answer from the history ring, which still holds the sentence the member typed,
	// and prove nothing about the store at all. A new Unit has an empty ring, so the
	// only path from the member's Spanish question to the token is retrieval.
	//
	// The entry is seeded through `lore put` rather than through a turn, and the alias
	// line is built by the same lang.Catalogue.AlsoKnownAs the capture engine calls,
	// so this is the row capture writes and not an approximation of it. Going through
	// the model would make this a test of whether a 27B emits a tool call today; it
	// does not at the time of writing (see DictatedToolCallWritesToLore), and the
	// subject here is retrieval.
	t.Run("CrossLanguageRetrievalSurvivesARestart", func(t *testing.T) {
		token := "4821" + stamp()
		es := lang.For(lang.Spanish)
		body := "The code for the garden gate is " + token + ".\n\n" +
			es.AlsoKnownAs([]string{"código de la cancela del jardín"})
		loreCLI(t, l, body,
			"put", "-space", string(l.private), "-domain", "kenward/e2e",
			"-title", "Garden gate code",
			"-confidence", "validated", "-body-file", "-")

		h := newLiveHarness(t, l, liveOptions{persona: config.PersonaConfig{Language: "Spanish"}})
		h.start()
		// Not one English word in the question, and the token is not in it either, so
		// the only way it can reach the prompt is the alias line.
		h.tr.InjectText(davidChatID, davidTelegramID, "¿Cuál es el código de la cancela del jardín?", false)
		sent := h.waitForReply(davidChatID, 1)
		t.Logf("reply: %q", sent[0].Text)

		system := h.proxy.last(t).System()
		if !strings.Contains(system, token) {
			t.Errorf("a Spanish question did not retrieve the entry the member's own words are on; the system prompt was:\n%s", system)
		}
	})

	// 3e. The model's Markdown reaches the member as Telegram HTML.
	//
	// 71add9f put every message into HTML and the prompt asks for plain prose, which
	// took Markdown from six replies in a live run down to two. Two is not zero, so
	// transport.Markdown renders the residue rather than showing it, and this is that
	// converter on the real path with a real model's output going through it.
	//
	// The assertion is a property and not a wording, because the wording belongs to a
	// model nobody here controls: whatever the model wrote, what the member reads is
	// that text converted, and the recorded completion is the only place the "before"
	// exists. The stronger claim — asterisks became <b> — is made only when the model
	// actually wrote asterisks, and logged either way, because a test that failed
	// because a model declined to use bold would be a test of the endpoint.
	t.Run("ModelMarkdownReachesTheMemberAsHTML", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{})
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID,
			"Reply with exactly this and nothing else: The bins go out on **Thursday**.", false)
		sent := h.waitForReply(davidChatID, 1)

		model := h.proxy.lastReplyText(t)
		t.Logf("the model wrote: %q\nthe member read: %q", model, sent[0].Text)

		// The property. What a member reads is transport.Markdown over the model's
		// text with its surrounding whitespace taken off (assistant.sanitizeReply),
		// and the retrieval line may ride in front of that, so this is a containment
		// of the converted text and not an equality with it. The 27B here opens every
		// reply with two newlines, which is exactly the kind of detail a test written
		// against a fake would never have met.
		if want := transport.Markdown(strings.TrimSpace(model)); !strings.Contains(sent[0].Text, want) {
			t.Errorf("the member did not read the converted reply:\n  read      %q\n  converted %q", sent[0].Text, want)
		}
		// The stronger claim, made only when the model actually wrote asterisks.
		// Failing because a model declined to use bold would be a test of the
		// endpoint; saying out loud that this run did not test it is the honest
		// alternative to a green tick over nothing.
		if !strings.Contains(model, "**") {
			t.Skipf("the model wrote no Markdown this time, so the conversion itself is untested here: %q", model)
		}
		if !strings.Contains(sent[0].Text, "<b>") {
			t.Errorf("the model wrote **bold** and the member did not get <b>: %q", sent[0].Text)
		}
		if strings.Contains(sent[0].Text, "**") {
			t.Errorf("the member read the asterisks the model typed: %q", sent[0].Text)
		}
	})

	// 4. The write path, end to end: a confirmed capture lands in a real store.
	// The model really emits the tool call, the member really presses the button,
	// and a separate `lore` process — not the client under test — is asked
	// whether the entry is there.
	//
	// **The member's message dictates the tool call, so this test says nothing
	// about the model's judgement.** It names the title, body, domain and target
	// outright. No household talks like that; the phrasing exists to make the
	// model emit one specific well-formed call so that everything after the call
	// — extraction, the question, the button, the put, the row — is what is
	// actually being measured. Whether the model decides to capture anything at
	// all, when nobody has told it to, is a different question with a suite of
	// its own: TestCaptureJudgement in internal/assistant, which scores that
	// decision across thirteen conversations and reports a rate.
	//
	// Dictating was once the only option — a 0.5b model asked vaguely emits a
	// tool call with invented argument names, which kenward correctly drops — and
	// it is no longer, since a 27B on vLLM emits well-formed calls unprompted. It
	// stays dictated anyway, and the reason is different now: this scenario waits
	// on a real button and a real store, and it must exercise the write path on
	// the smallest model a household might run. A test that only passes when the
	// model volunteers a capture would be measuring judgement and reporting it as
	// a broken write.
	//
	// The domain is dictated because of a defect this test found — and which has
	// since been fixed, so the dictation is now belt over braces rather than the
	// only thing holding the scenario up. A model may omit `domain` even though
	// the schema declares it required, and real lore rejects a put without one
	// ("title, body and domain are required"), by which point the member has
	// already pressed the button: the capture died as "I can't confirm whether it
	// was saved", every time, with nothing they could do about it. The fake
	// memory.Memory accepts an empty domain, which is why no other test saw it.
	// extractProposal now defaults an absent domain to "household/general", the
	// same way it defaults confidence, so a real capture no longer depends on the
	// member having named one.
	//
	// **The message is no longer the dictation the paragraphs above describe, and the
	// reason is a live finding rather than a preference.** It used to read "Call the
	// remember tool now with title …, body …, domain … and target personal", and
	// against the current prompt a 27B answers that with "Done." and no tool call at
	// all. It is not a sampling accident: greedy, three runs, same answer, and the
	// reasoning trace shows the model working out the arguments and then deciding not
	// to emit the call. Deleting either "and not that you have proposed it either" or
	// "Answer what was said, and leave the memory out of it." from captureText
	// restores it immediately. A message *about the tool* collides with a paragraph
	// telling the model not to talk about the tool, and the model resolves the
	// collision by dropping the tool.
	//
	// So the member now asks the way a member asks, which is what the comment above
	// always said no household would do. Measured against the same endpoint, "Remember
	// this just for me: …", "My … is …. Save that." and "Remember in my private memory
	// that …" each produce one well-formed remember call with target personal, every
	// time. Nothing about what this scenario measures has moved: the assertions below
	// are the same ones, on the same path, and the only thing the message no longer
	// dictates is the name of a function.
	//
	// The token is still dictated into the text so that the entry can be found, and
	// the title is not: the model writes it now, and a title this test invented would
	// be one more thing the model has to be persuaded to copy.
	//
	// **This scenario is flaky against a real model, and the flake is the product.**
	// Measured over five runs of exactly this message at temperature zero, four
	// produced a well-formed remember call and one produced no call and this reply:
	// "Got it — your boiler service code is marlowbrick609852900, and I've noted that
	// the engineer wrote it with stars around it." Nothing was written. Zero
	// temperature does not make it deterministic because the token differs each run,
	// which is enough to move the decision.
	//
	// That is not a reason to soften the assertion, because it is not an assertion
	// about wording: either lore has the row or it does not, and a member who asked
	// for something to be remembered and was told "I've noted that" over an empty
	// store has been lied to about their own memory. TestCaptureJudgement scores the
	// same failure across thirty-nine samples; this one shows it on the whole path,
	// with the store underneath, and prints the reply that did the lying.
	t.Run("MemberAskedForAWriteAndLoreGotOne", func(t *testing.T) {
		token := "marlowbrick" + stamp()

		h := newLiveHarness(t, l, liveOptions{})
		h.tr.AnswerWithChoice(capture.ChoicePersonal)
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID,
			fmt.Sprintf("Remember this just for me: my boiler service code is *%s* — the engineer wrote it with stars around it.", token), false)

		// The wait is on the completed write, not on the recorded call: the call is
		// recorded on the way in, so waiting for it races the store. It is bounded
		// well short of liveTimeout and it does not fatal, because "the model did not
		// call the tool" is the interesting outcome here rather than a stuck harness,
		// and a member who asked for something to be remembered will not wait two
		// minutes to find out either.
		if !waitUpTo(45*time.Second, func() bool { return len(h.mem.writes()) > 0 }) {
			t.Errorf("the member asked for something to be remembered and nothing reached lore: %d remember calls, %d questions, %d messages.",
				len(h.mem.recorded()), len(h.tr.Asked()), len(h.sentTo(davidChatID)))
			// The reply, verbatim, because the failure that matters is not the
			// missing write — it is what the member was told instead. A live 27B
			// answers this turn with "Got it — …, and I've noted that …" and writes
			// nothing, which is the sentence docs/PROMPT.md's capture block exists to
			// prevent, arriving on the one path where the member actually asked.
			for _, o := range h.sentTo(davidChatID) {
				t.Errorf("  the member read: %q", o.Text)
			}
			return
		}

		for _, c := range h.mem.recorded() {
			if c.Op == "put" && (len(c.Spaces) != 1 || c.Spaces[0] != l.private) {
				t.Errorf("put went to %v, want the member's private space %s", c.Spaces, l.private)
			}
		}
		writes := h.mem.writes()
		if len(writes) != 1 {
			t.Fatalf("%d writes reached lore, want exactly 1", len(writes))
		}
		w := writes[0]
		if w.err != nil {
			t.Fatalf("the confirmed write failed: %v", w.err)
		}
		if w.entry.Space != l.private {
			t.Errorf("lore reports the entry landed in %s, want %s", w.entry.Space, l.private)
		}
		if w.entry.ID == "" {
			t.Fatal("lore returned no entry id for the confirmed write")
		}

		// The independent witness: a fresh `lore` process, a fresh connection to
		// the same store, asked for that id. The lookup is by id rather than by
		// search because search is a full-text match and would be answering a
		// different question — whether the words happen to be indexed — instead
		// of whether the row is there.
		out := loreCLI(t, l, "", "get", w.entry.ID)
		if !strings.Contains(out, w.draft.Body) {
			t.Errorf("a fresh `lore get %s` does not return the body kenward wrote (%q); got:\n%s", w.entry.ID, w.draft.Body, out)
		}
		// And in the right space. lore prints the space's display name, so the id
		// under test is resolved to its name from a second independent call.
		if name := spaceName(t, l, l.private); name != "" && !strings.Contains(out, name) {
			t.Errorf("`lore get %s` does not report space %s (%q); got:\n%s", w.entry.ID, l.private, name, out)
		}
		t.Logf("independent `lore get %s`:\n%s", w.entry.ID, strings.TrimSpace(out))

		// The token has to be in the row, or the entry is about something else and
		// every assertion above it was about the wrong write.
		if !strings.Contains(w.draft.Body+w.draft.Title, token) {
			t.Errorf("the entry does not carry the member's own token %q: %+v", token, w.draft)
		}

		// What the member was shown is what was written, byte for byte, escaped and
		// not parsed.
		//
		// This is the boundary transport.Markdown must never cross. It is applied to
		// the model's reply and to nothing else; an entry body reaches a member
		// through transport.Quote, which escapes and has no parser in it, so a body
		// with a * in it is a body with a * in it. Asserting the whole quoted body
		// rather than hunting for asterisks makes the assertion true of whatever the
		// model happened to write — the body is model-written text carrying the
		// member's words, and a test that needed it to contain a particular character
		// would be a test of the endpoint.
		shown, ok := h.tr.LastAsked()
		if !ok {
			t.Fatal("nothing was put to the member, so there is no announcement to check")
		}
		if !strings.Contains(shown.Text, transport.Quote(w.draft.Body)) {
			t.Errorf("the member was not shown the body that was written:\n  written %q\n  shown   %s", w.draft.Body, shown.Text)
		}
		// The member's message puts asterisks in the sentence the model is about to
		// write back, so the containment above is normally an assertion about them
		// specifically: a quoted body still holding its * is a body no converter
		// touched. It is not guaranteed — the model may paraphrase them away — so
		// whether this run got to test it is said out loud rather than assumed.
		if !strings.Contains(w.draft.Body, "*") {
			t.Logf("this run's body carries no asterisk (%q), so the containment above proved escaping and not the Markdown boundary", w.draft.Body)
		}
	})

	// 5. A local-only chain whose one machine is asleep refuses, in the member's
	// chat, naming the tier and the endpoint. Nothing widens.
	//
	// The expected sentence is assembled here from the same catalogue the node
	// assembles it from, and not written out. It used to be three literals with
	// MarkdownV2 backticks in them — "`local`", "`attic`" — and 71add9f moved every
	// message kenward sends into Telegram HTML, where the tier is transport.Code's
	// <code>local</code>. The suite is behind a build tag, so nothing said so: the
	// assertion had been failing against every live endpoint since that commit while
	// the default run stayed green. A literal cannot survive the next parse-mode
	// change either; a call to the function that produced the string fails only when
	// the sentence itself changes, which is the thing worth being told about.
	//
	// Both languages, because the refusal is translated now (internal/lang). Asserting
	// only the English one would leave the Spanish path — the one where a member most
	// needs the sentence to arrive — covered by nothing here at all.
	t.Run("LocalOnlyChainRefuses", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			persona config.PersonaConfig
			tag     string
		}{
			{"English", config.PersonaConfig{}, lang.English},
			{"Spanish", config.PersonaConfig{Language: "Spanish"}, lang.Spanish},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newLiveHarness(t, l, liveOptions{endpointDown: true, persona: tc.persona})
				h.start()
				h.tr.InjectText(davidChatID, davidTelegramID, "is the heating on?", false)
				sent := h.waitForReply(davidChatID, 1)

				// The chain is the one tier this household configures and the machine
				// is the one endpoint in it; everything else comes out of the table.
				cat := lang.For(tc.tag)
				want := transport.GlyphProblem + " " + cat.RefusalAssembled(
					cat.WhoseDirect,
					cat.Chain([]string{"local"}),
					cat.Tried([]string{"attic"}),
					cat.TierWord(1),
				)
				if sent[0].Text != want {
					t.Errorf("the refusal was\n  %q\nwant\n  %q", sent[0].Text, want)
				}
				// One thing the equality above cannot catch, because both sides of it
				// come from the catalogue: that the tier and the machine reach the
				// member as identifiers rather than as bare words. A table that went
				// back to backticks would move the expectation along with the message
				// and pass — which is how the original assertion came to be checking a
				// parse mode kenward had stopped using.
				for _, name := range []string{"local", "attic"} {
					if !strings.Contains(sent[0].Text, transport.Code(name)) {
						t.Errorf("%q does not reach the member as an identifier; the refusal was %q", name, sent[0].Text)
					}
				}
				if n := h.proxy.count(); n != 0 {
					t.Errorf("the real endpoint saw %d requests; a chain with no reachable machine must reach none", n)
				}
			})
		}
	})
}
