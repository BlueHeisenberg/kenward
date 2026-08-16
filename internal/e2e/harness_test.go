package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/vault"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/transport"
	"github.com/BlueHeisenberg/kenward/internal/transport/telegramtest"
)

// The household these tests serve. The ids are arbitrary but fixed, so an
// assertion can name the account it means.
const (
	davidTelegramID int64 = 1001
	davidChatID     int64 = 5001
	meiTelegramID   int64 = 1002
	meiChatID       int64 = 5002
	strangerUserID  int64 = 9999
	strangerChatID  int64 = 7777
	groupChatID     int64 = -1001234567890
)

// The household's spaces. davidSpace and meiSpace are private to one member
// each; sharedSpace is the household's.
const (
	davidSpace  = domain.SpaceID("david-private")
	meiSpace    = domain.SpaceID("mei-private")
	sharedSpace = domain.SpaceID("household")
)

const (
	botTokenEnv = "KENWARD_BOT_TOKEN"
	cloudKeyEnv = "KENWARD_CLOUD_KEY"
	passphrase  = "correct horse battery staple"
	// testBotToken is the household's bot token throughout this package. It is
	// not a real one and never reaches Telegram; telegramtest.Server refuses any
	// other, so a harness driving the real transport proves the token was carried
	// rather than assumed.
	testBotToken = "123456:telegram-token"
)

// waitTimeout bounds every poll in this package. Turns here are loopback HTTP
// round trips, so anything approaching this is a hang, not slowness.
const waitTimeout = 15 * time.Second

// -----------------------------------------------------------------------------
// lore, faked at the memory.Memory seam
// -----------------------------------------------------------------------------

// memCall is one call made to memory, recorded so a test can assert on which
// spaces were reached rather than only on what came back. The difference
// matters: a group conversation that searched a private space and happened to
// find nothing has still broken the invariant.
type memCall struct {
	Op     string // search | get | put | share
	Spaces []domain.SpaceID
	Text   string
	Title  string
}

// fakeMemory is a recording memory.Memory. It answers searches from seeded
// entries and never re-ranks across spaces, matching what the real lore client
// promises.
type fakeMemory struct {
	mu     sync.Mutex
	calls  []memCall
	seeded map[domain.SpaceID][]memory.Entry
	puts   int
	// deleteErr makes an undo fail the way a real one can. Set it and the entry
	// stays written, which is the case the member must be told about honestly.
	deleteErr error
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{seeded: make(map[domain.SpaceID][]memory.Entry)}
}

// seed installs the entries a search of space will return.
func (m *fakeMemory) seed(space domain.SpaceID, entries ...memory.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range entries {
		entries[i].Space = space
	}
	m.seeded[space] = entries
}

func (m *fakeMemory) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(q.Spaces) == 0 {
		// The real client refuses rather than defaulting to "everything"; a fake
		// that quietly widened would hide the very bug the rule exists for.
		return nil, memory.ErrEmptySpaceSet
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{Op: "search", Spaces: append([]domain.SpaceID(nil), q.Spaces...), Text: q.Text})
	var out []memory.Entry
	for _, sp := range q.Spaces {
		for _, e := range m.seeded[sp] {
			// Matched, not returned wholesale. lore's search is conjunctive over
			// bare words — every term must be present, with no stemming and no
			// operators — and a fake that answered every query with everything it
			// held was how a dead retrieval path passed a full test suite.
			if loreMatch(e, q.Text) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// loreMatch reports whether an entry would come back from lore for this query.
// Whole words, because lore tokenises "quillfeather921834100" as one word and so
// does not find it by "quillfeather".
func loreMatch(e memory.Entry, query string) bool {
	words := make(map[string]bool)
	for _, w := range memory.Terms(e.Title + " " + e.Body) {
		words[w] = true
	}
	for _, t := range memory.Terms(query) {
		if !words[t] {
			return false
		}
	}
	return true
}

func (m *fakeMemory) Get(ctx context.Context, space domain.SpaceID, id string) (memory.Entry, error) {
	if err := ctx.Err(); err != nil {
		return memory.Entry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{Op: "get", Spaces: []domain.SpaceID{space}, Title: id})
	for _, e := range m.seeded[space] {
		if e.ID == id {
			return e, nil
		}
	}
	return memory.Entry{}, memory.ErrNotFound
}

func (m *fakeMemory) Put(ctx context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	if err := ctx.Err(); err != nil {
		return memory.Entry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{Op: "put", Spaces: []domain.SpaceID{space}, Title: d.Title})
	m.puts++
	return memory.Entry{
		ID:         fmt.Sprintf("entry-%d", m.puts),
		Space:      space,
		Domain:     d.Domain,
		Title:      d.Title,
		Body:       d.Body,
		Confidence: d.Confidence,
		Markers:    d.Markers,
	}, nil
}

func (m *fakeMemory) Share(ctx context.Context, from, to domain.SpaceID, entryID string) (memory.Entry, error) {
	if err := ctx.Err(); err != nil {
		return memory.Entry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{Op: "share", Spaces: []domain.SpaceID{from, to}, Title: entryID})
	return memory.Entry{ID: entryID, Space: to}, nil
}

func (m *fakeMemory) Delete(ctx context.Context, space domain.SpaceID, entryID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{Op: "delete", Spaces: []domain.SpaceID{space}, Title: entryID})
	return m.deleteErr
}

func (m *fakeMemory) Close() error { return nil }

var _ memory.Memory = (*fakeMemory)(nil)

// replyBody strips the retrieval line the assistant prefixes to a reply, so that a
// test about the turn reaching the model and coming back is not also a test of what
// the node reported about its own reading. TestRetrievalLineReachesTheMemberEndToEnd
// is the assertion about the line.
//
// Nothing here turns the line off to make its assertions simpler. These tests exist to
// exercise the product as it ships, and a suite that quietly configured the shipped
// behaviour away would stop proving the thing it is for.
func replyBody(text string) string {
	if !strings.HasPrefix(text, "[searched ") {
		return text
	}
	if _, rest, ok := strings.Cut(text, "]\n\n"); ok {
		return rest
	}
	return text
}

// recorded returns a copy of every call made so far.
func (m *fakeMemory) recorded() []memCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// searchedSpaces lists the distinct spaces a search named, first use first.
// Distinct because one turn issues a search per query term per space, and the
// invariant asserted here is always which spaces a conversation could reach.
func (m *fakeMemory) searchedSpaces() []domain.SpaceID {
	var out []domain.SpaceID
	for _, c := range m.recorded() {
		if c.Op == "search" {
			out = append(out, c.Spaces...)
		}
	}
	return distinctSpaces(out)
}

// distinctSpaces removes repeats while preserving first-use order.
func distinctSpaces(in []domain.SpaceID) []domain.SpaceID {
	seen := make(map[domain.SpaceID]bool, len(in))
	var out []domain.SpaceID
	for _, sp := range in {
		if !seen[sp] {
			seen[sp] = true
			out = append(out, sp)
		}
	}
	return out
}

// touchedSpaces lists every space named by any call of any kind. It is the
// assertion surface for "this conversation never reached that space at all".
func (m *fakeMemory) touchedSpaces() []domain.SpaceID {
	var out []domain.SpaceID
	for _, c := range m.recorded() {
		out = append(out, c.Spaces...)
	}
	return out
}

// putCalls returns only the writes.
func (m *fakeMemory) putCalls() []memCall {
	var out []memCall
	for _, c := range m.recorded() {
		if c.Op == "put" {
			out = append(out, c)
		}
	}
	return out
}

// deleteCalls returns only the deletes. An undo is the one place this household
// removes anything, so a test asserting on these is asserting on undo and nothing
// else.
func (m *fakeMemory) deleteCalls() []memCall {
	var out []memCall
	for _, c := range m.recorded() {
		if c.Op == "delete" {
			out = append(out, c)
		}
	}
	return out
}

func containsMember(ids []domain.MemberID, want domain.MemberID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func containsSpace(spaces []domain.SpaceID, want domain.SpaceID) bool {
	for _, s := range spaces {
		if s == want {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// the provider, faked at the wire
// -----------------------------------------------------------------------------

// wireMessage is one chat message as it arrives on the OpenAI-compatible wire.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// wireRequest is the part of an inbound /chat/completions body these tests read.
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// System returns the system prompt the unit assembled, which is where retrieval
// and scope disclosure land.
func (r wireRequest) System() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// UserText returns the last user message, which is the member's own words.
func (r wireRequest) UserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// providerToolCall is a tool call the fake model decides to make.
type providerToolCall struct {
	Name      string
	Arguments string
}

// providerReply is what the fake endpoint answers with.
type providerReply struct {
	Text         string
	FinishReason string
	ToolCalls    []providerToolCall
}

// fakeProvider is an OpenAI-compatible endpoint. It is an httptest server rather
// than a routing.Completer so that the pool's connect probe, keel/llm's request
// encoding and its tool-call decoding are all exercised for real; the only thing
// missing relative to production is a model.
type fakeProvider struct {
	name     string
	srv      *httptest.Server
	received chan struct{}

	mu       sync.Mutex
	requests []wireRequest
	reply    func(wireRequest) providerReply
	delay    time.Duration
}

func newFakeProvider(t *testing.T, name string) *fakeProvider {
	t.Helper()
	p := &fakeProvider{
		name:     name,
		received: make(chan struct{}, 32),
		reply: func(wireRequest) providerReply {
			return providerReply{Text: "Noted.", FinishReason: "stop"}
		},
	}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

// baseURL is what goes into the configuration's endpoints[].base_url.
func (p *fakeProvider) baseURL() string { return p.srv.URL + "/v1" }

func (p *fakeProvider) setReply(fn func(wireRequest) providerReply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reply = fn
}

func (p *fakeProvider) setDelay(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delay = d
}

func (p *fakeProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *fakeProvider) all() []wireRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]wireRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

// last returns the most recent request, failing the test if there was none.
func (p *fakeProvider) last(t *testing.T) wireRequest {
	t.Helper()
	all := p.all()
	if len(all) == 0 {
		t.Fatalf("provider %s received no requests", p.name)
	}
	return all[len(all)-1]
}

func (p *fakeProvider) handle(w http.ResponseWriter, r *http.Request) {
	var req wireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	p.requests = append(p.requests, req)
	reply := p.reply
	delay := p.delay
	p.mu.Unlock()

	select {
	case p.received <- struct{}{}:
	default:
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	rep := reply(req)
	if rep.FinishReason == "" {
		rep.FinishReason = "stop"
	}

	type wireFunction struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type wireCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function wireFunction `json:"function"`
	}
	calls := make([]wireCall, 0, len(rep.ToolCalls))
	for i, tc := range rep.ToolCalls {
		calls = append(calls, wireCall{
			ID:       fmt.Sprintf("call_%d", i),
			Type:     "function",
			Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments},
		})
	}

	body := map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"content":    rep.Text,
				"tool_calls": calls,
			},
			"finish_reason": rep.FinishReason,
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// deadEndpointURL returns a base URL on loopback that nothing is listening on.
// A server is started and immediately closed so the port is real, unused and
// refuses connections promptly — which is what a household machine that is
// powered off looks like to the pool's connect probe.
func deadEndpointURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url + "/v1"
}

// -----------------------------------------------------------------------------
// the harness
// -----------------------------------------------------------------------------

// harnessOptions describes the household one test wants.
type harnessOptions struct {
	// memberTiers is every member's private tier chain. Defaults to [local].
	memberTiers []string
	// householdTiers is the group conversation's chain. Defaults to [local].
	householdTiers []string
	// localDown points the local endpoint at a closed port instead of a running
	// one, modelling the machine being asleep.
	localDown bool
	// withCloud adds a second, reachable endpoint tagged cloud. It exists so a
	// test can prove the node did not reach for it.
	withCloud bool
	// unenrolled names members whose telegram_id is left out of the configuration
	// entirely, which is what a member who has been handed a claim code but has
	// not presented it looks like on disk.
	unenrolled []domain.MemberID
	// enrolFor, when set, supplies the Claimer the supervisor hands messages from
	// senders no unit serves. It is a callback rather than a value because a
	// Claimer is built over a Binder over the configuration, and the configuration
	// does not exist until newHarness has loaded it.
	enrolFor func(*testing.T, *config.Config) *enrol.Claimer
	// dataDir reuses an existing data directory instead of a fresh t.TempDir, so a
	// second household can be built over the state a first one wrote. That is the
	// only way to tell state that was persisted from state that only ever lived in
	// the first supervisor's memory.
	dataDir string
	// telegramAPI, when set, runs the household over the real transport.Telegram
	// pointed at this local Bot API server instead of transport.Fake. h.tr is nil
	// then: a test that drives the real transport reads what the server received,
	// not what a fake recorded.
	telegramAPI *telegramtest.Server
}

// harness is one running household: real configuration, real supervisor, real
// units, faked edges.
type harness struct {
	t *testing.T
	// dir is this household's data directory, which a second harness can be
	// pointed at to prove something outlived the first supervisor.
	dir string
	cfg *config.Config
	// tr is the scripted transport, and is nil when opts.telegramAPI put the real
	// one in its place.
	tr *transport.Fake
	// tg is the real Telegram transport, and is nil unless opts.telegramAPI was set.
	tg       *transport.Telegram
	mem      *fakeMemory
	local    *fakeProvider
	cloud    *fakeProvider
	sessions *session.Manager
	// sup is the supervisor under test. It is the interface rather than *Simple so
	// that a single-unit household — what runs inside an isolated pod — can be
	// driven through exactly the same helpers.
	sup supervisor.Supervisor

	startErr chan error
	stopOnce sync.Once
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	if len(opts.memberTiers) == 0 {
		opts.memberTiers = []string{"local"}
	}
	if len(opts.householdTiers) == 0 {
		opts.householdTiers = []string{"local"}
	}

	dir := opts.dataDir
	if dir == "" {
		dir = t.TempDir()
	}
	local := newFakeProvider(t, "attic")
	localURL := local.baseURL()
	if opts.localDown {
		localURL = deadEndpointURL(t)
	}

	var cloud *fakeProvider
	if opts.withCloud {
		cloud = newFakeProvider(t, "openrouter")
	}

	cfgPath := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML(dir, localURL, cloud, opts)), 0o600); err != nil {
		t.Fatalf("writing configuration: %v", err)
	}

	lookupEnv := fakeEnv(map[string]string{
		botTokenEnv: testBotToken,
		cloudKeyEnv: "sk-not-a-real-key",
	})

	cfg, err := config.LoadWithEnv(cfgPath, lookupEnv)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}

	// A real session.Manager over a real store, with the key-derivation cost
	// turned down so the test is not dominated by argon2id. The unlock path is
	// the production one; only the cost parameter differs.
	sessions, err := session.NewManager(session.ModeSimple, session.NewMemStore(),
		session.WithKDFParams(vault.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}))
	if err != nil {
		t.Fatalf("building session manager: %v", err)
	}
	t.Cleanup(sessions.Close)

	// Keys arrive the way `kenward run` delivers them, and only the way it does.
	// Startup covers the members this household has already enrolled; anybody who
	// claims later gets theirs on the claim path. Provisioning everyone up front —
	// which this harness used to do, unenrolled members included — hid the defect
	// where a mid-run claim produced a unit with no key, so the newest member's
	// first private message was answered with the locked notice.
	ctx := context.Background()
	unlockMember := func(ctx context.Context, m domain.Member) error {
		if err := sessions.Provision(ctx, m.ID, passphrase); err != nil &&
			!errors.Is(err, session.ErrDuplicateMember) {
			return err
		}
		return sessions.Unlock(ctx, m.ID, passphrase)
	}
	for _, m := range cfg.DomainMembers() {
		if !m.Enrolled() {
			continue
		}
		if err := unlockMember(ctx, m); err != nil {
			t.Fatalf("unlocking %s: %v", m.ID, err)
		}
	}

	// The household's one bot. Either the scripted stand-in, or — when a test
	// supplies a local Bot API server — the production transport, unmodified,
	// with only its API root redirected. Nothing below this line can tell which.
	var (
		tr   transport.Transport
		fake *transport.Fake
		tg   *transport.Telegram
	)
	if opts.telegramAPI != nil {
		var err error
		tg, err = transport.NewTelegram(testBotToken,
			transport.WithAPIServer(opts.telegramAPI.URL()),
			// Shorter than the production minute so a poll in flight at shutdown is
			// not the slowest thing in the test. What is under test here — offsets,
			// keyboards, encoding — does not depend on how long one call may hang.
			transport.WithPollTimeout(2*time.Second),
		)
		if err != nil {
			t.Fatalf("building the telegram transport: %v", err)
		}
		tr = tg
	} else {
		fake = transport.NewFake()
		tr = fake
	}
	mem := newFakeMemory()

	var claimer *enrol.Claimer
	if opts.enrolFor != nil {
		claimer = opts.enrolFor(t, cfg)
	}

	// Router is deliberately left nil: the supervisor then builds the real
	// routing.Pool over the real HTTP completer, which is the wiring production
	// uses and the thing this suite exists to exercise.
	sup, err := supervisor.NewSimple(cfg, supervisor.SimpleOptions{
		Transport:     tr,
		Memory:        mem,
		Sessions:      sessions,
		Enrol:         claimer,
		UnlockOnEnrol: unlockMember,
		LookupEnv:     lookupEnv,
	})
	if err != nil {
		t.Fatalf("building supervisor: %v", err)
	}

	h := &harness{
		t:        t,
		dir:      dir,
		cfg:      cfg,
		tr:       fake,
		tg:       tg,
		mem:      mem,
		local:    local,
		cloud:    cloud,
		sessions: sessions,
		sup:      sup,
		startErr: make(chan error, 1),
	}
	t.Cleanup(func() {
		h.stop()
		_ = tr.Close()
	})
	return h
}

// start launches the supervisor. Start blocks for the household's lifetime, so
// it runs on its own goroutine and its error is collected by stop.
func (h *harness) start() {
	h.t.Helper()
	go func() { h.startErr <- h.sup.Start(context.Background()) }()
}

// stop drains and shuts the household down, returning Stop's error. It is
// idempotent so a test may call it explicitly and the cleanup may call it again.
func (h *harness) stop() error {
	var err error
	h.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		err = h.sup.Stop(ctx)
		select {
		case <-h.startErr:
		case <-time.After(waitTimeout):
			h.t.Error("supervisor Start did not return after Stop")
		}
	})
	return err
}

// sentTo returns every outbound message addressed to one chat.
func (h *harness) sentTo(chatID int64) []transport.Outbound {
	var out []transport.Outbound
	for _, o := range h.tr.Sent() {
		if o.ChatID == chatID {
			out = append(out, o)
		}
	}
	return out
}

// waitForReply blocks until n messages have been sent to chatID.
func (h *harness) waitForReply(chatID int64, n int) []transport.Outbound {
	h.t.Helper()
	waitFor(h.t, fmt.Sprintf("%d message(s) to chat %d", n, chatID), func() bool {
		return len(h.sentTo(chatID)) >= n
	})
	return h.sentTo(chatID)
}

// waitFor polls cond until it holds or waitTimeout elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeEnv is a config.LookupEnvFunc over a fixed map, so no test mutates the
// process environment other tests share.
func fakeEnv(vars map[string]string) config.LookupEnvFunc {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// configYAML renders a real kenward.yaml for this household. It is written as
// text rather than built as a struct on purpose: the parser, the unknown-field
// rule and the validator are part of what is under test.
func configYAML(dataDir, localURL string, cloud *fakeProvider, opts harnessOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode: simple\n")
	fmt.Fprintf(&b, "data_dir: '%s'\n", dataDir)
	fmt.Fprintf(&b, "household:\n")
	fmt.Fprintf(&b, "  name: Ashfield\n")
	fmt.Fprintf(&b, "  shared_space: %s\n", sharedSpace)
	fmt.Fprintf(&b, "  group_chat_id: %d\n", groupChatID)
	fmt.Fprintf(&b, "  tiers: [%s]\n", strings.Join(opts.householdTiers, ", "))
	fmt.Fprintf(&b, "telegram:\n")
	fmt.Fprintf(&b, "  bot_token_env: %s\n", botTokenEnv)
	fmt.Fprintf(&b, "members:\n")
	for _, m := range []struct {
		id         domain.MemberID
		name       string
		telegramID int64
		space      domain.SpaceID
	}{
		{"david", "David", davidTelegramID, davidSpace},
		{"mei", "Mei", meiTelegramID, meiSpace},
	} {
		fmt.Fprintf(&b, "  - id: %s\n    name: %s\n", m.id, m.name)
		// A member named in opts.unenrolled has no telegram_id at all. Writing a
		// zero would not do: zero is how the file says "not claimed", so a member
		// enrolled through state.json must be indistinguishable from one who never
		// appeared in the YAML with an id.
		if !containsMember(opts.unenrolled, m.id) {
			fmt.Fprintf(&b, "    telegram_id: %d\n", m.telegramID)
		}
		fmt.Fprintf(&b, "    private_space: %s\n    tiers: [%s]\n",
			m.space, strings.Join(opts.memberTiers, ", "))
	}
	fmt.Fprintf(&b, "endpoints:\n")
	fmt.Fprintf(&b, "  - name: attic\n    base_url: '%s'\n    model: test-model\n    tags: [local]\n    timeout: 30s\n", localURL)
	if cloud != nil {
		fmt.Fprintf(&b, "  - name: openrouter\n    base_url: '%s'\n    model: cloud-model\n    api_key_env: %s\n    tags: [cloud]\n    timeout: 30s\n",
			cloud.baseURL(), cloudKeyEnv)
	}
	fmt.Fprintf(&b, "memory:\n  lore_command: [lore, mcp]\n  search_limit: 8\n")
	fmt.Fprintf(&b, "session:\n  idle_timeout: 30m\n")
	fmt.Fprintf(&b, "capture:\n  max_proposals_per_turn: 1\n")
	fmt.Fprintf(&b, "update:\n  channel: stable\n  check_interval: 6h\n")
	return b.String()
}

// entry builds a seeded memory entry.
func entry(id, title, body string, markers ...string) memory.Entry {
	return memory.Entry{
		ID:         id,
		Title:      title,
		Body:       body,
		Confidence: "validated",
		Markers:    markers,
	}
}

// rememberArgs renders a remember tool call's arguments.
func rememberArgs(title, body, target string) string {
	args, err := json.Marshal(map[string]any{
		"title":      title,
		"body":       body,
		"domain":     "household/logistics",
		"confidence": "provisional",
		"target":     target,
	})
	if err != nil {
		panic(err)
	}
	return string(args)
}
