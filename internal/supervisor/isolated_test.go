package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/sandbox"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// fakeSecretFS serves secret files from memory, always 0600. Paths are
// normalised to forward slashes so credential paths joined with the host OS
// separator still match.
type fakeSecretFS map[string]string

func (f fakeSecretFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	v, ok := f[strings.ReplaceAll(path, "\\", "/")]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return []byte(v), 0o600, nil
}

// isolatedTestConfig is a household of two enrolled members, one unenrolled, and
// a group chat, each with their own bot token variable.
//
// The unenrolled member has a token and a passphrase like everybody else, because
// internal/config requires both of her: in this mode her bot exists before she
// claims (D-023), and the claim provisions her key under her own passphrase inside
// her own pod. A fixture that omitted them described a household kenward would have
// refused to load.
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
			{ID: "david", Name: "David", TelegramID: 1, PrivateSpace: "d", Tiers: []string{"local"}, BotTokenEnv: "TOK_DAVID", PassphraseEnv: "PASS_DAVID"},
			{ID: "eve", Name: "Eve", TelegramID: 2, PrivateSpace: "e", Tiers: []string{"local"}, BotTokenEnv: "TOK_EVE", PassphraseEnv: "PASS_EVE"},
			{ID: "ana", Name: "Ana", PrivateSpace: "a", Tiers: []string{"local"}, BotTokenEnv: "TOK_ANA", PassphraseEnv: "PASS_ANA"},
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

	// One pod per member — enrolled or not, see TestIsolatedStartsAPodForAMember
	// WhoHasNotClaimed — plus the group's.
	for _, name := range []string{"kenward-member-david", "kenward-member-eve", "kenward-member-ana", "kenward-group"} {
		spec, ok := h.backend.spec(name)
		if !ok {
			t.Fatalf("pod %s was not created", name)
		}
		if spec.Env[EnvLoreHome] != DefaultLoreHome {
			t.Fatalf("pod %s LORE_HOME = %q", name, spec.Env[EnvLoreHome])
		}
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

	// Graceful stop for every pod; Purge never — the work volume is where the
	// member's lore lives.
	for _, name := range []string{"kenward-member-david", "kenward-member-eve", "kenward-member-ana", "kenward-group"} {
		if _, _, stops := h.backend.counts(name); stops != 1 {
			t.Fatalf("pod %s stopped %d times, want 1", name, stops)
		}
	}
	if h.backend.destroyed() != 0 {
		t.Fatalf("Purge called %d times, want never", h.backend.destroyed())
	}
	hs := mustHealth(t, h.sup)
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
	// Every unit, the unenrolled member included: she has a pod now, and before
	// Start nothing has been observed about any of them.
	for name, u := range hs {
		if u.State != StateUnknown {
			t.Fatalf("unit %s before Start = %v, want %v", name, u.State, StateUnknown)
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

// waitAllReady waits for the three pods of the standard test household.
func (h *isolatedHarness) waitAllReady(t *testing.T) {
	t.Helper()
	waitFor(t, "pods ready", func() bool {
		hs := mustHealth(t, h.sup)
		return hs["david"].State == StateReady &&
			hs["eve"].State == StateReady &&
			hs["group"].State == StateReady
	})
}

func TestIsolatedRollRecreatesInOrderAndWaitsForHealth(t *testing.T) {
	h := newIsolatedHarness(t, func(o *IsolatedOptions) {
		o.RollTimeout = time.Second
	})
	h.start(t)
	h.waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Roll(ctx); err != nil {
		t.Fatalf("Roll: %v", err)
	}

	// Deterministic order: members in configuration order, the group last. Roll
	// only returned nil because every pod held healthy across two polls before
	// the next was touched, so a completed roll is itself the health-wait proof;
	// the stop-on-failure test covers the sequencing when a wait fails.
	want := []string{"kenward-member-david", "kenward-member-eve", "kenward-member-ana", "kenward-group"}
	got := h.backend.recreations()
	if len(got) != len(want) {
		t.Fatalf("recreations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recreation order = %v, want %v", got, want)
		}
	}

	// Each pod was gracefully stopped exactly once for its recreation — the
	// drain — and recreated exactly once.
	for _, name := range want {
		if _, _, stops := h.backend.counts(name); stops != 1 {
			t.Fatalf("pod %s stopped %d times during roll, want 1", name, stops)
		}
		if n := h.backend.recreated(name); n != 1 {
			t.Fatalf("pod %s recreated %d times, want 1", name, n)
		}
	}

	hs := mustHealth(t, h.sup)
	for _, unit := range []string{"david", "eve", "group"} {
		if hs[unit].State != StateReady {
			t.Fatalf("after roll, %s = %v, want ready", unit, hs[unit].State)
		}
	}

	h.stop(t)
}

func TestIsolatedRollPreservesVolumeIdentity(t *testing.T) {
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.RollTimeout = time.Second })
	h.start(t)
	h.waitAllReady(t)

	names := []string{"kenward-member-david", "kenward-member-eve", "kenward-member-ana", "kenward-group"}
	before := map[string]int{}
	for _, name := range names {
		id, ok := h.backend.volumeID(name)
		if !ok {
			t.Fatalf("pod %s has no volume", name)
		}
		before[name] = id
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Roll(ctx); err != nil {
		t.Fatalf("Roll: %v", err)
	}

	// The recreated pods are attached to the same volumes, by identity. A roll
	// that came back with fresh empty volumes would look exactly this
	// successful from every other angle — and the members' memory would be
	// gone. This is the assertion behind splitting Recreate from Purge.
	for _, name := range names {
		id, ok := h.backend.volumeID(name)
		if !ok || id != before[name] {
			t.Fatalf("pod %s volume identity changed across roll: %d -> %d (ok=%v)", name, before[name], id, ok)
		}
	}
	if h.backend.destroyed() != 0 {
		t.Fatal("a roll purged a pod")
	}

	h.stop(t)
}

func TestIsolatedRollFailsWhenHealthLies(t *testing.T) {
	// The new pod reports running exactly once and then dies — the health check
	// that lies. One sighting is not health: the supervisor requires the pod to
	// hold running across consecutive polls, so this roll fails at david and
	// nobody else is touched. What "healthy" means here is a property of the
	// pod process, held over time; the unit inside exits on fatal wiring
	// errors, which is what makes process-up meaningful at all.
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.RollTimeout = time.Second })
	h.backend.setRecreateFlap("kenward-member-david")
	h.start(t)
	h.waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.sup.Roll(ctx)

	var re *RollError
	if !errors.As(err, &re) || re.Member != "david" {
		t.Fatalf("Roll = %v, want a *RollError at david", err)
	}
	if !strings.Contains(err.Error(), "after recreation") {
		t.Fatalf("Roll error = %v, want it to say the pod died after recreation", err)
	}
	if got := h.backend.recreations(); len(got) != 1 || got[0] != "kenward-member-david" {
		t.Fatalf("recreations = %v, want david only", got)
	}

	h.stop(t)
}

func TestIsolatedRollInterruptedMidwayIsCoherent(t *testing.T) {
	// Stop lands while eve's new pod is still warming up. What must be left
	// behind is coherent: david rolled and healthy, eve recreated (not stopped
	// and abandoned), the group untouched on its working old pod.
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.RollTimeout = 10 * time.Second })
	h.backend.setRecreateWarmup("kenward-member-eve", 1<<30)
	h.start(t)
	h.waitAllReady(t)

	rollErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rollErr <- h.sup.Roll(ctx)
	}()
	waitFor(t, "roll to reach eve", func() bool {
		return h.backend.recreated("kenward-member-eve") == 1
	})

	// Before the shutdown: nobody is stopped-but-not-recreated, and the group
	// has not been touched at all.
	if _, _, stops := h.backend.counts("kenward-group"); stops != 0 {
		t.Fatalf("group was stopped %d times mid-roll, want 0", stops)
	}
	if h.backend.recreated("kenward-group") != 0 {
		t.Fatal("group was recreated after the roll was interrupted")
	}
	if h.backend.recreated("kenward-member-david") != 1 {
		t.Fatal("david was not rolled before the interruption")
	}

	h.stop(t)
	if err := <-rollErr; err == nil {
		t.Fatal("interrupted Roll returned nil")
	}
	if got := h.backend.recreations(); len(got) != 2 {
		t.Fatalf("recreations after interruption = %v, want david and eve only", got)
	}
}

// fiveUnitConfig is four members plus the group, for the failure-mid-sequence
// case.
func fiveUnitConfig() *config.Config {
	cfg := &config.Config{
		Mode: config.ModeIsolated,
		Household: config.HouseholdConfig{
			Name: "Home", SharedSpace: "household", GroupChatID: groupChatID, Tiers: []string{"local"},
		},
		Telegram: config.TelegramConfig{BotTokenEnv: "TOK_GROUP"},
	}
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("m%d", i)
		cfg.Members = append(cfg.Members, config.MemberConfig{
			ID: id, Name: id, TelegramID: int64(i),
			PrivateSpace: id + "-private", Tiers: []string{"local"},
			BotTokenEnv:   "TOK_" + id,
			PassphraseEnv: "PASS_" + id,
		})
	}
	return cfg
}

func TestIsolatedRollFailureLeavesSurvivorsServing(t *testing.T) {
	backend := newFakeBackend()
	opts := isolatedTestOptions(backend)
	opts.RollTimeout = 40 * time.Millisecond
	sup, err := newIsolated(fiveUnitConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	backend.setRecreateBroken("kenward-member-m3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- sup.Start(ctx) }()
	waitFor(t, "all five ready", func() bool {
		hs := mustHealth(t, sup)
		return hs["m1"].State == StateReady && hs["m2"].State == StateReady &&
			hs["m3"].State == StateReady && hs["m4"].State == StateReady &&
			hs["group"].State == StateReady
	})

	rollCtx, rollCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rollCancel()
	rerr := sup.Roll(rollCtx)
	var re *RollError
	if !errors.As(rerr, &re) || re.Member != "m3" {
		t.Fatalf("Roll = %v, want a *RollError at m3", rerr)
	}

	// Members one and two rolled; three failed; four and the group were never
	// touched and are still serving — that is what stopping on failure means.
	for name, want := range map[string]int{
		"kenward-member-m1": 1, "kenward-member-m2": 1, "kenward-member-m3": 1,
		"kenward-member-m4": 0, "kenward-group": 0,
	} {
		if got := backend.recreated(name); got != want {
			t.Fatalf("pod %s recreated %d times, want %d", name, got, want)
		}
	}
	for _, survivor := range []string{"kenward-member-m4", "kenward-group"} {
		if !backend.isRunning(survivor) {
			t.Fatalf("survivor %s is not running after the failed roll", survivor)
		}
	}
	hs := mustHealth(t, sup)
	if hs["m4"].State != StateReady || hs["group"].State != StateReady {
		t.Fatalf("survivors after failed roll: m4=%v group=%v, want ready", hs["m4"].State, hs["group"].State)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

func TestIsolatedRollsAreExclusive(t *testing.T) {
	// Whatever two concurrent rolls could mean, it must never be two recreates
	// of the same member.
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.RollTimeout = 10 * time.Second })
	h.backend.setRecreateWarmup("kenward-member-david", 100)
	h.start(t)
	h.waitAllReady(t)

	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		first <- h.sup.Roll(ctx)
	}()
	waitFor(t, "first roll to begin", func() bool {
		return h.backend.recreated("kenward-member-david") == 1
	})

	if err := h.sup.Roll(context.Background()); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent Roll = %v, want already-in-progress", err)
	}

	if err := <-first; err != nil {
		t.Fatalf("first Roll: %v", err)
	}
	for _, name := range []string{"kenward-member-david", "kenward-member-eve", "kenward-group"} {
		if got := h.backend.recreated(name); got != 1 {
			t.Fatalf("pod %s recreated %d times, want exactly 1", name, got)
		}
	}

	h.stop(t)
}

func TestIsolatedRollStopsOnFirstFailure(t *testing.T) {
	before := runtime.NumGoroutine()

	h := newIsolatedHarness(t, func(o *IsolatedOptions) {
		o.RollTimeout = 40 * time.Millisecond
	})
	h.backend.setRecreateBroken("kenward-member-eve")
	h.start(t)
	h.waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.sup.Roll(ctx)

	var re *RollError
	if !errors.As(err, &re) {
		t.Fatalf("Roll = %v, want a *RollError", err)
	}
	if re.Member != "eve" || re.Group {
		t.Fatalf("RollError names %q (group=%v), want eve", re.Member, re.Group)
	}

	// The roll stopped at eve: david was recreated, the group was never touched
	// and stayed on its working old pod, serving throughout.
	got := h.backend.recreations()
	if len(got) != 2 || got[0] != "kenward-member-david" || got[1] != "kenward-member-eve" {
		t.Fatalf("recreations = %v, want david then eve and nothing more", got)
	}
	if n := h.backend.recreated("kenward-group"); n != 0 {
		t.Fatalf("group pod recreated %d times after an earlier failure, want 0", n)
	}
	hs := mustHealth(t, h.sup)
	if hs["group"].State != StateReady || hs["group"].Restarts != 0 {
		t.Fatalf("group after failed roll = %+v, want untouched and ready", hs["group"])
	}
	if hs["david"].State != StateReady {
		t.Fatalf("david after roll = %v, want ready on the new image", hs["david"].State)
	}
	// Eve's monitor keeps trying to bring her back, so her state oscillates;
	// what must hold is that the failure is recorded and counted.
	if hs["eve"].Err == nil || hs["eve"].Restarts == 0 {
		t.Fatalf("eve after failed roll = %+v, want the failure retained and counted", hs["eve"])
	}

	h.stop(t)

	// No goroutine leaks on the failure path.
	waitFor(t, "goroutines to drain", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}

// fakeBackend must satisfy the full Backend contract, Recreate and Purge
// included, so these tests exercise the same interface production runs on.
var _ sandbox.Backend = (*fakeBackend)(nil)

func TestIsolatedRollRequiresStart(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	if err := h.sup.Roll(context.Background()); err == nil {
		t.Fatal("Roll before Start succeeded; it must require a started supervisor")
	}
}

// TestIsolatedRollsOnStartWhenTheImageChanged walks the three starts a household
// actually experiences: a first one with no pods, one after the host self-updated,
// and one that changed nothing.
//
// The middle one is the whole point. ensureRunning starts a pod that exists and
// creates one that does not; it never replaces a running container. So without a
// roll at startup the host upgrades itself, comes back on the new binary, finds
// every member's pod running and leaves them on the previous image indefinitely.
func TestIsolatedRollsOnStartWhenTheImageChanged(t *testing.T) {
	record := filepath.Join(t.TempDir(), "pod-image")
	backend := newFakeBackend()
	names := []string{"kenward-member-david", "kenward-member-eve", "kenward-member-ana", "kenward-group"}
	onBackend := func(image string) func(*IsolatedOptions) {
		return func(o *IsolatedOptions) {
			o.Backend = backend
			o.ImageStatePath = record
			o.RollTimeout = time.Second
			if image != "" {
				o.Image = image
			}
		}
	}

	// First start. No pod exists, so there is no old image to roll off and no
	// recreation happens — a fresh household must not serialise its first boot
	// behind a health wait per member for nothing.
	first := newIsolatedHarness(t, onBackend(""))
	first.backend = backend
	first.start(t)
	first.waitAllReady(t)
	if got := backend.recreations(); len(got) != 0 {
		t.Fatalf("first start recreated %v; no pod existed to roll", got)
	}
	first.stop(t)
	if got := readImageRecord(t, record); got != "example.test/kenward:dev" {
		t.Fatalf("recorded image after the first start = %q, want the image the pods were created on", got)
	}

	volumes := make(map[string]int, len(names))
	for _, name := range names {
		id, ok := backend.volumeID(name)
		if !ok {
			t.Fatalf("pod %s has no work volume", name)
		}
		volumes[name] = id
	}

	// The host self-updated and came back on a new binary, so the image it starts
	// pods from is new and every pod is a version behind.
	second := newIsolatedHarness(t, onBackend("example.test/kenward:v2"))
	second.backend = backend
	second.start(t)
	// The record is written only after the last pod came back healthy, so waiting
	// on it waits for the whole roll rather than for the last Recreate call.
	waitFor(t, "the household to roll onto the new image", func() bool {
		b, err := os.ReadFile(record)
		return err == nil && strings.TrimSpace(string(b)) == "example.test/kenward:v2"
	})

	// Members in configuration order, the group last — Roll's order, unchanged.
	got := backend.recreations()
	for i, want := range names {
		if got[i] != want {
			t.Fatalf("roll order = %v, want %v", got, names)
		}
	}
	for _, name := range names {
		spec, ok := backend.spec(name)
		if !ok {
			t.Fatalf("pod %s vanished", name)
		}
		if spec.Image != "example.test/kenward:v2" {
			t.Fatalf("pod %s is still on %q after the host updated", name, spec.Image)
		}
		// The work volume is the member's lore. Recreate preserves it; Purge is
		// the only call that would not, and nothing on this path can reach it.
		if id, _ := backend.volumeID(name); id != volumes[name] {
			t.Fatalf("pod %s work volume changed from %d to %d: the member's lore was lost", name, volumes[name], id)
		}
	}
	if n := backend.destroyed(); n != 0 {
		t.Fatalf("Purge called %d times during a rolling update, want never", n)
	}
	if got := readImageRecord(t, record); got != "example.test/kenward:v2" {
		t.Fatalf("recorded image after the roll = %q, want the image the pods now run", got)
	}
	second.waitAllReady(t)
	second.stop(t)

	// Third start, nothing changed. A restart for any other reason — a crash, a
	// configuration reload, an operator — must not churn every member's pod.
	third := newIsolatedHarness(t, onBackend("example.test/kenward:v2"))
	third.backend = backend
	third.start(t)
	third.waitAllReady(t)
	if n := len(backend.recreations()); n != len(names) {
		t.Fatalf("%d recreations after a start that changed nothing, want the %d from the roll", n, len(names))
	}
	third.stop(t)
}

// TestIsolatedNeverRollsWithoutAnImageRecord proves the check is off, not
// defaulted on, when no path is given: an embedder with nowhere to write must get
// the old behaviour rather than a roll on every start.
func TestIsolatedNeverRollsWithoutAnImageRecord(t *testing.T) {
	backend := newFakeBackend()
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.Backend = backend })
	h.backend = backend
	h.start(t)
	h.waitAllReady(t)
	h.stop(t)

	next := newIsolatedHarness(t, func(o *IsolatedOptions) {
		o.Backend = backend
		o.Image = "example.test/kenward:v2"
	})
	next.backend = backend
	next.start(t)
	next.waitAllReady(t)
	if got := backend.recreations(); len(got) != 0 {
		t.Fatalf("recreated %v with no ImageStatePath configured, want nothing", got)
	}
	next.stop(t)
}

func readImageRecord(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the recorded pod image: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestIsolatedPodConfigProvisioning(t *testing.T) {
	cfgPath := t.TempDir() + "/kenward.yaml"
	yaml := []byte("mode: isolated\n")
	if err := os.WriteFile(cfgPath, yaml, 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	backend := newFakeBackend()
	opts := isolatedTestOptions(backend)
	opts.ConfigFile = cfgPath
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}

	// Every pod gets the compose-identical argv and the configuration at the
	// compose-identical path, so both deployment shapes run one contract.
	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		wantFlag := "--member=" + string(p.key.member)
		if p.key.group {
			wantFlag = "--group"
		}
		// Starting with the subcommand, because the image's ENTRYPOINT is the
		// bare binary: a list that starts with a flag replaces `run` instead of
		// following it, and the pod exits 2 with `kenward: unknown command
		// "--config=…"`. See PodCommand.
		want := PodCommand(wantFlag)
		if len(spec.Command) != len(want) {
			t.Fatalf("pod %s command = %v, want %v", p.name, spec.Command, want)
		}
		for i := range want {
			if spec.Command[i] != want[i] {
				t.Fatalf("pod %s command = %v, want %v", p.name, spec.Command, want)
			}
		}
		if len(spec.Files) != 1 || spec.Files[0].Path != PodConfigPath || string(spec.Files[0].Data) != string(yaml) {
			t.Fatalf("pod %s files = %+v, want the configuration at %s", p.name, spec.Files, PodConfigPath)
		}
	}
}

func TestIsolatedPodTokenFromFileAndCredential(t *testing.T) {
	// david's secrets come from files, eve's from systemd credentials; no
	// environment variable exists anywhere. Each pod receives both of them — its
	// bot token and the passphrase that unwraps its own member's key — in the
	// shape its own resolver will look for: the file at the same path, 0600; the
	// credential as a file under a synthetic CREDENTIALS_DIRECTORY.
	cfg := &config.Config{
		Mode: config.ModeIsolated,
		Household: config.HouseholdConfig{
			Name: "Home", SharedSpace: "household", Tiers: []string{"local"},
		},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", TelegramID: 1, PrivateSpace: "d",
				Tiers: []string{"local"}, BotTokenFile: "/etc/kenward/tokens/david",
				PassphraseFile: "/etc/kenward/pass/david"},
			{ID: "eve", Name: "Eve", TelegramID: 2, PrivateSpace: "e",
				Tiers: []string{"local"}},
		},
	}
	secrets := config.NewSecrets(config.SecretOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		FS: fakeSecretFS{
			"/etc/kenward/tokens/david": "tok-david-file",
			"/etc/kenward/pass/david":   "pass-david-file",
			"/creds/bot_token.eve":      "tok-eve-cred",
			"/creds/passphrase.eve":     "pass-eve-cred",
		},
		CredentialsDir: "/creds",
	})
	opts := isolatedTestOptions(newFakeBackend())
	opts.Secrets = secrets
	sup, err := newIsolated(cfg, opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}

	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		switch string(p.key.member) {
		case "david":
			assertProvisioned(t, spec, "/etc/kenward/tokens/david", "tok-david-file")
			assertProvisioned(t, spec, "/etc/kenward/pass/david", "pass-david-file")
			for k := range spec.Env {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "token") || strings.Contains(lower, "pass") {
					t.Fatalf("david's secret leaked into the environment as %s", k)
				}
			}
		case "eve":
			if spec.Env["CREDENTIALS_DIRECTORY"] != podCredentialsDir {
				t.Fatalf("eve's pod has no synthetic credentials directory: %v", spec.Env)
			}
			assertProvisioned(t, spec, podCredentialsDir+"/bot_token.eve", "tok-eve-cred")
			assertProvisioned(t, spec, podCredentialsDir+"/passphrase.eve", "pass-eve-cred")
		}
	}
}

// TestIsolatedGivesEachPodItsOwnPassphraseAndTheGroupNone is the third defect the
// first real-podman run of isolated mode found, and the one that blocked the mode
// outright: nothing provisioned a passphrase, so every member's pod exited at startup
// with "no session passphrase available, so no member's key can be unwrapped".
//
// A pod has no terminal to be asked at, and the host set neither of the two mechanisms
// `readPassphrase` accepts. The passphrase is now named per member in the household
// configuration and provisioned exactly the way the bot token is — which makes the
// assertions here the shape of the mode itself: each member's pod holds its own
// passphrase, no member's pod holds another's, and the group's pod holds none at all,
// because it serves the shared space and there is no key there to unwrap.
func TestIsolatedGivesEachPodItsOwnPassphraseAndTheGroupNone(t *testing.T) {
	sup, err := newIsolated(isolatedTestConfig(), isolatedTestOptions(newFakeBackend()), "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}

	seen := 0
	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		seen++
		if p.key.group {
			for k := range spec.Env {
				if strings.HasPrefix(k, "PASS_") {
					t.Errorf("the group's pod was given %s; it holds no member key, so a passphrase there unwraps nothing and is one more thing to lose", k)
				}
			}
			continue
		}

		own := "PASS_" + strings.ToUpper(string(p.key.member))
		if got := spec.Env[own]; got != testLookupEnvValue(own) {
			t.Errorf("%s's pod does not carry its own passphrase in %s (env %v)", p.key.member, own, spec.Env)
		}
		for k := range spec.Env {
			if strings.HasPrefix(k, "PASS_") && k != own {
				t.Errorf("%s's pod carries %s, which wraps another member's key; that is the whole of what isolated mode sells", p.key.member, k)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("inspected %d pods, want three members and the group", seen)
	}
}

// TestIsolatedStartsAPodForAMemberWhoHasNotClaimed.
//
// D-023: in isolated mode a member's bot exists BEFORE they claim. The operator adds
// them to the configuration, starts their pod, and hands over a claim code, which the
// member redeems in a conversation with their own bot — never the household's, because
// the onboarding messages are where the privacy model is explained and explaining it
// over the one channel it does not apply to would be a poor start.
//
// The pod's inside already did its half: `kenward run --member=jordan` for an
// unenrolled member comes up claim-only. The host would not create that pod. It said
//
//	level=INFO msg="supervisor: member not enrolled, no pod" member=jordan
//
// and made none, so the sequence D-023 settles was reachable through the compose path
// — which starts a service per member regardless — and through a hand-run process, and
// never through `kenward run`. The one deployment path that manages itself was the one
// that could not onboard anybody.
//
// The pod is identical to an enrolled member's, deliberately: same argv, same
// configuration, its own token and its own passphrase. Which of the two states it is in
// is the pod's own process's business, decided from the configuration it reads.
func TestIsolatedStartsAPodForAMemberWhoHasNotClaimed(t *testing.T) {
	cfgPath := t.TempDir() + "/kenward.yaml"
	if err := os.WriteFile(cfgPath, []byte("mode: isolated\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	h := newIsolatedHarness(t, func(o *IsolatedOptions) { o.ConfigFile = cfgPath })
	h.start(t)

	waitFor(t, "the unenrolled member's pod is up", func() bool {
		return mustHealth(t, h.sup)["ana"].State == StateNotEnrolled
	})

	spec, ok := h.backend.spec("kenward-member-ana")
	if !ok {
		t.Fatal("no pod for a member who has not claimed; D-023's onboarding cannot happen")
	}
	if spec.Env[EnvMember] != "ana" {
		t.Errorf("pod env %v does not name the member it serves", spec.Env)
	}
	if got, want := spec.Command, PodCommand("--member=ana"); !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v — a claim-only pod is started exactly like any other", got, want)
	}
	if spec.Env["TOK_ANA"] != testLookupEnvValue("TOK_ANA") {
		t.Error("the pod has no bot token, so there is no bot for the member to claim against")
	}
	if spec.Env["PASS_ANA"] != testLookupEnvValue("PASS_ANA") {
		t.Error("the pod has no passphrase, so the claim could not provision the member's key")
	}
	for k := range spec.Env {
		if (strings.HasPrefix(k, "TOK_") || strings.HasPrefix(k, "PASS_")) &&
			k != "TOK_ANA" && k != "PASS_ANA" {
			t.Errorf("the pod carries %s, which belongs to another unit", k)
		}
	}

	// "awaiting enrolment" is what a RUNNING claim-only pod reports. A claim-only
	// pod that will not start is a failure like any other, and the two must be
	// distinguishable: before this, both a pod that could not start and a pod that
	// was never created reported StateNotEnrolled with a nil error, so an operator
	// waiting on somebody to accept an invitation and an operator with a broken
	// container saw the same line.
	if u := mustHealth(t, h.sup)["ana"]; u.Err != nil || u.Restarts != 0 {
		t.Fatalf("a healthy claim-only pod = %+v, want no error and no restarts", u)
	}
	h.backend.kill("kenward-member-ana")
	waitFor(t, "the claim-only pod's failure is counted", func() bool {
		u := mustHealth(t, h.sup)["ana"]
		return u.Restarts == 1 && u.Err != nil && u.State == StateNotEnrolled
	})

	h.stop(t)
}

// testLookupEnvValue is what testLookupEnv answers for a name.
func testLookupEnvValue(name string) string {
	v, _ := testLookupEnv(name)
	return v
}

// assertProvisioned requires exactly one 0600 file at path holding want. Mode is
// asserted because a secret provisioned world-readable inside the pod is the same
// finding config.Secrets refuses on the host.
func assertProvisioned(t *testing.T, spec sandbox.Spec, path, want string) {
	t.Helper()
	found := 0
	for _, f := range spec.Files {
		if f.Path != path {
			continue
		}
		found++
		if string(f.Data) != want {
			t.Errorf("%s holds the wrong value", path)
		}
		if f.Mode != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", path, f.Mode)
		}
		// Owned by the identity the image runs as. Root-owned and 0600 is a file
		// the pod's own non-root process cannot open, which broke the file and
		// credential sources in this deployment path entirely — see podSecretUID.
		if f.UID != podSecretUID || f.GID != podSecretGID {
			t.Errorf("%s is owned by %d:%d, want the pod's own %d:%d — root-owned and 0600 is unreadable inside the pod",
				path, f.UID, f.GID, podSecretUID, podSecretGID)
		}
	}
	if found != 1 {
		t.Errorf("%s provisioned %d times, want once (files: %+v)", path, found, spec.Files)
	}
}

// TestIsolatedRefusesCollidingPodNames.
//
// Pod names are sanitised into the alphabet sandbox.Spec.Name allows, which is
// lossy: the distinct member ids "ana.smith" and "ana-smith" produce one name.
// Configuration rejects empty and exactly-duplicate ids and nothing else, so
// both reach the supervisor. If both pods were built, the second member's
// monitor would inspect a pod that is already running — the first member's —
// find it up, and report its own member ready on it. That member's turns would
// then be served by another member's pod, with that member's lore volume and
// that member's bot token. The supervisor must refuse instead, whatever the
// layer above did or did not check.
func TestIsolatedRefusesCollidingPodNames(t *testing.T) {
	// The premise: two ids, one name.
	if a, b := podName("kenward", "member-ana.smith"), podName("kenward", "member-ana-smith"); a != b {
		t.Fatalf("sanitisation no longer collides these ids (%q vs %q); rewrite this test around ones that do", a, b)
	}

	cfg := isolatedTestConfig()
	cfg.Members = []config.MemberConfig{
		{ID: "ana.smith", Name: "Ana", TelegramID: 1, PrivateSpace: "a", Tiers: []string{"local"}, BotTokenEnv: "TOK_A", PassphraseEnv: "PASS_A"},
		{ID: "ana-smith", Name: "Anna", TelegramID: 2, PrivateSpace: "b", Tiers: []string{"local"}, BotTokenEnv: "TOK_B", PassphraseEnv: "PASS_B"},
	}
	backend := newFakeBackend()

	sup, err := newIsolated(cfg, isolatedTestOptions(backend), "linux")
	if err == nil {
		_ = sup.Stop(context.Background())
		t.Fatal("newIsolated accepted two members whose pod names collide; the second would be served by the first's pod")
	}
	for _, want := range []string{"ana.smith", "ana-smith", "kenward-member-ana-smith"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if sup != nil {
		t.Fatal("newIsolated returned a supervisor alongside the refusal")
	}
	// Refused before anything existed: no pod was created, so none could be
	// adopted.
	if n := len(backend.created); n != 0 {
		t.Fatalf("refusal created %d pods, want none", n)
	}
}

// TestIsolatedReadyMeansTheContainerIsRunning pins what StateReady claims in
// this mode, because the honest answer is weaker than the word suggests: the
// backend reports whether the container process is alive and nothing finer, so
// a pod whose image starts and whose unit then wedges is reported ready and the
// supervisor cannot tell the difference. Nothing here probes the unit — no
// exec, no readiness call — and if that ever changes, this test and the
// documentation on StateReady, Healthy and runPod change together.
func TestIsolatedReadyMeansTheContainerIsRunning(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.start(t)
	h.waitAllReady(t)

	for name, u := range mustHealth(t, h.sup) {
		if name == "ana" {
			continue // has a pod, but is awaiting enrolment rather than ready
		}
		if !u.Healthy() {
			t.Fatalf("%s = %v, want ready", name, u.State)
		}
	}

	// The only questions asked of the backend were lifecycle ones. A readiness
	// probe would show up here as something other than create/start/stop.
	h.backend.mu.Lock()
	events := append([]string(nil), h.backend.events...)
	h.backend.mu.Unlock()
	for _, e := range events {
		switch verb, _, _ := strings.Cut(e, " "); verb {
		case "create", "start", "stop", "recreate":
		default:
			t.Fatalf("unexpected backend call %q: readiness is documented as container liveness only", e)
		}
	}

	h.stop(t)
}

// TestIsolatedRunPodPanicDoesNotCrashProcess proves isolated mode's own promise
// about itself: a panic inside runPod's monitor loop — not a turn, not
// anything a member did, the goroutine that watches the container — must not
// reach the process. If recoverPump were missing here, this goroutine's panic
// would crash the whole test binary rather than fail one assertion, so this
// test passing at all (rather than the process dying) is part of the proof;
// the health assertions below check that the failure was reported rather than
// swallowed silently.
func TestIsolatedRunPodPanicDoesNotCrashProcess(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.start(t)
	h.waitAllReady(t)

	h.backend.setPanicOnInspect("kenward-member-eve")

	waitFor(t, "eve's monitor to panic and report failed", func() bool {
		return mustHealth(t, h.sup)["eve"].State == StateStopped
	})
	hs := mustHealth(t, h.sup)
	if hs["eve"].Err == nil {
		t.Fatal("eve's panic was not retained as a failure")
	}

	// The rest of the household is unaffected: david and the group keep serving
	// while eve's monitor goroutine is gone.
	if hs["david"].State != StateReady || hs["group"].State != StateReady {
		t.Fatalf("david=%v group=%v after eve's monitor panicked, want both still ready", hs["david"].State, hs["group"].State)
	}

	h.stop(t)
}

// TestIsolatedShutdownStopPanicDoesNotCrashProcess is the sibling case: a panic
// in the per-pod goroutine Stop launches during shutdown must not take the
// process down either, and Stop must still return once every other pod's
// graceful stop has completed.
func TestIsolatedShutdownStopPanicDoesNotCrashProcess(t *testing.T) {
	h := newIsolatedHarness(t, nil)
	h.start(t)
	h.waitAllReady(t)

	h.backend.setPanicOnStop("kenward-member-eve")

	// h.stop asserts Stop returns without error and Start unwinds cleanly; if
	// the panic reached the process this call would never return.
	h.stop(t)

	if _, _, stops := h.backend.counts("kenward-member-david"); stops != 1 {
		t.Fatal("david was not gracefully stopped alongside eve's panicking stop")
	}
	if _, _, stops := h.backend.counts("kenward-group"); stops != 1 {
		t.Fatal("the group pod was not gracefully stopped alongside eve's panicking stop")
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

// TestIsolatedProvisionsEachMemberTheirOwnInvitesAndTheGroupNone is D-023's last mile,
// and the gap it closes was found by running the mode against real podman.
//
// `kenward invite` mints on the host, into the host's own invite store. The claim is
// redeemed inside the member's pod, against the invite store on that pod's own volume,
// and nothing crossed between the two: the operator handed over a code the pod had
// never heard of, the claimer refused it, and — correctly, because enrolment owes a
// stranger silence — said nothing to anybody. The whole of D-023 ended one step short.
//
// The seed is what crosses, and what it is matters as much as that it exists. It is
// this member's file and no other member's; the records in it are PBKDF2 digests, so
// nothing redeemable travels; and it goes into the container's own filesystem rather
// than the pod's work volume, which the host neither reads nor writes.
func TestIsolatedProvisionsEachMemberTheirOwnInvitesAndTheGroupNone(t *testing.T) {
	dir := t.TempDir()
	// One file per member, as `kenward invite` writes them. eve has none: not every
	// member has an invite outstanding, and that is not a failure.
	for name, body := range map[string]string{
		"david.json": `{"version":1,"codes":[{"hash":"david-digest"}]}`,
		"ana.json":   `{"version":1,"codes":[{"hash":"ana-digest"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing seed: %v", err)
		}
	}

	opts := isolatedTestOptions(newFakeBackend())
	opts.InviteSeedDir = dir
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}

	seen := 0
	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		seen++
		var got []string
		for _, f := range spec.Files {
			if f.Path == PodInvitesPath {
				got = append(got, string(f.Data))
			}
		}
		switch string(p.key.member) {
		case "david":
			assertProvisioned(t, spec, PodInvitesPath, `{"version":1,"codes":[{"hash":"david-digest"}]}`)
			// And nobody else's. The host's own store holds every member's digests;
			// a member's pod may hold exactly its own.
			for _, body := range got {
				if strings.Contains(body, "ana-digest") {
					t.Errorf("david's pod was handed ana's claim codes: %s", body)
				}
			}
		case "eve":
			if len(got) != 0 {
				t.Errorf("eve has no invite outstanding but her pod was given %v", got)
			}
		case "ana":
			assertProvisioned(t, spec, PodInvitesPath, `{"version":1,"codes":[{"hash":"ana-digest"}]}`)
		default:
			// The household group. It has no claimer — D-023 puts the claim
			// conversation on the member's own bot — so a file of codes there would
			// be one more thing in the one pod every member talks to.
			if len(got) != 0 {
				t.Errorf("the group's pod was given claim codes: %v", got)
			}
			if slices.Contains(spec.Command, "--invites="+PodInvitesPath) {
				t.Errorf("the group's pod is told to import invites it has no claimer for: %v", spec.Command)
			}
		}
		// Nothing on the pod's own volume is named, written or read on this path.
		for _, f := range spec.Files {
			if strings.HasPrefix(f.Path, "/work") {
				t.Errorf("pod %s: the host provisioned %s, which is the member's own volume", p.name, f.Path)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("inspected %d pods, want three members and the group", seen)
	}
}

// TestIsolatedMemberPodIsToldWhereItsInvitesAre pins the argv half. Provisioning the
// file is useless if the pod is never told to read it, and the two are decided in
// different functions.
func TestIsolatedMemberPodIsToldWhereItsInvitesAre(t *testing.T) {
	want := "--invites=" + PodInvitesPath
	if got := PodCommand("--member=david"); !slices.Contains(got, want) {
		t.Errorf("a member's pod command %v does not carry %s", got, want)
	}
	if got := PodCommand(PodGroupFlag); slices.Contains(got, want) {
		t.Errorf("the group's pod command %v carries %s and has no claimer to use it", got, want)
	}
}

// TestIsolatedRecreatesOnlyThePodsWhoseProvisionedFilesMayBeStale.
//
// `kenward invite` and `kenward revoke` both tell the operator to restart kenward, and
// a restart on its own delivers neither: ensureRunning starts a container that already
// exists, so it comes back on the `/etc/kenward` it was built with. Against real podman
// that meant an operator could revoke a member, restart exactly as instructed, and watch
// the pod come up still serving the revoked account:
//
//	--- record inside the pod? ---
//	>>> NOT IN THE POD <<<
//	... enrolled=true
//
// So Start replaces the pods that may be holding stale copies, and only those: a
// revoked member's, and an unclaimed member's with a code outstanding. A pod serving
// somebody is started, not replaced.
func TestIsolatedRecreatesOnlyThePodsWhoseProvisionedFilesMayBeStale(t *testing.T) {
	revDir, seedDir := t.TempDir(), t.TempDir()
	// david was revoked. ana has a code outstanding and has not claimed. eve has a
	// seed too but has claimed, so her pod is serving somebody.
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(revDir, "david.json", `{"member_id":"david","revoked_at":"2026-08-15T12:00:00Z"}`)
	write(seedDir, "ana.json", `{"version":1,"codes":[]}`)
	write(seedDir, "eve.json", `{"version":1,"codes":[]}`)

	b := newFakeBackend()
	opts := isolatedTestOptions(b)
	opts.RevocationDir, opts.InviteSeedDir = revDir, seedDir
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}

	// Every pod already exists, which is what a restart finds.
	vols := make(map[string]int, len(sup.pods))
	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		if _, err := b.Create(context.Background(), spec); err != nil {
			t.Fatalf("create %s: %v", p.name, err)
		}
		id, _ := b.volumeID(p.name)
		vols[p.name] = id
	}

	sup.recreateStalePods(context.Background())

	for _, tc := range []struct {
		pod  string
		want int
		why  string
	}{
		{podName(DefaultNamePrefix, "member-david"), 1, "david was revoked and his pod would otherwise never see the record"},
		{podName(DefaultNamePrefix, "member-ana"), 1, "ana has a code outstanding and her pod is serving nobody"},
		{podName(DefaultNamePrefix, "member-eve"), 0, "eve has claimed; her pod is serving her and must not be replaced"},
		{podName(DefaultNamePrefix, "group"), 0, "the group's pod is given neither file"},
	} {
		if got := b.recreated(tc.pod); got != tc.want {
			t.Errorf("%s recreated %d times, want %d — %s", tc.pod, got, tc.want, tc.why)
		}
	}
	// And no member's lore was taken with it. Recreate preserves the work volume;
	// nothing on this path may reach Purge.
	for name, want := range vols {
		if got, ok := b.volumeID(name); !ok || got != want {
			t.Errorf("%s's work volume changed from %d to %d (present=%v); the member's lore lives there", name, want, got, ok)
		}
	}
	if b.destroyed() != 0 {
		t.Errorf("%d volumes were destroyed", b.destroyed())
	}
}

// TestIsolatedProvisionsEachMemberTheirOwnRevocationAndTheGroupNone.
//
// The other direction of the same crossing, and the only delivery a revocation has in
// this mode: `kenward revoke` cannot clear a binding that lives on a member's volume,
// so it records the fact and this is what carries it in. One member's record reaches
// that member's pod and nobody else's — a record applied in the wrong pod unbinds
// whoever that pod serves — and the group's pod, which holds no member's binding, gets
// none at all.
func TestIsolatedProvisionsEachMemberTheirOwnRevocationAndTheGroupNone(t *testing.T) {
	dir := t.TempDir()
	// One file per revoked member, as `kenward revoke` writes them. Almost nobody has
	// one, which is why an absent file is not a failure.
	const davidRecord = `{"member_id":"david","revoked_at":"2026-08-15T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "david.json"), []byte(davidRecord), 0o644); err != nil {
		t.Fatalf("writing the record: %v", err)
	}

	opts := isolatedTestOptions(newFakeBackend())
	opts.RevocationDir = dir
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	for _, p := range sup.pods {
		spec, err := sup.specFor(p)
		if err != nil {
			t.Fatalf("specFor(%s): %v", p.name, err)
		}
		var got []string
		for _, f := range spec.Files {
			if f.Path == PodRevokedPath {
				got = append(got, string(f.Data))
			}
		}
		if string(p.key.member) == "david" {
			assertProvisioned(t, spec, PodRevokedPath, davidRecord)
			continue
		}
		if len(got) > 0 {
			t.Errorf("pod %s was handed david's revocation record: %v", p.name, got)
		}
	}
}

// TestIsolatedUnreadableRevocationRefusesTheSpec.
//
// An absent record is no revocation, which is the ordinary case. A record that is
// present and unreadable is the operator having revoked somebody; starting the pod
// anyway serves the account they revoked, and reports the pod healthy doing it.
func TestIsolatedUnreadableRevocationRefusesTheSpec(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "david.json"), 0o700); err != nil {
		t.Fatalf("preparing the record: %v", err)
	}
	opts := isolatedTestOptions(newFakeBackend())
	opts.RevocationDir = dir
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	for _, p := range sup.pods {
		if string(p.key.member) != "david" {
			continue
		}
		if _, err := sup.specFor(p); err == nil {
			t.Fatal("specFor accepted an unreadable revocation; david's pod would start and serve the revoked account")
		}
	}
}

// TestIsolatedUnreadableInvitesRefuseTheSpec.
//
// A seed that is absent means no invite is outstanding, which is the ordinary case. A
// seed that is present and unreadable means the operator minted a code that will not
// arrive, and starting the pod anyway produces the one enrolment failure nobody can
// diagnose: the member presents a real code to their own bot and is answered with the
// silence a stranger gets.
func TestIsolatedUnreadableInvitesRefuseTheSpec(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be. It is what a container runtime creates
	// when asked to bind-mount a source that does not exist, and it is unreadable in
	// exactly the way that matters.
	if err := os.MkdirAll(filepath.Join(dir, "david.json"), 0o700); err != nil {
		t.Fatalf("preparing the seed: %v", err)
	}
	opts := isolatedTestOptions(newFakeBackend())
	opts.InviteSeedDir = dir
	sup, err := newIsolated(isolatedTestConfig(), opts, "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	for _, p := range sup.pods {
		if string(p.key.member) != "david" {
			continue
		}
		if _, err := sup.specFor(p); err == nil {
			t.Fatal("specFor accepted an unreadable seed; david's pod would start and refuse his code in silence")
		}
	}
}
