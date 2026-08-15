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
// is exactly what a crash-looping member looks like from the host. It also
// implements PodRecreator; a name in recreateBroken models a new image that
// crashes on startup — the recreate call itself succeeds, and the resulting
// container never runs.
type fakeBackend struct {
	mu             sync.Mutex
	created        map[string]sandbox.Spec
	running        map[string]bool
	crashLoop      map[string]bool
	recreateBroken map[string]bool
	// recreateWarmup makes a recreated pod report not-running for that many
	// Inspect calls before coming up, modelling a slow start.
	recreateWarmup map[string]int
	warmupLeft     map[string]int
	// recreateFlap makes a recreated pod report running exactly once and then
	// die — the health check that lies.
	recreateFlap map[string]bool
	flapArmed    map[string]bool
	// volumes models the named work volume: created once with Create, reused
	// by Recreate, deleted only by Purge. The id is the identity assertion —
	// a recreation that changed it would have lost the member's lore.
	volumes   map[string]int
	volSeq    int
	creates   map[string]int
	starts    map[string]int
	stops     map[string]int
	recreates map[string]int
	destroys  int
	events    []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		created:        make(map[string]sandbox.Spec),
		running:        make(map[string]bool),
		crashLoop:      make(map[string]bool),
		recreateBroken: make(map[string]bool),
		recreateWarmup: make(map[string]int),
		warmupLeft:     make(map[string]int),
		recreateFlap:   make(map[string]bool),
		flapArmed:      make(map[string]bool),
		volumes:        make(map[string]int),
		creates:        make(map[string]int),
		starts:         make(map[string]int),
		stops:          make(map[string]int),
		recreates:      make(map[string]int),
	}
}

func (b *fakeBackend) setCrashLoop(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.crashLoop[name] = true
}

// setRecreateBroken makes the named pod's next recreation produce a container
// that never runs, as a broken new image would.
func (b *fakeBackend) setRecreateBroken(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recreateBroken[name] = true
}

// setRecreateWarmup makes the named pod report not-running for n Inspect calls
// after each recreation before coming up.
func (b *fakeBackend) setRecreateWarmup(name string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recreateWarmup[name] = n
}

// setRecreateFlap makes the named pod report running exactly once after
// recreation and then die.
func (b *fakeBackend) setRecreateFlap(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recreateFlap[name] = true
}

// volumeID returns the identity of the pod's work volume.
func (b *fakeBackend) volumeID(name string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.volumes[name]
	return id, ok
}

func (b *fakeBackend) isRunning(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running[name]
}

// recreations lists the pods recreated so far, in order.
func (b *fakeBackend) recreations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, e := range b.events {
		if strings.HasPrefix(e, "recreate ") {
			out = append(out, strings.TrimPrefix(e, "recreate "))
		}
	}
	return out
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

func (b *fakeBackend) recreated(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recreates[name]
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
	// Volume creation is idempotent, as in keel: an existing volume is reused
	// with its identity — and its data — intact.
	if _, ok := b.volumes[spec.Name]; !ok {
		b.volSeq++
		b.volumes[spec.Name] = b.volSeq
	}
	b.events = append(b.events, "create "+spec.Name)
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
	b.events = append(b.events, "start "+id)
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
	b.events = append(b.events, "stop "+id)
	return nil
}

// Recreate mirrors keel's contract: the container is replaced from the spec and
// the work volume survives with its identity intact — no path through here
// touches b.volumes. A name marked recreateBroken becomes a crash-looper from
// here on — its new image never runs; warmup and flap script slow starts and
// lying health.
func (b *fakeBackend) Recreate(_ context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.recreateBroken[spec.Name] {
		b.crashLoop[spec.Name] = true
	}
	b.created[spec.Name] = spec
	b.recreates[spec.Name]++
	b.running[spec.Name] = !b.crashLoop[spec.Name]
	if w := b.recreateWarmup[spec.Name]; w > 0 && b.running[spec.Name] {
		b.warmupLeft[spec.Name] = w
		b.running[spec.Name] = false
	}
	if b.recreateFlap[spec.Name] {
		b.running[spec.Name] = true
		b.flapArmed[spec.Name] = true
	}
	b.events = append(b.events, "recreate "+spec.Name)
	return sandbox.Handle{ID: spec.Name}, nil
}

// Purge deletes the pod and its volume — the one call the supervisor must never
// make; purges() lets tests assert it never happened.
func (b *fakeBackend) Purge(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroys++
	delete(b.created, id)
	delete(b.running, id)
	delete(b.volumes, id)
	return nil
}

func (b *fakeBackend) Inspect(_ context.Context, id string) (sandbox.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.created[id]; !ok {
		return sandbox.Status{}, sandbox.ErrSandboxNotFound
	}
	if n := b.warmupLeft[id]; n > 0 {
		b.warmupLeft[id] = n - 1
		if b.warmupLeft[id] == 0 {
			b.running[id] = true
		}
		return sandbox.Status{Running: false}, nil
	}
	running := b.running[id]
	if running && b.flapArmed[id] {
		// Seen alive exactly once; dead by the next look.
		b.flapArmed[id] = false
		b.running[id] = false
	}
	return sandbox.Status{Running: running}, nil
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
