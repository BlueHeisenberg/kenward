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
//	KENWARD_E2E_ENDPOINT=http://localhost:11434/v1 \
//	KENWARD_E2E_MODEL=qwen2.5:0.5b \
//	go test -tags integration -run TestLive -v ./internal/e2e/
//
// The store is not configured, because the suite creates its own and throws it
// away again; see newLoreStore. Only the model endpoint is external.

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
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// liveTimeout bounds every poll here. A real 0.5b model on a real machine and a
// real lore subprocess are both slower than loopback httptest, and a tool-call
// turn is two model round trips plus a button.
const liveTimeout = 2 * time.Minute

// live gathers what this suite runs against: the endpoint and model come from the
// environment, the store from newLoreStore. A missing endpoint or model skips, so
// `go test ./...` and CI never reach a model.
type live struct {
	loreBin  string
	loreHome string
	private  domain.SpaceID
	shared   domain.SpaceID
	endpoint string
	model    string
}

func liveEnv(t *testing.T) live {
	t.Helper()
	l := live{
		loreBin:  os.Getenv("KENWARD_LORE_BIN"),
		endpoint: os.Getenv("KENWARD_E2E_ENDPOINT"),
		model:    os.Getenv("KENWARD_E2E_MODEL"),
	}
	if l.loreBin == "" {
		l.loreBin = "lore"
	}
	if l.endpoint == "" || l.model == "" {
		t.Skip("set KENWARD_E2E_ENDPOINT and KENWARD_E2E_MODEL; the lore store is created and destroyed by the test and needs no configuration")
	}
	return l
}

// newLoreStore gives this run a lore store it owns outright: a fresh LORE_HOME
// under t.TempDir(), holding two spaces created here and nothing else. The
// temporary directory takes it away again, so no state survives a run and no run
// can see another's.
//
// This replaces pointing the suite at spaces kept for the purpose in a persistent
// store, which does not work, because lore has no delete. Not in the CLI, not over
// MCP, and not for spaces at all: `internal/store.DeleteEntry` writes a proper
// propagating tombstone but nothing exposes it, and no code path anywhere removes
// a space. So every run added entries no later run could remove, and after eight
// runs the store held eight near-identical greenhouse entries that retrieval could
// not tell apart — the suite had made itself fail. A test whose store degrades
// every time it runs is measuring its own history.
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
// of every request body. It is not a fake provider: it adds nothing and answers
// nothing, so the completion the pool reads is the model's own.
type recordingProxy struct {
	srv    *httptest.Server
	target string

	mu       sync.Mutex
	requests []wireRequest
	raw      [][]byte
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
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	client, err := memory.NewClient(memory.Config{
		Command:  l.loreBin,
		LoreHome: l.loreHome,
	})
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
	deadline := time.Now().Add(liveTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
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
	fmt.Fprintf(&b, "telegram:\n  bot_token_env: %s\n", botTokenEnv)
	fmt.Fprintf(&b, "members:\n  - id: david\n    name: David\n")
	fmt.Fprintf(&b, "    telegram_id: %d\n", davidTelegramID)
	fmt.Fprintf(&b, "    private_space: %s\n    tiers: [%s]\n", l.private, tiers)
	fmt.Fprintf(&b, "endpoints:\n  - name: attic\n    base_url: '%s'\n    model: '%s'\n    tags: [local]\n    timeout: 120s\n", url, l.model)
	fmt.Fprintf(&b, "memory:\n  lore_command: [%s, mcp]\n  search_limit: 8\n", l.loreBin)
	fmt.Fprintf(&b, "session:\n  idle_timeout: 30m\n")
	fmt.Fprintf(&b, "capture:\n  max_proposals_per_turn: 1\n")
	fmt.Fprintf(&b, "update:\n  channel: stable\n  check_interval: 6h\n")
	return b.String()
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

	// 4. The assertion nothing has ever made: a confirmed capture lands in a real
	// store. The model really proposes, the member really presses the button,
	// and a separate `lore` process — not the client under test — is asked
	// whether the entry is there.
	//
	// The member's message dictates the title, body, domain and target outright.
	// That is unusual phrasing for a household, and it is deliberate in two
	// different ways.
	//
	// Title, body and target are dictated because a 0.5b model asked vaguely
	// emits a tool call with invented argument names, which kenward correctly
	// drops. What is under test here is the path from a well-formed tool call to
	// a row in lore, not the model's judgement about when to make one.
	//
	// The domain is dictated because of a defect this test found and does not
	// paper over: the remember schema does not require `domain`, extractProposal
	// defaults `confidence` when the model omits it but leaves `domain` empty,
	// and real lore rejects a put with no domain — "title, body and domain are
	// required". The member has already pressed the button by then, so the whole
	// capture dies as "I can't confirm whether it was saved", every time, with
	// nothing they can do about it. The fake memory.Memory accepts an empty
	// domain, which is why nothing saw this. Dictating the domain keeps this
	// scenario about the write landing; the defect belongs to internal/assistant.
	t.Run("ConfirmedCaptureWritesToLore", func(t *testing.T) {
		token := "marlowbrick" + stamp()
		title := "Boiler service code " + token
		body := "The boiler service code is " + token + "."

		h := newLiveHarness(t, l, liveOptions{})
		h.tr.AnswerWithChoice(capture.ChoicePersonal)
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID,
			fmt.Sprintf("Call the remember tool now with title %q, body %q, domain \"household/logistics\" and target personal.", title, body), false)

		waitForLive(t, "a capture question", func() bool { return len(h.tr.Asked()) > 0 })
		// The wait is on the completed write, not on the recorded call: the call
		// is recorded on the way in, so waiting for it races the store.
		waitForLive(t, "a write completing against lore", func() bool { return len(h.mem.writes()) > 0 })

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
	})

	// 5. A local-only chain whose one machine is asleep refuses, in the member's
	// chat, naming the tier and the endpoint. Nothing widens.
	t.Run("LocalOnlyChainRefuses", func(t *testing.T) {
		h := newLiveHarness(t, l, liveOptions{endpointDown: true})
		h.start()
		h.tr.InjectText(davidChatID, davidTelegramID, "is the heating on?", false)
		sent := h.waitForReply(davidChatID, 1)

		for _, want := range []string{"`local`", "`attic`", "won't send it anywhere else"} {
			if !strings.Contains(sent[0].Text, want) {
				t.Errorf("refusal %q does not contain %q", sent[0].Text, want)
			}
		}
		if n := h.proxy.count(); n != 0 {
			t.Errorf("the real endpoint saw %d requests; a chain with no reachable machine must reach none", n)
		}
	})
}
