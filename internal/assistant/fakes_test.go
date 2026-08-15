package assistant

import (
	"context"
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
	bySpace  map[domain.SpaceID][]memory.Entry
	errFor   map[domain.SpaceID]error
	putErr   error
}

type putCall struct {
	space domain.SpaceID
	draft memory.Draft
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{
		bySpace: map[domain.SpaceID][]memory.Entry{},
		errFor:  map[domain.SpaceID]error{},
	}
}

func (f *fakeMemory) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, q)
	if len(q.Spaces) != 1 {
		return nil, memory.ErrEmptySpaceSet
	}
	if err := f.errFor[q.Spaces[0]]; err != nil {
		return nil, err
	}
	return f.bySpace[q.Spaces[0]], nil
}

func (f *fakeMemory) Get(ctx context.Context, space domain.SpaceID, id string) (memory.Entry, error) {
	return memory.Entry{}, memory.ErrNotFound
}

func (f *fakeMemory) Put(ctx context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return memory.Entry{}, f.putErr
	}
	f.puts = append(f.puts, putCall{space: space, draft: d})
	return memory.Entry{ID: "e-1", Space: space, Title: d.Title, Body: d.Body}, nil
}

func (f *fakeMemory) Share(ctx context.Context, from, to domain.SpaceID, entryID string) (memory.Entry, error) {
	return memory.Entry{}, memory.ErrNotFound
}

func (f *fakeMemory) Close() error { return nil }

func (f *fakeMemory) searchedSpaces() []domain.SpaceID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.SpaceID
	for _, q := range f.searches {
		out = append(out, q.Spaces...)
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

func (f *fakeTransport) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, o := range f.sent {
		out[i] = o.Text
	}
	return out
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

// entry fabricates one search hit. Partial is set because that is what the real
// lore client guarantees for everything Search returns: a search result is an
// excerpt, and the fakes model that contract rather than a friendlier one.
func entry(space domain.SpaceID, title, body, confidence string, markers ...string) memory.Entry {
	return memory.Entry{
		ID:         "id-" + title,
		Space:      space,
		Title:      title,
		Body:       body,
		Confidence: confidence,
		Markers:    markers,
		Partial:    true,
	}
}
