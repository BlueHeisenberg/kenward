//go:build integration && linux

package main

// The only test in this repository that starts real containers.
//
// Isolated mode had twelve separate fatal defects found in one day, and every one
// of them lived in the seam between internal/supervisor — which tests the capability
// against a fake container backend — and cmd/kenward, which wires it. A fake backend
// has no container layer to go stale, no named volume to preserve, no argv the image's
// ENTRYPOINT can reject and no process environment a sibling's token can leak into, so
// every one of those defects was invisible to a suite in which every library test
// passed. They were found by driving real podman by hand. This file is that, automated.
//
// # What is real here and what is not
//
// Real: podman, the image built from this repository's own Dockerfile, a real `lore`
// binary answering a real MCP handshake inside each pod, the pods' named volumes, the
// host's `kenward invite` and `kenward revoke` through the real dispatcher, and
// cmd/kenward's own isolatedOptions — so the wiring under test is the wiring `kenward
// run` uses, not a second copy written for the test.
//
// Two things are not. supervisor.NewIsolated is called here rather than through
// defaultSupervisor, because the options need a Backend that records what was asked of
// podman (see recordingBackend); the switch statement that would have chosen it is one
// line and supervisors_test.go covers it. And Telegram: no bot token exists, so every
// pod reaches `getMe`, is refused, and exits.
//
// # Why a pod exiting at getMe is the assertion rather than the problem
//
// A pod's terminal state here is:
//
//	kenward: supervisor: building telegram transport: transport: telegram: error call getMe, unauthorized, Unauthorized
//
// measured at about 170ms after start. Nothing in this environment can make that call
// succeed — the bot library's server URL is not reachable from configuration — so no
// assertion here may depend on a pod staying up.
//
// That costs less than it sounds like, because everything this file exists to check
// happens *before* that call and leaves evidence that outlives the process. By the time
// getMe runs, the pod has read the configuration provisioned into it, resolved its own
// two secrets, answered a real lore handshake, applied any revocation the host recorded,
// written its member's wrapped key to its work volume and imported its outstanding
// claim codes into its own store. So the assertions are on the container, the volume,
// the argv, the environment and the disk — and reaching the Telegram refusal is itself
// asserted, because a pod that died earlier died of something this file is looking for.
// Every one of the twelve defects would fail one of these before Telegram was reached.
//
// # Running it
//
//	go test -tags integration -run TestIsolatedPodman -timeout 30m -v ./cmd/kenward/
//
// on a Linux host with podman and `go` on PATH, and a checkout of lore beside this one
// (or KENWARD_E2E_LORE_SRC naming it). Everything else — the image, the lore binary, the
// volumes, the stores — is built by the test and destroyed with it. Nothing touches the
// developer's own ~/.lore, podman state outside the `sbx-kwe2e-` name prefix, or any
// household configuration.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/sandbox"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// namePrefixRoot begins every sandbox name this file creates, so cleanup can find
// everything it made and nothing it did not. keel prefixes containers and volumes with
// "sbx-", so the podman objects are all `sbx-kwe2e-…`.
const namePrefixRoot = "kwe2e"

// sandboxPrefix is keel's own default (sandbox.Config.NamePrefix). It is written here
// because the assertions below inspect podman directly, by the name keel gives it.
const sandboxPrefix = "sbx-"

// podUID and podGID are the distroless base's fixed non-root account. Files this test
// seeds onto a work volume have to be owned by it, exactly as the supervisor owns the
// secrets it provisions, or the pod cannot read what it is given.
const podUID, podGID = 65532, 65532

// Timeouts. Container work is slower than anything else in this repository's tests and
// faster than a model, so these are seconds rather than milliseconds or minutes.
const (
	podmanCallTimeout = 2 * time.Minute
	podWaitTimeout    = 2 * time.Minute
	// pollInterval is the supervisor's liveness poll, and it is not only that: the
	// supervisor bounds every backend call at four times it, and one Create is a
	// volume, a container, a file copy and a start. Four of those at once on a
	// developer's machine measured about three and a half seconds each, which a
	// one-second poll turns into
	//
	//	sandbox: start after provisioning failed … start_err="podman start: exit -1"
	//
	// on every pod — the test cancelling podman mid-start and blaming the supervisor.
	pollInterval = 5 * time.Second
	// restartBackoff is long on purpose: every pod here crash-loops on the Telegram
	// refusal, and a short backoff would churn containers throughout the run for no
	// information. One restart is enough to prove the monitor is doing its job.
	restartBackoff = 30 * time.Second
	// rollTimeout bounds supervisor.Roll's wait for a recreated pod to hold running.
	// It cannot succeed here (see the file comment), so it is short: the roll's
	// substance — the Recreate, and the volume surviving it — has already happened by
	// the time this expires.
	rollTimeout = 12 * time.Second
)

var householdSeq atomic.Int64

// -----------------------------------------------------------------------------
// the rig: podman, the image, and a lore binary
// -----------------------------------------------------------------------------

// rig is what every scenario in this file shares: a reachable podman, one image built
// from this repository's Dockerfile with a real lore added to it, a second image that
// differs from the first so a rolling update has somewhere to roll, and the same lore
// binary runnable on the host so a work volume can be initialised and read back.
type rig struct {
	podmanBin string
	// image is the derived image pods run. rolled is a genuinely different image —
	// a different id, not merely a second tag — so supervisor.Roll is exercised
	// against a real image change rather than a renamed one.
	image, rolled string
	loreBin       string
	repo          string
	// buildEnv carries CONTAINERS_REGISTRIES_CONF for the build only. The Dockerfile's
	// builder stage names `golang:1.25.0-bookworm` unqualified, which podman refuses
	// unless a search registry is configured; writing one into a scratch file keeps
	// this test off the machine's own /etc/containers configuration.
	buildEnv []string
}

func newRig(t *testing.T) *rig {
	t.Helper()

	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman is not on PATH; isolated mode cannot be exercised without a container runtime")
	}
	if out, err := exec.Command(podman, "info", "--format", "{{.Host.Arch}}").CombinedOutput(); err != nil {
		t.Skipf("podman is installed but not usable here (%v):\n%s", err, out)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; the test builds a lore binary for the pod image and cannot without it")
	}

	repo := repoRoot(t)
	loreSrc, ok := findLoreCheckout(repo)
	if !ok {
		t.Skipf("no lore checkout beside %s (set KENWARD_E2E_LORE_SRC to one): the published image deliberately carries "+
			"no lore, and `kenward run` refuses to serve a unit whose memory layer does not answer, so a pod without one "+
			"cannot start", repo)
	}

	scratch := t.TempDir()
	regConf := filepath.Join(scratch, "registries.conf")
	writeFile(t, regConf, []byte("unqualified-search-registries = [\"docker.io\"]\n"), 0o644)

	r := &rig{
		podmanBin: podman,
		repo:      repo,
		buildEnv:  append(os.Environ(), "CONTAINERS_REGISTRIES_CONF="+regConf),
	}

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	base := "localhost/kenward-e2e-base:" + tag
	r.image = "localhost/kenward-e2e:" + tag
	r.rolled = "localhost/kenward-e2e:" + tag + "-rolled"

	// Everything this run made, gone again — including on failure, which is when it
	// matters. The name prefix is the whole reach: no object outside `sbx-kwe2e-` and
	// these three image tags is touched.
	t.Cleanup(func() { r.purgeEverything(t) })
	t.Cleanup(func() {
		for _, ref := range []string{r.rolled, r.image, base} {
			_, _ = r.try(t, "rmi", "-f", ref)
		}
	})

	r.loreBin = buildLore(t, scratch, loreSrc)

	// The real Dockerfile's final stage, built here rather than pulled, so a change to
	// it is a change to what this test runs. The builder stage inside it is the
	// golang:1.25-bookworm / GOWORK=off / CGO_ENABLED=0 build the image needs.
	t.Logf("building %s from %s/Dockerfile", base, repo)
	r.build(t, repo, []string{"--target", "final", "-t", base, "-f", filepath.Join(repo, "Dockerfile"), repo})

	// The supported way to give a supervisor-started pod a memory layer: a derived
	// image. keel's sandbox.Spec has no bind-mount, so the compose file's route is
	// closed here and this is the only one.
	derived := filepath.Join(scratch, "derived")
	mkdirAll(t, derived)
	copyFile(t, r.loreBin, filepath.Join(derived, "lore"), 0o755)
	writeFile(t, filepath.Join(derived, "Containerfile"),
		[]byte("FROM "+base+"\nCOPY lore /usr/local/bin/lore\n"), 0o644)
	r.build(t, derived, []string{"-t", r.image, "-f", filepath.Join(derived, "Containerfile"), derived})

	// And a second image that is not the first. A retag would leave the image id
	// unchanged, and then "the pod is on the new image" could not be told from "the
	// pod was never replaced".
	writeFile(t, filepath.Join(derived, "Containerfile.rolled"),
		[]byte("FROM "+r.image+"\nLABEL kenward.e2e.roll=\"second build\"\n"), 0o644)
	r.build(t, derived, []string{"-t", r.rolled, "-f", filepath.Join(derived, "Containerfile.rolled"), derived})

	return r
}

func (r *rig) build(t *testing.T, dir string, args []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.podmanBin, append([]string{"build"}, args...)...)
	cmd.Dir = dir
	cmd.Env = r.buildEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("podman build %v: %v\n%s", args, err, out)
	}
}

// podman runs one podman command and fails the test if it does not succeed.
func (r *rig) podman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := r.try(t, args...)
	if err != nil {
		t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (r *rig) try(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), podmanCallTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.podmanBin, args...).CombinedOutput()
	return string(out), err
}

// purgeEverything removes every container and volume this file could have made. It
// filters on the name prefix rather than on a list kept in memory, so a pod created by
// a scenario that then panicked is still cleaned up.
func (r *rig) purgeEverything(t *testing.T) {
	t.Helper()
	filter := "name=^" + sandboxPrefix + namePrefixRoot
	if out, err := r.try(t, "ps", "-aq", "--filter", filter); err == nil {
		for _, id := range strings.Fields(out) {
			_, _ = r.try(t, "rm", "-f", "-t", "1", id)
		}
	}
	if out, err := r.try(t, "volume", "ls", "-q", "--filter", filter); err == nil {
		for _, name := range strings.Fields(out) {
			_, _ = r.try(t, "volume", "rm", "-f", name)
		}
	}
}

// buildLore compiles lore for the image, from a scratch copy of the checkout.
//
// A copy rather than the checkout itself: lore's go.mod may carry a local `replace`
// while somebody is working in it, and this must neither read that state nor write
// anything into a repository it does not own. CGO_ENABLED=0 because the pod image is
// distroless and has no libc; GOWORK=off because a workspace file would pull in
// whatever else the developer has linked.
func buildLore(t *testing.T, scratch, src string) string {
	t.Helper()
	work := filepath.Join(scratch, "lore-src")
	if out, err := exec.Command("cp", "-a", src+"/.", work).CombinedOutput(); err != nil {
		t.Fatalf("copying the lore checkout from %s: %v\n%s", src, err, out)
	}
	if err := os.RemoveAll(filepath.Join(work, ".git")); err != nil {
		t.Fatalf("pruning .git from the lore copy: %v", err)
	}
	out := filepath.Join(scratch, "lore")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/lore")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building lore from %s: %v\n%s", work, err, b)
	}
	return out
}

// findLoreCheckout locates the sibling lore repository the pod image needs a binary
// from. KENWARD_E2E_LORE_SRC wins; otherwise it looks for a `lore` module beside this
// checkout and beside each of its parents, because a git worktree lives several levels
// under the repository it belongs to and its "sibling" is not where the checkout is.
func findLoreCheckout(repo string) (string, bool) {
	if v := os.Getenv("KENWARD_E2E_LORE_SRC"); v != "" {
		_, err := os.Stat(filepath.Join(v, "go.mod"))
		return v, err == nil
	}
	for dir := repo; ; {
		candidate := filepath.Join(filepath.Dir(dir), "lore")
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // cmd/kenward
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	if _, err := os.Stat(filepath.Join(root, "Dockerfile")); err != nil {
		t.Fatalf("no Dockerfile at %s: %v", root, err)
	}
	return root
}

// -----------------------------------------------------------------------------
// recordingBackend
// -----------------------------------------------------------------------------

// recordingBackend is a real podman backend that writes down what it was asked to do.
//
// It replaces nothing: every call is delegated, so every container is a real container.
// What it adds is the one question podman cannot be asked afterwards — whether Purge
// was ever called. Purge is the only method in keel's Backend that deletes a member's
// work volume, and "the volume survived" and "nothing ever tried to delete it" are
// different facts: a Purge followed by a Create leaves a volume in place and a member's
// lore gone.
type recordingBackend struct {
	sandbox.Backend

	mu    sync.Mutex
	calls []backendCall
}

type backendCall struct {
	op   string
	name string
}

func (b *recordingBackend) note(op, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, backendCall{op: op, name: name})
}

func (b *recordingBackend) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	b.note("Create", spec.Name)
	return b.Backend.Create(ctx, spec)
}

func (b *recordingBackend) Recreate(ctx context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	b.note("Recreate", spec.Name)
	return b.Backend.Recreate(ctx, spec)
}

func (b *recordingBackend) Purge(ctx context.Context, id string) error {
	b.note("Purge", id)
	return b.Backend.Purge(ctx, id)
}

func (b *recordingBackend) ops(op, name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		if c.op == op && (name == "" || c.name == name) {
			n++
		}
	}
	return n
}

var _ sandbox.Backend = (*recordingBackend)(nil)

// -----------------------------------------------------------------------------
// a household under test
// -----------------------------------------------------------------------------

// household is one scenario's world: its own configuration, its own host data
// directory, its own pod name prefix, and therefore its own containers and volumes.
type household struct {
	t      *testing.T
	rig    *rig
	h      *harness
	prefix string
}

func newHousehold(t *testing.T, r *rig, yaml string, vars map[string]string) *household {
	t.Helper()
	h := newHarness(t, yaml, vars)
	// The harness freezes the clock for golden output. Here the clock is compared
	// against real container creation times and real file modification times, so it
	// has to be the real one.
	h.e.now = time.Now
	// And the real PATH: this process is about to be the isolated host supervisor,
	// which is the one process that legitimately has no lore of its own.
	h.e.lookPath = exec.LookPath
	h.e.probes = probes{}

	return &household{
		t:      t,
		rig:    r,
		h:      h,
		prefix: fmt.Sprintf("%s-%d", namePrefixRoot, householdSeq.Add(1)),
	}
}

func (hh *household) memberPod(id string) string { return hh.prefix + "-member-" + id }
func (hh *household) groupPod() string           { return hh.prefix + "-group" }

func (hh *household) container(pod string) string { return sandboxPrefix + pod }
func (hh *household) volume(pod string) string    { return sandboxPrefix + pod + "-work" }

func (hh *household) dataDir() string { return filepath.Join(hh.h.dir, "data") }

// cli runs a real kenward subcommand through the real dispatcher, against this
// household's configuration and data directory. It is how a claim code is minted and a
// revocation recorded, so those files are written by the code that writes them in
// production rather than by the test's idea of their shape.
func (hh *household) cli(args ...string) string {
	hh.t.Helper()
	before := hh.h.both()
	if code := hh.h.run(args...); code != exitOK {
		hh.t.Fatalf("kenward %s exited %d:\n%s", strings.Join(args, " "), code, hh.h.both()[len(before):])
	}
	return hh.h.both()[len(before):]
}

// supervisorFor builds the isolated host supervisor exactly as `kenward run` does — the
// options come from cmd/kenward's own isolatedOptions — with the timings shortened and
// the backend wrapped so the test can see what podman was asked for.
func (hh *household) supervisorFor(image string) (*supervisor.Isolated, *recordingBackend) {
	t := hh.t
	t.Helper()

	cfg, err := loadConfig(hh.h.config, resolveDataDir(hh.h.e, ""), hh.h.e.secrets())
	if err != nil {
		t.Fatalf("loading %s: %v\n%s", hh.h.config, err, renderConfigError(hh.h.config, err))
	}
	logger := slog.New(slog.NewTextHandler(podLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	opts, err := isolatedOptions(hh.h.e, runOptions{
		configPath: hh.h.config,
		dataDir:    cfg.DataDir,
		image:      image,
	}, logger)
	if err != nil {
		t.Fatalf("isolatedOptions: %v", err)
	}
	opts.NamePrefix = hh.prefix
	opts.PollInterval = pollInterval
	opts.RestartBackoff = restartBackoff
	opts.MaxRestartBackoff = restartBackoff
	opts.RollTimeout = rollTimeout

	backend := &recordingBackend{Backend: sandbox.NewPodmanBackend(sandbox.Config{}, logger)}
	opts.Backend = backend

	sup, err := supervisor.NewIsolated(cfg, opts)
	if err != nil {
		t.Fatalf("supervisor.NewIsolated: %v", err)
	}
	return sup, backend
}

// start runs the supervisor until until() holds, then drains it. The health sampler
// runs for the whole of it: a pod's reported state is only observable while the
// supervisor is up, and several of the facts below are about what it never reported.
func (hh *household) start(sup *supervisor.Isolated, until func() bool) *healthWatch {
	t := hh.t
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- sup.Start(ctx) }()

	watch := newHealthWatch(sup)
	defer watch.stop()

	waitFor(t, "the scenario's condition", podWaitTimeout, until)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stopCancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Errorf("supervisor Stop: %v", err)
	}
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("supervisor Start: %v", err)
		}
	case <-time.After(time.Minute):
		t.Error("supervisor Start did not return after Stop")
	}
	return watch
}

type podLogWriter struct{ t *testing.T }

func (w podLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// -----------------------------------------------------------------------------
// health sampling
// -----------------------------------------------------------------------------

// healthWatch records every state the supervisor ever reported for every unit.
//
// It samples rather than reads once because a pod here is short-lived: it is created,
// reported, refused by Telegram and gone. The question worth asking is not what the
// state is at one instant but which states a unit was ever in — specifically, that a
// member who has not claimed their invite is never once reported ready, which is an
// assertion about a value the supervisor computes (pod.upState) and would fail even if
// the sampling caught only one moment of it.
type healthWatch struct {
	mu   sync.Mutex
	seen map[string]map[supervisor.State]int
	done chan struct{}
	once sync.Once
}

func newHealthWatch(sup supervisor.Supervisor) *healthWatch {
	w := &healthWatch{seen: map[string]map[supervisor.State]int{}, done: make(chan struct{})}
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-tick.C:
				hs, err := sup.Health(context.Background())
				if err != nil {
					continue
				}
				w.mu.Lock()
				for _, h := range hs {
					key := string(h.Member)
					if h.Group {
						key = "group"
					}
					if w.seen[key] == nil {
						w.seen[key] = map[supervisor.State]int{}
					}
					w.seen[key][h.State]++
				}
				w.mu.Unlock()
			}
		}
	}()
	return w
}

func (w *healthWatch) stop() { w.once.Do(func() { close(w.done) }) }

func (w *healthWatch) states(unit string) map[supervisor.State]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[supervisor.State]int{}
	for k, v := range w.seen[unit] {
		out[k] = v
	}
	return out
}

func (w *healthWatch) mustHaveSeen(t *testing.T, unit string, want supervisor.State) {
	t.Helper()
	if got := w.states(unit); got[want] == 0 {
		t.Errorf("unit %q was never reported %s; states seen: %s", unit, want, renderStates(got))
	}
}

func (w *healthWatch) mustNeverHaveSeen(t *testing.T, unit string, unwanted supervisor.State) {
	t.Helper()
	if got := w.states(unit); got[unwanted] > 0 {
		t.Errorf("unit %q was reported %s %d time(s); states seen: %s",
			unit, unwanted, got[unwanted], renderStates(got))
	}
}

func renderStates(m map[supervisor.State]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	var parts []string
	for s, n := range m {
		parts = append(parts, fmt.Sprintf("%s×%d", s, n))
	}
	return strings.Join(parts, " ")
}

// -----------------------------------------------------------------------------
// looking at podman
// -----------------------------------------------------------------------------

// containerView is the part of `podman inspect` this file asserts on.
type containerView struct {
	ID        string    `json:"Id"`
	Created   time.Time `json:"Created"`
	ImageName string    `json:"ImageName"`
	Config    struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint any      `json:"Entrypoint"`
	} `json:"Config"`
	State struct {
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Mounts []struct {
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	raw string
}

func (hh *household) inspect(pod string) containerView {
	t := hh.t
	t.Helper()
	v, err := hh.tryInspect(pod)
	if err != nil {
		t.Fatalf("inspecting container for pod %s: %v", pod, err)
	}
	return v
}

func (hh *household) tryInspect(pod string) (containerView, error) {
	out, err := hh.rig.try(hh.t, "inspect", "--type", "container", "--format", "{{json .}}", hh.container(pod))
	if err != nil {
		return containerView{}, fmt.Errorf("%w: %s", err, out)
	}
	var v containerView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return containerView{}, fmt.Errorf("decoding podman inspect output: %w", err)
	}
	v.raw = out
	return v, nil
}

func (hh *household) containerExists(pod string) bool {
	_, err := hh.tryInspect(pod)
	return err == nil
}

// logs returns everything the pod's process wrote. It is the only place the pod's own
// refusals are visible from outside it.
func (hh *household) logs(pod string) string {
	out, _ := hh.rig.try(hh.t, "logs", hh.container(pod))
	return out
}

// mountpoint is where a pod's work volume lives on the host, so the test can read what
// the pod wrote to it and seed what a previous claim would have left there. Nothing in
// kenward may do this — a host that can write into a running member's volume is one
// edit from reading it back, which is the property isolated mode exists to provide —
// and that is precisely why the test does: it is the independent witness.
func (hh *household) mountpoint(pod string) string {
	hh.t.Helper()
	out := hh.rig.podman(hh.t, "volume", "inspect", hh.volume(pod), "--format", "{{.Mountpoint}}")
	return strings.TrimSpace(out)
}

func (hh *household) workFile(pod string, rel ...string) string {
	return filepath.Join(append([]string{hh.mountpoint(pod)}, rel...)...)
}

func (hh *household) volumeExists(pod string) bool {
	_, err := hh.rig.try(hh.t, "volume", "inspect", hh.volume(pod))
	return err == nil
}

// bootstrapVolumes brings the household up once for the sole purpose of letting the
// supervisor create each pod's work volume, then initialises the lore instance the pod
// has to find at /work/lore and returns the space id lore made in each.
//
// It is in two steps because neither half can be skipped and neither can be reordered.
//
// The lore half is an operator step with no automation behind it: `lore mcp` exits before
// the MCP handshake when its LORE_HOME holds no account, which is the state every fresh
// work volume is in, and `kenward run` refuses to serve a unit whose memory layer does
// not answer. So a supervisor-started pod on a new volume crash-loops until somebody runs
// `lore init` against that volume, and nothing in kenward does.
//
// The supervisor half is why the volume is not simply created here first. keel's
// Create runs `podman volume create` unconditionally, and podman refuses a name that
// already exists — exit 125 — so a pod whose work volume exists and whose container does
// not can never be created at all:
//
//	supervisor: pod failed to start pod=… error="creating pod …: sandbox create volume: podman volume: exit 125"
//
// forever, on every backoff. That is keel's to fix; here it means the volume has to be
// keel's own, made the way production makes it, and written to only afterwards.
func (hh *household) bootstrapVolumes(image string, pods []string) map[string]domain.SpaceID {
	t := hh.t
	t.Helper()

	sup, _ := hh.supervisorFor(image)
	hh.start(sup, func() bool {
		for _, p := range pods {
			if !hh.volumeExists(p) {
				return false
			}
		}
		return true
	})

	spaces := make(map[string]domain.SpaceID, len(pods))
	for _, p := range pods {
		mp := hh.mountpoint(p)
		out := hh.lore(filepath.Join(mp, "lore"), "", "init", "--yes-i-saved-it", "--name", "kenward-e2e")
		space := firstUUID(out)
		if space == "" {
			t.Fatalf("`lore init` printed no space id for pod %s:\n%s", p, out)
		}
		chownTree(t, mp, podUID, podGID)
		spaces[p] = domain.SpaceID(space)
	}
	return spaces
}

// loreSearch asks a pod's own store, through a fresh lore process, whether it still
// holds an entry. It is the independent witness that a member's memory survived
// whatever the supervisor just did to their container.
// A failed search is returned rather than fatal: "the store is gone" is one of the
// answers this question has, and the assertion that names what was lost is a better
// failure than a helper aborting on lore's exit code.
func (hh *household) loreSearch(home string, space domain.SpaceID, query string) string {
	hh.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hh.rig.loreBin,
		"search", "-space", string(space), "-domain", "kenward/e2e", query)
	cmd.Env = append(os.Environ(), "LORE_HOME="+home)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// lore runs the same binary the image carries, against a LORE_HOME on the host. It is
// the independent witness for anything on a pod's volume: what it reports, kenward did
// not tell it.
func (hh *household) lore(home, stdin string, args ...string) string {
	t := hh.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hh.rig.loreBin, args...)
	cmd.Env = append(os.Environ(), "LORE_HOME="+home)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lore %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// -----------------------------------------------------------------------------
// the scenarios
// -----------------------------------------------------------------------------

// householdYAML is the shape docs/IMPLEMENTATION.md §4 describes for isolated mode:
// one bot per member, one passphrase per member, one household bot for the group.
//
// david and jordan carry a telegram_id, so both are enrolled and both get a serving pod.
// eve does not, so hers is the claim-only pod D-023 requires. Every chain is local-only
// and the cloud endpoint's key is therefore reachable by nobody, which is what makes the
// "no provider key its tier chain does not reach" assertion mean something.
const householdYAML = `mode: isolated
household:
  name: Ashfield
  shared_space: dac31e70-72e4-4b10-9cef-a6276c4a87b8
  group_chat_id: -1001234567890
  tiers: [local]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD
members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: 7d5047bb-d939-4539-b3db-8b6221a2e245
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_DAVID
    passphrase_env: KENWARD_PASSPHRASE_DAVID
  - id: jordan
    name: Jordan
    telegram_id: 87654321
    private_space: 5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_JORDAN
    passphrase_env: KENWARD_PASSPHRASE_JORDAN
  - id: eve
    name: Eve
    private_space: 9c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_EVE
    passphrase_env: KENWARD_PASSPHRASE_EVE
endpoints:
  - name: attic
    base_url: http://attic.localdomain:8000/v1
    model: qwen3-4b
    tags: [local]
    timeout: 120s
  - name: openrouter
    base_url: https://openrouter.localdomain/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: KENWARD_E2E_PROVIDER_KEY
    tags: [cloud]
memory:
  lore_command: [lore, mcp]
`

// soloYAML is one member and no group: the smallest household that can be rolled, and
// the one the revocation scenarios use so a single pod's history is unambiguous.
const soloYAML = `mode: isolated
household:
  name: Ashfield
  shared_space: dac31e70-72e4-4b10-9cef-a6276c4a87b8
  tiers: [local]
telegram:
  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD
members:
  - id: %ID%
    name: %NAME%
    private_space: 7d5047bb-d939-4539-b3db-8b6221a2e245
    tiers: [local]
    bot_token_env: KENWARD_BOT_TOKEN_%UPPER%
    passphrase_env: KENWARD_PASSPHRASE_%UPPER%
endpoints:
  - name: attic
    base_url: http://attic.localdomain:8000/v1
    model: qwen3-4b
    tags: [local]
    timeout: 120s
memory:
  lore_command: [lore, mcp]
`

func soloFor(id, name string) string {
	return strings.NewReplacer(
		"%ID%", id, "%NAME%", name, "%UPPER%", strings.ToUpper(id),
	).Replace(soloYAML)
}

const (
	e2eDavidToken  = "1111111111:AAH-e2e-davids-own-bot-token-XXXXXXXXX"
	e2eJordanToken = "2222222222:AAH-e2e-jordans-own-bot-token-YYYYYYYY"
	e2eEveToken    = "3333333333:AAH-e2e-eves-own-bot-token-ZZZZZZZZZZZ"
	e2eGroupToken  = "4444444444:AAH-e2e-household-group-bot-token-WWWW"
	e2eRexToken    = "5555555555:AAH-e2e-rexs-own-bot-token-VVVVVVVVVV"

	e2eDavidPass  = "e2e passphrase for david only"
	e2eJordanPass = "e2e passphrase for jordan only"
	e2eEvePass    = "e2e passphrase for eve only"
	e2eRexPass    = "e2e passphrase for rex only"

	e2eProviderKey = "sk-e2e-provider-key-no-pod-may-hold-this"
)

// othersOf names every secret in this household that the given unit must not hold: the
// other members' bot tokens and passphrases, the household bot where the unit is a
// member's, and the provider key no chain here reaches.
//
// It is a map rather than a list of values so a failure can say whose secret leaked
// without printing it — "jordan's passphrase" rather than fourteen characters of one,
// which for a household of similar passphrases names nobody.
func othersOf(unit string) map[string]string {
	all := map[string]string{
		"david's bot token":       e2eDavidToken,
		"david's passphrase":      e2eDavidPass,
		"jordan's bot token":      e2eJordanToken,
		"jordan's passphrase":     e2eJordanPass,
		"eve's bot token":         e2eEveToken,
		"eve's passphrase":        e2eEvePass,
		"the household bot token": e2eGroupToken,
		"the cloud provider key":  e2eProviderKey,
	}
	for label := range all {
		if unit != "group" && strings.HasPrefix(label, unit+"'s") {
			delete(all, label)
		}
		if unit == "group" && label == "the household bot token" {
			delete(all, label)
		}
	}
	return all
}

func householdEnv() map[string]string {
	return map[string]string{
		"KENWARD_BOT_TOKEN_HOUSEHOLD": e2eGroupToken,
		"KENWARD_BOT_TOKEN_DAVID":     e2eDavidToken,
		"KENWARD_BOT_TOKEN_JORDAN":    e2eJordanToken,
		"KENWARD_BOT_TOKEN_EVE":       e2eEveToken,
		"KENWARD_BOT_TOKEN_REX":       e2eRexToken,
		"KENWARD_PASSPHRASE_DAVID":    e2eDavidPass,
		"KENWARD_PASSPHRASE_JORDAN":   e2eJordanPass,
		"KENWARD_PASSPHRASE_EVE":      e2eEvePass,
		"KENWARD_PASSPHRASE_REX":      e2eRexPass,
		"KENWARD_E2E_PROVIDER_KEY":    e2eProviderKey,
	}
}

func TestIsolatedPodman(t *testing.T) {
	r := newRig(t)

	t.Run("HouseholdComesUp", func(t *testing.T) { testHouseholdComesUp(t, r) })
	t.Run("RollPreservesLore", func(t *testing.T) { testRollPreservesLore(t, r) })
	t.Run("RevocationAndStalePods", func(t *testing.T) { testRevocationAndStalePods(t, r) })
}

// testHouseholdComesUp is the first four facts at once, because they are four questions
// about one household and starting four pods twice would buy nothing:
//
//  1. every pod is created with the compose-identical argv and the household
//     configuration byte-exact at /etc/kenward/kenward.yaml;
//  2. each pod holds its own two secrets and no sibling's, and no provider key;
//  3. an enrolled member's wrapped key lands on the /work named volume and not on the
//     image's /var/lib/kenward;
//  4. a member who has not claimed gets a pod that reports awaiting enrolment rather
//     than ready, and the claim code minted on the host reaches that pod's own store.
func testHouseholdComesUp(t *testing.T, r *rig) {
	hh := newHousehold(t, r, householdYAML, householdEnv())

	david, jordan, eve, group := hh.memberPod("david"), hh.memberPod("jordan"), hh.memberPod("eve"), hh.groupPod()
	pods := []string{david, jordan, eve, group}
	hh.bootstrapVolumes(r.image, pods)

	// A claim code for the one member who has not claimed, minted by the real command
	// on the host. In isolated mode `kenward invite` also exports it to that member's
	// seed file, which is the only thing that can carry it into a pod.
	//
	// It is minted after the pods exist deliberately, because that is the case the
	// operator is warned about and the one that used to be broken: a code minted while
	// the household is already running is on the host and nowhere else until the
	// member's pod is recreated. The next start is what has to notice, and only for the
	// pod the seed belongs to.
	hh.cli("invite", "--name", "Eve", "--ttl", "720h")
	seed := filepath.Join(hh.dataDir(), inviteSeedDirName, "eve.json")
	wantDigests := digestsIn(t, seed)
	if len(wantDigests) != 1 {
		t.Fatalf("`kenward invite` wrote %d digests to %s, want exactly 1", len(wantDigests), seed)
	}

	sup, backend := hh.supervisorFor(r.image)
	watch := hh.start(sup, func() bool {
		for _, p := range pods {
			if !reachedTelegram(hh, p) {
				return false
			}
		}
		return true
	})

	// Eve's pod, and only eve's, was rebuilt to be given the seed. Rebuilding a pod
	// that has nothing new to receive would interrupt a member who is being served, so
	// the question is asked per pod rather than of the household.
	if n := backend.ops("Recreate", eve); n != 1 {
		t.Errorf("eve's pod was recreated %d time(s) to receive one claim code, want exactly 1", n)
	}
	for _, p := range []string{david, jordan, group} {
		if n := backend.ops("Recreate", p); n != 0 {
			t.Errorf("pod %s was recreated %d time(s) for a claim code that is not its member's", p, n)
		}
	}

	// --- 1. created, with the argv and the configuration the compose path uses ---

	wantConfig, err := os.ReadFile(hh.h.config)
	if err != nil {
		t.Fatalf("reading the household configuration: %v", err)
	}
	for _, p := range pods {
		v := hh.inspect(p)
		if v.State.StartedAt == "" {
			t.Errorf("pod %s was created but never started", p)
		}
		got := hh.readFromContainer(p, supervisor.PodConfigPath)
		if !bytes.Equal(got, wantConfig) {
			t.Errorf("pod %s holds a different %s than the host's configuration:\n--- want ---\n%s\n--- got ---\n%s",
				p, supervisor.PodConfigPath, wantConfig, got)
		}
	}
	assertArgv(t, hh, david, supervisor.PodCommand("--member=david"))
	assertArgv(t, hh, jordan, supervisor.PodCommand("--member=jordan"))
	assertArgv(t, hh, eve, supervisor.PodCommand("--member=eve"))
	assertArgv(t, hh, group, supervisor.PodCommand(supervisor.PodGroupFlag))

	// Every pod reached the Telegram call, which is the last thing it does and the one
	// thing this environment cannot satisfy. A pod that fails earlier — a rejected
	// argv, a configuration it cannot find, a secret it was not given, a lore that does
	// not answer — never gets here, and that is what makes this the assertion rather
	// than an excuse.
	for _, p := range pods {
		if !reachedTelegram(hh, p) {
			t.Errorf("pod %s's last run did not reach the Telegram call; it stopped earlier, at:\n%s",
				p, lastLine(hh.logs(p)))
		}
	}

	// --- 2. its own secrets and nobody else's ---

	assertPodEnv(t, hh, david, map[string]string{
		supervisor.EnvMember:       "david",
		supervisor.EnvLoreHome:     supervisor.DefaultLoreHome,
		supervisor.EnvDataDir:      supervisor.DefaultPodDataDir,
		"KENWARD_BOT_TOKEN_DAVID":  e2eDavidToken,
		"KENWARD_PASSPHRASE_DAVID": e2eDavidPass,
	}, othersOf("david"))
	assertPodEnv(t, hh, jordan, map[string]string{
		supervisor.EnvMember:        "jordan",
		"KENWARD_BOT_TOKEN_JORDAN":  e2eJordanToken,
		"KENWARD_PASSPHRASE_JORDAN": e2eJordanPass,
	}, othersOf("jordan"))
	// The group's pod holds the household bot and deliberately no passphrase: it serves
	// the shared space and holds no member's key, so a passphrase here would be a secret
	// that unwraps nothing sitting in the one pod every member talks to.
	assertPodEnv(t, hh, group, map[string]string{
		supervisor.EnvGroup:           "1",
		"KENWARD_BOT_TOKEN_HOUSEHOLD": e2eGroupToken,
	}, othersOf("group"))

	// --- 3. the wrapped key is on the work volume, not in the image ---

	for _, p := range []string{david, jordan} {
		keyFile := hh.workFile(p, "kenward", sessionsFileName)
		if _, err := os.Stat(keyFile); err != nil {
			t.Errorf("pod %s wrote no wrapped key to its %s volume (%s): %v",
				p, hh.volume(p), keyFile, err)
		}
		// And not to the path the image's own CMD would have used. A wrapped key on
		// the container layer is a key the first rolling update takes with it.
		if _, err := hh.tryReadFromContainer(p, "/var/lib/kenward/"+sessionsFileName); err == nil {
			t.Errorf("pod %s wrote a wrapped key to /var/lib/kenward, which no Recreate preserves", p)
		}
		if !mountsVolumeAt(hh.inspect(p), hh.volume(p), "/work") {
			t.Errorf("pod %s does not mount %s at /work", p, hh.volume(p))
		}
	}

	// --- 4. the claim-only pod ---

	watch.mustHaveSeen(t, "david", supervisor.StateReady)
	watch.mustHaveSeen(t, "jordan", supervisor.StateReady)
	watch.mustHaveSeen(t, "group", supervisor.StateReady)
	watch.mustHaveSeen(t, "eve", supervisor.StateNotEnrolled)
	watch.mustNeverHaveSeen(t, "eve", supervisor.StateReady)
	watch.mustNeverHaveSeen(t, "david", supervisor.StateNotEnrolled)

	// The digest, not a log line. The code was minted on the host, exported to eve's
	// seed file, provisioned into eve's pod and imported there into the store the pod
	// will actually redeem against — on eve's own volume, which the host cannot reach
	// except as this test does, from outside.
	podStore := hh.workFile(eve, "kenward", inviteStoreFileName)
	gotDigests := digestsIn(t, podStore)
	for want := range wantDigests {
		if !gotDigests[want] {
			t.Errorf("the digest minted on the host is not in eve's own store at %s\nhost seed: %v\npod store: %v",
				podStore, keysOf(wantDigests), keysOf(gotDigests))
		}
	}
	// Nobody else's pod may hold it. A seed is one member's alone.
	for _, p := range []string{david, jordan} {
		if other := digestsIn(t, hh.workFile(p, "kenward", inviteStoreFileName)); len(other) > 0 {
			t.Errorf("pod %s holds %d claim-code digest(s); eve's invite is eve's alone", p, len(other))
		}
	}

	// Nothing here may ever have reached for Purge: it is the one call that deletes a
	// member's work volume, and this path — create, start, restart — must not contain it.
	if n := backend.ops("Purge", ""); n != 0 {
		t.Errorf("Purge was called %d time(s) bringing a household up; it deletes a member's lore", n)
	}
}

// testRollPreservesLore is the highest-stakes assertion in the file: a rolling update
// moves a member's pod onto a new image without taking their memory with it.
//
// The entry is written by the real lore binary into the real store on the pod's own work
// volume, and read back by a second lore process afterwards. Between the two, the image
// changes and supervisor.Roll recreates the pod for real.
func testRollPreservesLore(t *testing.T, r *rig) {
	hh := newHousehold(t, r, soloFor("david", "David"), householdEnv())
	pod := hh.memberPod("david")
	space := hh.bootstrapVolumes(r.image, []string{pod})[pod]

	home := hh.workFile(pod, "lore")
	token := fmt.Sprintf("quillfeather-%d", time.Now().UnixNano())
	hh.lore(home, "The greenhouse override phrase is "+token+".",
		"put", "-space", string(space), "-domain", "kenward/e2e",
		"-title", "Greenhouse override phrase", "-confidence", "validated", "-body-file", "-")
	chownTree(t, hh.mountpoint(pod), podUID, podGID)
	if before := hh.loreSearch(home, space, token); !strings.Contains(before, token) {
		t.Fatalf("the entry this test is about is not in the store before the roll:\n%s", before)
	}

	// First start: the pod comes up on the first image and its image is recorded.
	sup, _ := hh.supervisorFor(r.image)
	hh.start(sup, func() bool { return reachedTelegram(hh, pod) })
	first := hh.inspect(pod)
	if !strings.Contains(first.ImageName, r.image) {
		t.Fatalf("pod %s came up on %q, want %q", pod, first.ImageName, r.image)
	}

	// Second start on a different image. Start compares it with what it recorded and
	// rolls the household onto the current one before the monitors run.
	sup2, backend := hh.supervisorFor(r.rolled)
	hh.start(sup2, func() bool {
		v, err := hh.tryInspect(pod)
		return err == nil && strings.Contains(v.ImageName, r.rolled)
	})

	second := hh.inspect(pod)
	if second.ID == first.ID {
		t.Errorf("the pod's container was never replaced: still %s", first.ID[:12])
	}
	if !strings.Contains(second.ImageName, r.rolled) {
		t.Errorf("pod %s is on %q after the roll, want %q", pod, second.ImageName, r.rolled)
	}
	if n := backend.ops("Recreate", pod); n == 0 {
		t.Errorf("the roll never called Recreate on %s; it must be the only way a pod's container is replaced", pod)
	}
	if n := backend.ops("Purge", ""); n != 0 {
		t.Errorf("Purge was called %d time(s) during a rolling update; it is the one call that deletes the member's lore", n)
	}

	// The volume, and the member's memory on it, outlived the container.
	if _, err := hh.rig.try(t, "volume", "inspect", hh.volume(pod)); err != nil {
		t.Fatalf("the work volume %s did not survive the roll: %v", hh.volume(pod), err)
	}
	after := hh.loreSearch(home, space, token)
	if !strings.Contains(after, token) {
		t.Errorf("the member's lore did not survive the roll: %q is gone from %s\n%s", token, home, after)
	}
}

// testRevocationAndStalePods covers the two facts that only exist together: a revocation
// recorded on the host has no way to reach the pod that holds the binding except by the
// pod being recreated, and a pod may not be recreated every time the household starts.
//
//  1. a revocation recorded on the host reaches the pod and clears its binding;
//  2. the pod is rebuilt once for it and not on every subsequent start;
//  3. a record naming a different member is refused rather than acted on.
func testRevocationAndStalePods(t *testing.T, r *rig) {
	hh := newHousehold(t, r, soloFor("rex", "Rex"), householdEnv())
	pod := hh.memberPod("rex")
	hh.bootstrapVolumes(r.image, []string{pod})

	// The binding this revocation has to clear was written inside the pod when rex
	// claimed, on the pod's own volume, which is why the host cannot clear it and why
	// this test has to put it there the same way — from outside, as the pod's own
	// process would have left it.
	seedBinding(t, hh, pod, "rex", 55512345, time.Now().Add(-30*24*time.Hour))

	sup1, _ := hh.supervisorFor(r.image)
	hh.start(sup1, func() bool { return reachedTelegram(hh, pod) })
	created := hh.inspect(pod)
	if !bindingPresent(t, hh, pod, "rex") {
		t.Fatalf("the pod did not come up holding the binding this test is about")
	}

	// --- 1 & 2: recorded on the host, applied in the pod, once ---

	hh.cli("revoke", "rex")
	record := filepath.Join(hh.dataDir(), revocationDirName, "rex.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("`kenward revoke rex` recorded nothing at %s: %v", record, err)
	}
	// The staleness question compares the record's modification time against the pod's
	// creation time with a two-second tolerance for coarse filesystem timestamps, so an
	// operator who revokes and restarts inside that window buys one extra rebuild. This
	// test asserts the rebuild happens exactly once, so it waits out the tolerance
	// rather than measuring it.
	time.Sleep(3 * time.Second)

	sup2, backend2 := hh.supervisorFor(r.image)
	hh.start(sup2, func() bool {
		v, err := hh.tryInspect(pod)
		return err == nil && v.ID != created.ID && reachedTelegram(hh, pod)
	})
	rebuilt := hh.inspect(pod)
	if rebuilt.ID == created.ID {
		t.Fatalf("the pod was not rebuilt, so the revocation never reached it")
	}
	if n := backend2.ops("Recreate", pod); n != 1 {
		t.Errorf("Recreate was called %d time(s) delivering one revocation, want exactly 1", n)
	}
	if bindingPresent(t, hh, pod, "rex") {
		t.Errorf("the pod still holds rex's binding after the revocation reached it; it is still serving the account")
	}
	if log := hh.logs(pod); strings.Contains(log, "refusing to unbind") {
		t.Errorf("the pod refused its own revocation:\n%s", log)
	}

	// --- 2: and not again ---
	//
	// The wait is on the pod being *started* — podman's own StartedAt advancing — which
	// is what ensureRunning does to a container that already exists. A pod that was
	// instead replaced would have a new container and a new id, which is the next
	// assertion, so waiting for a start rather than for a recreation is what lets this
	// distinguish the two rather than assume one.
	sup3, backend3 := hh.supervisorFor(r.image)
	hh.start(sup3, func() bool {
		v, err := hh.tryInspect(pod)
		return err == nil && v.State.StartedAt != rebuilt.State.StartedAt
	})
	if n := backend3.ops("Recreate", pod); n != 0 {
		t.Errorf("the pod was recreated %d more time(s) on a later start; the record is delivered once, "+
			"and a pod created after it already holds it", n)
	}
	if now := hh.inspect(pod); now.ID != rebuilt.ID {
		t.Errorf("the pod's container changed on a start with nothing new to deliver: %s → %s",
			rebuilt.ID[:12], now.ID[:12])
	}

	// --- 3: a record naming somebody else is refused ---

	seedBinding(t, hh, pod, "rex", 55512345, time.Now().Add(-30*24*time.Hour))
	writeFile(t, record, mustJSON(t, revocation{MemberID: "someone-else", RevokedAt: time.Now()}), 0o644)

	sup4, _ := hh.supervisorFor(r.image)
	hh.start(sup4, func() bool {
		return strings.Contains(hh.logs(pod), "refusing to unbind")
	})
	log := hh.logs(pod)
	for _, want := range []string{`revokes member "someone-else"`, `this pod serves member "rex"`, "refusing to unbind"} {
		if !strings.Contains(log, want) {
			t.Errorf("the pod's refusal does not contain %q:\n%s", want, log)
		}
	}
	if !bindingPresent(t, hh, pod, "rex") {
		t.Errorf("the pod unbound rex on the strength of a record naming somebody else")
	}
}

// -----------------------------------------------------------------------------
// assertions
// -----------------------------------------------------------------------------

// reachedTelegram reports whether a pod ran all the way to the one call this
// environment cannot satisfy. See the file comment: it is the proof that everything
// before it worked.
// It reads the pod's last line rather than searching the whole log, because a container
// accumulates every run it has had and the question is about the most recent one: a pod
// that reached Telegram yesterday and cannot read its configuration today has failed,
// and a log that merely contains "getMe" somewhere cannot tell the two apart.
func reachedTelegram(hh *household, pod string) bool {
	if !hh.containerExists(pod) {
		return false
	}
	return strings.Contains(lastLine(hh.logs(pod)), "getMe")
}

// lastLine is the final non-empty line of a pod's log: how its most recent run ended.
func lastLine(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

func assertArgv(t *testing.T, hh *household, pod string, want []string) {
	t.Helper()
	got := hh.inspect(pod).Config.Cmd
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("pod %s runs\n  %q\nwant\n  %q", pod, got, want)
	}
}

// assertPodEnv checks the container's own environment as podman recorded it: every
// variable the pod must have with the value it must have, and no trace anywhere of any
// value it must not.
//
// The forbidden values are searched for across the whole of `podman inspect`, not only
// the environment, because a secret that reached a pod by some other route — a
// provisioned file, a command-line argument — is the same failure.
func assertPodEnv(t *testing.T, hh *household, pod string, want map[string]string, forbidden map[string]string) {
	t.Helper()
	v := hh.inspect(pod)
	have := map[string]string{}
	for _, kv := range v.Config.Env {
		if k, val, ok := strings.Cut(kv, "="); ok {
			have[k] = val
		}
	}
	for k, wantVal := range want {
		switch got, ok := have[k]; {
		case !ok:
			t.Errorf("pod %s has no %s in its environment", pod, k)
		case got != wantVal:
			t.Errorf("pod %s has the wrong %s", pod, k)
		}
	}
	for label, secret := range forbidden {
		if strings.Contains(v.raw, secret) {
			t.Errorf("pod %s holds %s; a unit's pod holds its own secrets and no others", pod, label)
		}
	}
}

func mountsVolumeAt(v containerView, volume, dest string) bool {
	for _, m := range v.Mounts {
		if m.Name == volume && m.Destination == dest {
			return true
		}
	}
	return false
}

// readFromContainer copies one file out of a container's filesystem. The image is
// distroless — no shell, no coreutils — so `podman cp` is the only way in.
func (hh *household) readFromContainer(pod, path string) []byte {
	hh.t.Helper()
	b, err := hh.tryReadFromContainer(pod, path)
	if err != nil {
		hh.t.Fatalf("reading %s from pod %s: %v", path, pod, err)
	}
	return b
}

func (hh *household) tryReadFromContainer(pod, path string) ([]byte, error) {
	dst := filepath.Join(hh.t.TempDir(), "copied")
	if out, err := hh.rig.try(hh.t, "cp", hh.container(pod)+":"+path, dst); err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return os.ReadFile(dst)
}

// digestsIn reads an invite store — the host's per-member seed or the pod's own — and
// returns the code digests in it. A missing file is no invites, which is what most pods
// have.
func digestsIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out
	case err != nil:
		t.Fatalf("reading the invite store at %s: %v", path, err)
	}
	var file struct {
		Codes []enrol.Code `json:"codes"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decoding the invite store at %s: %v", path, err)
	}
	for _, c := range file.Codes {
		out[c.Hash] = true
	}
	return out
}

// seedBinding writes the enrolment record a claim redeemed inside the pod would have
// left on the pod's own volume, and nowhere else. The host's state file is deliberately
// left alone: in this mode the host does not hold the binding, which is the whole reason
// a revocation has to travel.
func seedBinding(t *testing.T, hh *household, pod, id string, telegramID int64, at time.Time) {
	t.Helper()
	st := config.NewState()
	st.Bind(domain.MemberID(id), telegramID, at)
	path := hh.workFile(pod, "kenward", config.StateFileName)
	mkdirAll(t, filepath.Dir(path))
	if err := st.Save(path); err != nil {
		t.Fatalf("seeding %s's binding on the pod's volume: %v", id, err)
	}
	chownTree(t, hh.mountpoint(pod), podUID, podGID)
}

func bindingPresent(t *testing.T, hh *household, pod, id string) bool {
	t.Helper()
	st, err := config.LoadState(hh.workFile(pod, "kenward", config.StateFileName))
	if err != nil {
		t.Fatalf("reading the pod's enrolment state: %v", err)
	}
	b, ok := st.Binding(domain.MemberID(id))
	return ok && b.TelegramID != 0
}

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func firstUUID(s string) string {
	for _, f := range strings.Fields(s) {
		if len(f) == 36 && strings.Count(f, "-") == 4 {
			return f
		}
	}
	return ""
}

func chownTree(t *testing.T, root string, uid, gid int) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
	if err != nil {
		t.Fatalf("giving %s to the pod's own account: %v", root, err)
	}
}

func writeFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

func copyFile(t *testing.T, src, dst string, mode fs.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	writeFile(t, dst, b, mode)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encoding %T: %v", v, err)
	}
	return append(b, '\n')
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
