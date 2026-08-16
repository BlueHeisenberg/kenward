package assistant

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// fakeMemory records every call and serves canned entries per space.
type fakeMemory struct {
	mu       sync.Mutex
	searches []memory.SearchQuery
	puts     []putCall
	gets     []string
	shares   []shareCall
	deletes  []string
	bySpace  map[domain.SpaceID][]memory.Entry
	errFor   map[domain.SpaceID]error
	putErr   error
	// getErr, shareErr and deleteErr make the fake fail the way lore fails. Without
	// them no test in this package could reach the publish or undo flows' error
	// paths at all, which is how those paths came to be missing their member-facing
	// half — the same pattern this file's Search comment warns about, for the fifth
	// time.
	getErr    error
	shareErr  error
	deleteErr error
}

type putCall struct {
	space domain.SpaceID
	draft memory.Draft
}

type shareCall struct {
	from, to domain.SpaceID
	entryID  string
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{
		bySpace: map[domain.SpaceID][]memory.Entry{},
		errFor:  map[domain.SpaceID]error{},
	}
}

// Search matches the query text the way lore does, conjunctively.
//
// It used to return everything seeded in the space and ignore q.Text, and that is
// why every test here passed while retrieval was dead in production: a fake that
// cannot miss cannot fail the way the real store fails. Same reasoning as the fake
// that once accepted lore space display names — a fake is only worth having if it
// refuses what production refuses.
func (f *fakeMemory) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, q)
	if len(q.Spaces) != 1 {
		return nil, memory.ErrEmptySpaceSet
	}
	if strings.TrimSpace(q.Text) == "" {
		// The real client refuses an empty query rather than returning the space.
		return nil, memory.ErrInvalidArgument
	}
	if err := f.errFor[q.Spaces[0]]; err != nil {
		return nil, err
	}
	var out []memory.Entry
	for _, e := range f.bySpace[q.Spaces[0]] {
		if loreMatch(e, q.Text) {
			out = append(out, e)
		}
	}
	// The limit is honoured, because it is load-bearing: the caller sets it to the
	// same budget it later truncates the union to, so a word the store holds many
	// entries for returns that whole budget by itself and leaves no room for a
	// precise word's hit. A fake that returns every match makes that impossible to
	// reproduce, which is the same way this file's fakes have hidden retrieval bugs
	// before. Seeded order stands in for lore's relevance ordering.
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// loreMatch reports whether an entry would come back from lore for this query:
// every query term must appear in the entry as a whole word. Whole word, not
// substring, because lore tokenises "quillfeather921834100" as one word and does
// not find it by "quillfeather"; and no stemming, because lore has none.
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

// Get serves out of the same canned spaces Search draws from, so a test can only
// Get what that space actually holds — which is the property the promotion flow
// depends on.
func (f *fakeMemory) Get(ctx context.Context, space domain.SpaceID, id string) (memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, id)
	if f.getErr != nil {
		return memory.Entry{}, f.getErr
	}
	for _, e := range f.bySpace[space] {
		if e.ID == id {
			return e, nil
		}
	}
	return memory.Entry{}, memory.ErrNotFound
}

// Put mirrors real lore's title/body/domain requirement (internal/memory.Client.Put)
// instead of accepting anything. A fake that accepts what lore refuses hides exactly
// this kind of defect from the test suite instead of catching it.
func (f *fakeMemory) Put(ctx context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return memory.Entry{}, f.putErr
	}
	if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Body) == "" || strings.TrimSpace(d.Domain) == "" {
		return memory.Entry{}, fmt.Errorf("memory: title, body and domain are required: %w", memory.ErrInvalidArgument)
	}
	f.puts = append(f.puts, putCall{space: space, draft: d})
	// Kept in the space, so a Delete that follows can find it and a Delete of
	// anything else cannot. A store you cannot read back what you just wrote from is
	// a fake that fails differently from the real one, which is the whole complaint
	// this file keeps making about its own history.
	e := memory.Entry{ID: "e-1", Space: space, Title: d.Title, Body: d.Body}
	f.bySpace[space] = append(f.bySpace[space], e)
	return e, nil
}

func (f *fakeMemory) Share(ctx context.Context, from, to domain.SpaceID, entryID string) (memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shares = append(f.shares, shareCall{from: from, to: to, entryID: entryID})
	if f.shareErr != nil {
		return memory.Entry{}, f.shareErr
	}
	for _, e := range f.bySpace[from] {
		if e.ID == entryID {
			e.Space = to
			return e, nil
		}
	}
	return memory.Entry{}, memory.ErrNotFound
}

// Delete removes the entry from the space it was seeded in, so a test can assert on
// the store's contents afterwards rather than only on the call. An id the space does
// not hold is ErrNotFound, as it is in lore: undoing something that is not there is
// not the same as undoing something that is, and only one of the two is a no-op the
// real store treats as success.
func (f *fakeMemory) Delete(ctx context.Context, space domain.SpaceID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, string(space)+"/"+id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for i, e := range f.bySpace[space] {
		if e.ID == id {
			f.bySpace[space] = append(f.bySpace[space][:i], f.bySpace[space][i+1:]...)
			return nil
		}
	}
	return memory.ErrNotFound
}

func (f *fakeMemory) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeMemory) gotIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gets...)
}

func (f *fakeMemory) sharedCalls() []shareCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]shareCall(nil), f.shares...)
}

func (f *fakeMemory) Close() error { return nil }

// searchedSpaces reports the distinct spaces searched, first use first. Distinct
// because a turn makes one search per query term per space, and the invariant these
// tests hold is which spaces were reachable, never how many queries it took.
func (f *fakeMemory) searchedSpaces() []domain.SpaceID {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[domain.SpaceID]bool{}
	var out []domain.SpaceID
	for _, q := range f.searches {
		for _, sp := range q.Spaces {
			if !seen[sp] {
				seen[sp] = true
				out = append(out, sp)
			}
		}
	}
	return out
}

func (f *fakeMemory) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// fakeTransport records sends and answers questions with a canned answer. A non-nil
// askGate makes Ask block until the gate is closed, standing in for a member who
// has not tapped a button yet.
type fakeTransport struct {
	mu      sync.Mutex
	sent    []transport.Outbound
	asked   []transport.Question
	answer  transport.Answer
	askGate chan struct{}
}

func (f *fakeTransport) Updates(ctx context.Context) (<-chan transport.Inbound, error) {
	return nil, transport.ErrClosed
}

func (f *fakeTransport) Send(ctx context.Context, o transport.Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, o)
	return nil
}

func (f *fakeTransport) Ask(ctx context.Context, q transport.Question) (transport.Answer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	gate := f.askGate
	answer := f.answer
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return transport.Answer{}, ctx.Err()
		}
	}
	return answer, nil
}

func (f *fakeTransport) Close() error { return nil }

// sentTexts returns what was sent with the retrieval line stripped off the front, so
// that a test about what the assistant said is not also a test of what the node
// reported about itself. TestReplyCarriesTheRetrievalLine and its neighbours use
// sentTextsRaw and are the only assertions on the line itself.
//
// The default configuration announces reads, so the suite runs the shipped behaviour
// and strips at the assertion rather than turning the feature off at the rig — a suite
// that only ever exercises a non-default setting proves nothing about the default.
func (f *fakeTransport) sentTexts() []string {
	out := f.sentTextsRaw()
	for i, s := range out {
		out[i] = withoutReadNotice(s)
	}
	return out
}

func (f *fakeTransport) sentTextsRaw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, o := range f.sent {
		out[i] = o.Text
	}
	return out
}

// withoutReadNotice removes a leading "<i>🔍 searched …</i>" line and the blank line
// after it. It matches only that exact shape, so a malformed line survives into the
// assertion that was going to compare it.
func withoutReadNotice(s string) string {
	if !strings.HasPrefix(s, "<i>🔍 searched ") {
		return s
	}
	_, rest, ok := strings.Cut(s, "</i>\n\n")
	if !ok {
		return s
	}
	return rest
}

func (f *fakeTransport) askCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// fakeRouter records chains and requests and delegates to fn.
type fakeRouter struct {
	mu     sync.Mutex
	chains [][]string
	reqs   []routing.Request
	fn     func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error)
}

func (f *fakeRouter) Complete(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
	f.mu.Lock()
	c := make([]string, len(chain))
	copy(c, chain)
	f.chains = append(f.chains, c)
	f.reqs = append(f.reqs, req)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, chain, req)
	}
	return routing.Completion{Text: "ok", Endpoint: "fake", Tier: "local"}, nil
}

func (f *fakeRouter) lastRequest() (routing.Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return routing.Request{}, false
	}
	return f.reqs[len(f.reqs)-1], true
}

// fakeSessions treats the ids in unlocked as having live keys.
type fakeSessions struct {
	mu       sync.Mutex
	unlocked map[domain.MemberID]bool
	touched  []domain.MemberID
}

var _ session.Sessions = (*fakeSessions)(nil)

func newFakeSessions(ids ...domain.MemberID) *fakeSessions {
	f := &fakeSessions{unlocked: map[domain.MemberID]bool{}}
	for _, id := range ids {
		f.unlocked[id] = true
	}
	return f
}

func (f *fakeSessions) Unlock(ctx context.Context, id domain.MemberID, passphrase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlocked[id] = true
	return nil
}

func (f *fakeSessions) Key(id domain.MemberID) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlocked[id] {
		return []byte("key"), true
	}
	return nil, false
}

func (f *fakeSessions) Touch(id domain.MemberID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
}

func (f *fakeSessions) Lock(id domain.MemberID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.unlocked, id)
}

func (f *fakeSessions) LockAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlocked = map[domain.MemberID]bool{}
}

// Test scopes, built directly so the tests exercise the Unit's contract rather than
// scope resolution, which has its own package and its own tests.

const (
	testMemberChat = int64(111)
	testGroupChat  = int64(-1001)
	testUserID     = int64(111)
)

func testDirectScope() domain.Scope {
	m := domain.Member{
		ID:         "david",
		Name:       "David",
		TelegramID: testUserID,
		Private:    "david-private",
		Tiers:      []string{"local"},
	}
	return domain.Scope{
		Kind:   domain.ScopeDirect,
		Member: &m,
		Write:  "david-private",
		Read:   []domain.SpaceID{"david-private", "household"},
		Tiers:  []string{"local"},
		ChatID: testMemberChat,
	}
}

func testGroupScope() domain.Scope {
	return domain.Scope{
		Kind:   domain.ScopeGroup,
		Member: nil,
		Write:  "household",
		Read:   []domain.SpaceID{"household"},
		Tiers:  []string{"local", "cloud"},
		ChatID: testGroupChat,
	}
}

func fixedResolver(sc domain.Scope) ResolveFunc {
	return func(in transport.Inbound) (domain.Scope, error) { return sc, nil }
}

func errResolver(err error) ResolveFunc {
	return func(in transport.Inbound) (domain.Scope, error) { return domain.Scope{}, err }
}

// testRig is one Unit wired to fakes.
type testRig struct {
	unit     *Unit
	mem      *fakeMemory
	tr       *fakeTransport
	router   *fakeRouter
	sessions *fakeSessions
}

func testOptions() Options {
	return Options{
		HouseholdName: "Home",
		Now: func() time.Time {
			return time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
		},
	}
}

func newTestRig(resolve ResolveFunc, opts Options) (*testRig, error) {
	mem := newFakeMemory()
	tr := &fakeTransport{}
	router := &fakeRouter{}
	sessions := newFakeSessions("david")
	unit, err := New(Deps{
		Resolve:   resolve,
		Memory:    mem,
		Router:    router,
		Transport: tr,
		Sessions:  sessions,
		Capture:   capture.New(mem, tr, capture.Options{}),
	}, opts)
	if err != nil {
		return nil, err
	}
	return &testRig{unit: unit, mem: mem, tr: tr, router: router, sessions: sessions}, nil
}

func directInbound(text string) transport.Inbound {
	return transport.Inbound{
		ChatID:    testMemberChat,
		UserID:    testUserID,
		Text:      text,
		MessageID: 1,
		At:        time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC),
	}
}

func groupInbound(text string) transport.Inbound {
	in := directInbound(text)
	in.ChatID = testGroupChat
	in.IsGroup = true
	return in
}

// entry fabricates one search hit. Body is the whole body, because that is what
// the real client guarantees for everything Search returns now that lore is
// imported rather than parsed: a search result carries the entry, and the fakes
// model that contract rather than the excerpt one it replaced.
func entry(space domain.SpaceID, title, body, confidence string, markers ...string) memory.Entry {
	return memory.Entry{
		ID:         "id-" + title,
		Space:      space,
		Title:      title,
		Body:       body,
		Confidence: confidence,
		Markers:    markers,
	}
}
