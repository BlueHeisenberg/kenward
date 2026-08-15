package supervisor

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// isolatedTestConfig is a household of two enrolled members, one unenrolled, and
// a group chat, each with their own bot token variable.
func isolatedTestConfig() *config.Config {
	return &config.Config{
		Mode: config.ModeIsolated,
		Household: config.HouseholdConfig{
			Name:        "Home",
			SharedSpace: "household",
			GroupChatID: groupChatID,
			Tiers:       []string{"local"},
		},
		Telegram: config.TelegramConfig{BotTokenEnv: "TOK_GROUP"},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", TelegramID: 1, PrivateSpace: "d", Tiers: []string{"local"}, BotTokenEnv: "TOK_DAVID"},
			{ID: "eve", Name: "Eve", TelegramID: 2, PrivateSpace: "e", Tiers: []string{"local"}, BotTokenEnv: "TOK_EVE"},
			{ID: "ana", Name: "Ana", PrivateSpace: "a", Tiers: []string{"local"}},
		},
	}
}

func testLookupEnv(name string) (string, bool) { return "token-for-" + name, true }

func isolatedTestOptions(b *fakeBackend) IsolatedOptions {
	return IsolatedOptions{
		Backend:           b,
		Image:             "example.test/kenward:dev",
		LookupEnv:         testLookupEnv,
		PollInterval:      time.Millisecond,
		RestartBackoff:    4 * time.Millisecond,
		MaxRestartBackoff: 16 * time.Millisecond,
		HealthyReset:      time.Hour, // never reset inside a test
	}
}

func TestIsolatedRefusesOffLinux(t *testing.T) {
	cfg := isolatedTestConfig()
	opts := isolatedTestOptions(newFakeBackend())

	for _, goos := range []string{"windows", "darwin", "freebsd"} {
		_, err := newIsolated(cfg, opts, goos)
		if !errors.Is(err, ErrUnsupportedMode) {
			t.Fatalf("newIsolated on %s = %v, want ErrUnsupportedMode", goos, err)
		}
	}

	// The exported constructor gives the same refusal on this host, never a
	// silent downgrade to anything.
	if runtime.GOOS != "linux" {
		_, err := NewIsolated(cfg, opts)
		if !errors.Is(err, ErrUnsupportedMode) {
			t.Fatalf("NewIsolated on %s = %v, want ErrUnsupportedMode", runtime.GOOS, err)
		}
	}

	// It works as an error value, not a downgrade: nothing was created.
	if n := len(newFakeBackend().created); n != 0 {
		t.Fatalf("refusal created %d pods", n)
	}
}

func TestIsolatedConstructionErrors(t *testing.T) {
	backend := newFakeBackend()

	cfg := isolatedTestConfig()
	cfg.Mode = config.ModeSimple
	if _, err := newIsolated(cfg, isolatedTestOptions(backend), "linux"); err == nil {
		t.Fatal("newIsolated accepted a simple-mode configuration")
	}

	noImage := isolatedTestOptions(backend)
	noImage.Image = ""
	if _, err := newIsolated(isolatedTestConfig(), noImage, "linux"); err == nil {
		t.Fatal("newIsolated accepted an empty pod image")
	}

	// A missing bot token refuses to start rather than silently skipping the pod.
	missing := isolatedTestOptions(backend)
	missing.LookupEnv = func(name string) (string, bool) {
		if name == "TOK_EVE" {
			return "", false
		}
		return "x", true
	}
	_, err := newIsolated(isolatedTestConfig(), missing, "linux")
	if err == nil || !strings.Contains(err.Error(), "TOK_EVE") {
		t.Fatalf("missing token error = %v, want it to name TOK_EVE", err)
	}

	// Nothing to run at all.
	empty := isolatedTestConfig()
	empty.Members = nil
	empty.Household.GroupChatID = 0
	if _, err := newIsolated(empty, isolatedTestOptions(backend), "linux"); !errors.Is(err, ErrNoUnits) {
		t.Fatalf("empty household = %v, want ErrNoUnits", err)
	}
}

// isolatedHarness runs an Isolated over a fake backend.
type isolatedHarness struct {
	sup      *Isolated
	backend  *fakeBackend
	cancel   context.CancelFunc
	startErr chan error
}

func newIsolatedHarness(t *testing.T, mutate func(*IsolatedOptions)) *isolatedHarness {
	t.Helper()
	h := &isolatedHarness{backend: newFakeBackend(), startErr: make(chan error, 1)}
	opts := isolatedTestOptions(h.backend)
	if mutate != nil {
		mutate(&opts)
	}
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	h.sup = sup
	return h
}

func (h *isolatedHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.startErr <- h.sup.Start(ctx) }()
}

func (h *isolatedHarness) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-h.startErr:
		if err != nil {
			t.Fatalf("Start returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	h.cancel()
}

func TestIsolatedRunsOnePodPerUnit(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.start(t)

	waitFor(t, "pods ready", func() bool {
		hs := mustHealth(t, h.sup)
		return hs["david"].State == StateReady &&
			hs["eve"].State == StateReady &&
			hs["group"].State == StateReady
	})

	// One pod per enrolled member plus the group's; the unenrolled member has
	// none and is reported as such.
	for _, name := range []string{"kenward-member-david", "kenward-member-eve", "kenward-group"} {
		spec, ok := h.backend.spec(name)
		if !ok {
			t.Fatalf("pod %s was not created", name)
		}
		if spec.Env[EnvLoreHome] != DefaultLoreHome {
			t.Fatalf("pod %s LORE_HOME = %q", name, spec.Env[EnvLoreHome])
		}
	}
	if _, ok := h.backend.spec("kenward-member-ana"); ok {
		t.Fatal("a pod was created for an unenrolled member")
	}
	hs := mustHealth(t, h.sup)
	if !errors.Is(hs["ana"].Err, ErrNotEnrolled) || hs["ana"].State != StateUnknown {
		t.Fatalf("unenrolled member = %+v, want unknown/not-enrolled", hs["ana"])
	}

	// Each pod carries its own bot token under the variable the configuration
	// named, and its unit selector.
	david, _ := h.backend.spec("kenward-member-david")
	if david.Env["TOK_DAVID"] != "token-for-TOK_DAVID" || david.Env[EnvMember] != "david" {
		t.Fatalf("david pod env = %v", david.Env)
	}
	group, _ := h.backend.spec("kenward-group")
	if group.Env["TOK_GROUP"] != "token-for-TOK_GROUP" || group.Env[EnvGroup] != "1" {
		t.Fatalf("group pod env = %v", group.Env)
	}
	if david.Env["TOK_EVE"] != "" || group.Env["TOK_DAVID"] != "" {
		t.Fatal("a pod was handed another unit's bot token")
	}

	h.stop(t)

	// Graceful stop for every pod; Destroy never — the work volume is where the
	// member's lore lives.
	for _, name := range []string{"kenward-member-david", "kenward-member-eve", "kenward-group"} {
		if _, _, stops := h.backend.counts(name); stops != 1 {
			t.Fatalf("pod %s stopped %d times, want 1", name, stops)
		}
	}
	if h.backend.destroyed() != 0 {
		t.Fatalf("Destroy called %d times, want never", h.backend.destroyed())
	}
	hs = mustHealth(t, h.sup)
	if hs["david"].State != StateStopped || hs["group"].State != StateStopped {
		t.Fatalf("after Stop: david=%v group=%v, want stopped", hs["david"].State, hs["group"].State)
	}
}

func TestIsolatedPerPodRestartWithBackoff(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.backend.setCrashLoop("kenward-member-eve")

	var mu sync.Mutex
	delays := map[domain.MemberID][]time.Duration{}
	h.sup.testHookBackoff = func(k unitKey, d time.Duration) {
		mu.Lock()
		delays[k.member] = append(delays[k.member], d)
		mu.Unlock()
	}

	h.start(t)

	waitFor(t, "eve to crash-loop through the backoff ladder", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays["eve"]) >= 4
	})

	mu.Lock()
	got := append([]time.Duration(nil), delays["eve"][:4]...)
	davidDelays := len(delays["david"])
	mu.Unlock()

	want := []time.Duration{4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 16 * time.Millisecond}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("eve backoff ladder = %v, want %v", got, want)
		}
	}

	hs := mustHealth(t, h.sup)
	if hs["eve"].Restarts < 3 {
		t.Fatalf("eve restarts = %d, want the counter climbing", hs["eve"].Restarts)
	}
	if hs["eve"].Err == nil {
		t.Fatal("eve's failure was not retained")
	}

	// One member crash-looping never touches the rest of the household.
	if davidDelays != 0 {
		t.Fatalf("david was backed off %d times by eve's crash loop", davidDelays)
	}
	if hs["david"].State != StateReady || hs["david"].Restarts != 0 {
		t.Fatalf("david = %+v, want ready with zero restarts", hs["david"])
	}
	if hs["group"].State != StateReady {
		t.Fatalf("group = %v, want ready", hs["group"].State)
	}

	h.stop(t)
}

func TestIsolatedRestartCounterAndRecovery(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.start(t)

	waitFor(t, "david ready", func() bool {
		return mustHealth(t, h.sup)["david"].State == StateReady
	})

	// The pod dies once; the supervisor notices, counts it, restarts it, and the
	// failure stays visible after recovery.
	h.backend.kill("kenward-member-david")
	waitFor(t, "david restarted", func() bool {
		hs := mustHealth(t, h.sup)
		return hs["david"].Restarts == 1 && hs["david"].State == StateReady
	})
	hs := mustHealth(t, h.sup)
	if hs["david"].Err == nil {
		t.Fatal("recovered pod's Err was cleared; it must be retained")
	}
	if _, starts, _ := h.backend.counts("kenward-member-david"); starts < 1 {
		t.Fatal("pod was never restarted through the backend")
	}
	if h.backend.destroyed() != 0 {
		t.Fatal("a restart destroyed a pod")
	}

	h.stop(t)
}

func TestIsolatedHealthBeforeStartAndAfterStop(t *testing.T) {
	h := newIsolatedHarness(t, nil)

	hs := mustHealth(t, h.sup)
	if len(hs) != 4 {
		t.Fatalf("Health before Start reported %d units, want 4", len(hs))
	}
	for name, u := range hs {
		if u.State != StateUnknown {
			t.Fatalf("unit %s before Start = %v, want unknown", name, u.State)
		}
	}

	h.start(t)
	waitFor(t, "ready", func() bool { return mustHealth(t, h.sup)["david"].State == StateReady })
	h.stop(t)

	if got := mustHealth(t, h.sup)["eve"].State; got != StateStopped {
		t.Fatalf("after Stop eve = %v, want stopped", got)
	}
	if err := h.sup.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestIsolatedNoGoroutineLeaksAfterStop(t *testing.T) {
	before := runtime.NumGoroutine()

	h := newIsolatedHarness(t, nil)
	h.start(t)
	waitFor(t, "ready", func() bool { return mustHealth(t, h.sup)["david"].State == StateReady })
	h.stop(t)

	waitFor(t, "goroutines to drain", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}
