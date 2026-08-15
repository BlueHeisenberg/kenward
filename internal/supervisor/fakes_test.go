package supervisor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/sandbox"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// waitFor polls cond until it reports true or the test's patience runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- memory ------------------------------------------------------------------

type fakeMemory struct {
	mu       sync.Mutex
	searches []memory.SearchQuery
}

func (m *fakeMemory) Search(_ context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searches = append(m.searches, q)
	return nil, nil
}

func (m *fakeMemory) Get(context.Context, domain.SpaceID, string) (memory.Entry, error) {
	return memory.Entry{}, memory.ErrNotFound
}

func (m *fakeMemory) Put(_ context.Context, space domain.SpaceID, d memory.Draft) (memory.Entry, error) {
	return memory.Entry{ID: "e1", Space: space, Title: d.Title, Body: d.Body}, nil
}

func (m *fakeMemory) Share(_ context.Context, _, to domain.SpaceID, id string) (memory.Entry, error) {
	return memory.Entry{ID: id, Space: to}, nil
}

func (m *fakeMemory) Close() error { return nil }

// searchedSpaces flattens every recorded query's space set.
func (m *fakeMemory) searchedSpaces() []domain.SpaceID {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.SpaceID
	for _, q := range m.searches {
		out = append(out, q.Spaces...)
	}
	return out
}

// --- routing -----------------------------------------------------------------

// fakeRouter answers every completion with the tier chain it was given, so a test
// can tell which unit's scope produced a reply. It can be gated to hold a turn in
// flight, and told to panic once to exercise the restart path.
type fakeRouter struct {
	mu        sync.Mutex
	calls     [][]string
	panicOnce bool

	gate    chan struct{} // when non-nil, Complete blocks until closed
	entered chan struct{} // when non-nil, receives one token per Complete call
}

func (r *fakeRouter) Complete(ctx context.Context, chain []string, _ routing.Request) (routing.Completion, error) {
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	r.mu.Lock()
	shouldPanic := r.panicOnce
	r.panicOnce = false
	gate := r.gate
	r.calls = append(r.calls, append([]string(nil), chain...))
	r.mu.Unlock()

	if shouldPanic {
		panic("fakeRouter: scripted panic")
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return routing.Completion{}, ctx.Err()
		}
	}
	return routing.Completion{
		Text:         "via:" + strings.Join(chain, ","),
		FinishReason: routing.FinishStop,
	}, nil
}

// --- sessions ----------------------------------------------------------------

type fakeSessions struct {
	mu           sync.Mutex
	unlocked     map[domain.MemberID][]byte
	lockAllCalls int
	onLockAll    func()
}

func newFakeSessions(ids ...domain.MemberID) *fakeSessions {
	s := &fakeSessions{unlocked: make(map[domain.MemberID][]byte)}
	for _, id := range ids {
		s.unlocked[id] = []byte("key")
	}
	return s
}

func (s *fakeSessions) Unlock(_ context.Context, id domain.MemberID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unlocked[id] = []byte("key")
	return nil
}

func (s *fakeSessions) Key(id domain.MemberID) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.unlocked[id]
	return k, ok
}

func (s *fakeSessions) Touch(domain.MemberID) {}

func (s *fakeSessions) Lock(id domain.MemberID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unlocked, id)
}

func (s *fakeSessions) LockAll() {
	s.mu.Lock()
	s.unlocked = make(map[domain.MemberID][]byte)
	s.lockAllCalls++
	fn := s.onLockAll
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *fakeSessions) lockAllCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockAllCalls
}

// unlock adds ids after construction, for members enrolled mid-test.
func (s *fakeSessions) unlock(ids ...domain.MemberID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.unlocked[id] = []byte("key")
	}
}

// --- sandbox backend ---------------------------------------------------------

// fakeBackend scripts pod behaviour by name. A pod whose name is in crashLoop
// reports not-running on every Inspect no matter how often it is started, which
// is exactly what a crash-looping member looks like from the host.
type fakeBackend struct {
	mu        sync.Mutex
	created   map[string]sandbox.Spec
	running   map[string]bool
	crashLoop map[string]bool
	creates   map[string]int
	starts    map[string]int
	stops     map[string]int
	destroys  int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		created:   make(map[string]sandbox.Spec),
		running:   make(map[string]bool),
		crashLoop: make(map[string]bool),
		creates:   make(map[string]int),
		starts:    make(map[string]int),
		stops:     make(map[string]int),
	}
}

func (b *fakeBackend) setCrashLoop(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.crashLoop[name] = true
}

func (b *fakeBackend) kill(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running[name] = false
}

func (b *fakeBackend) counts(name string) (creates, starts, stops int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.creates[name], b.starts[name], b.stops[name]
}

func (b *fakeBackend) destroyed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.destroys
}

func (b *fakeBackend) spec(name string) (sandbox.Spec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.created[name]
	return s, ok
}

func (b *fakeBackend) Create(_ context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created[spec.Name] = spec
	b.creates[spec.Name]++
	b.running[spec.Name] = !b.crashLoop[spec.Name]
	return sandbox.Handle{ID: spec.Name}, nil
}

func (b *fakeBackend) Start(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.created[id]; !ok {
		return sandbox.ErrSandboxNotFound
	}
	b.starts[id]++
	b.running[id] = !b.crashLoop[id]
	return nil
}

func (b *fakeBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.created[id]; !ok {
		return sandbox.ErrSandboxNotFound
	}
	b.stops[id]++
	b.running[id] = false
	return nil
}

func (b *fakeBackend) Destroy(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroys++
	delete(b.created, id)
	delete(b.running, id)
	return nil
}

func (b *fakeBackend) Inspect(_ context.Context, id string) (sandbox.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.created[id]; !ok {
		return sandbox.Status{}, sandbox.ErrSandboxNotFound
	}
	return sandbox.Status{Running: b.running[id]}, nil
}

func (b *fakeBackend) Exec(context.Context, string, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, errors.New("fakeBackend: Exec not scripted")
}

func (b *fakeBackend) ExecStream(context.Context, string, []string, sandbox.ExecOpts) (io.ReadCloser, error) {
	return nil, errors.New("fakeBackend: ExecStream not scripted")
}

func (b *fakeBackend) WriteFile(context.Context, string, string, []byte) error {
	return errors.New("fakeBackend: WriteFile not scripted")
}

func (b *fakeBackend) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("fakeBackend: ReadFile not scripted")
}

func (b *fakeBackend) Snapshot(context.Context, string, string) (sandbox.SnapshotRef, error) {
	return sandbox.SnapshotRef{}, errors.New("fakeBackend: Snapshot not scripted")
}

func (b *fakeBackend) RemoveSnapshot(context.Context, string) error {
	return errors.New("fakeBackend: RemoveSnapshot not scripted")
}

func (b *fakeBackend) Restore(context.Context, string, sandbox.SnapshotRef) (sandbox.Handle, error) {
	return sandbox.Handle{}, errors.New("fakeBackend: Restore not scripted")
}

func (b *fakeBackend) DesktopEndpoint(context.Context, string) (string, error) {
	return "", errors.New("fakeBackend: DesktopEndpoint not scripted")
}

func (b *fakeBackend) WebEndpoint(context.Context, string) (string, error) {
	return "", errors.New("fakeBackend: WebEndpoint not scripted")
}

func (b *fakeBackend) ContainerAddr(context.Context, string, int) (string, error) {
	return "", errors.New("fakeBackend: ContainerAddr not scripted")
}

var _ sandbox.Backend = (*fakeBackend)(nil)

// healthByMember indexes a Health snapshot for assertions.
func healthByMember(hs []UnitHealth) map[string]UnitHealth {
	out := make(map[string]UnitHealth, len(hs))
	for _, h := range hs {
		key := string(h.Member)
		if h.Group {
			key = "group"
		}
		out[key] = h
	}
	return out
}

// mustHealth fetches Health and fails the test on error.
func mustHealth(t *testing.T, s Supervisor) map[string]UnitHealth {
	t.Helper()
	hs, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return healthByMember(hs)
}
