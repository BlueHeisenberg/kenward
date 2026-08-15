package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/keel/sandbox"
	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// Environment variables the pod image's entrypoint reads to decide which single
// unit it runs. sandbox.Spec carries no command vector, so unit selection travels
// as environment: the entrypoint translates these into `kenward run --member ID`
// or `kenward run --group`.
const (
	// EnvMember names the member a pod serves.
	EnvMember = "KENWARD_MEMBER"
	// EnvGroup marks the household group's pod. Its value is "1".
	EnvGroup = "KENWARD_GROUP"
	// EnvLoreHome is lore's own isolation switch: one LORE_HOME is one lore
	// instance, which is exactly what makes one lore per pod work.
	EnvLoreHome = "LORE_HOME"
)

// Defaults for IsolatedOptions.
const (
	// DefaultPollInterval is how often each pod's liveness is inspected.
	DefaultPollInterval = 15 * time.Second
	// DefaultHealthyReset is how long a restarted pod must stay up before its
	// backoff schedule returns to base.
	DefaultHealthyReset = time.Minute
	// DefaultLoreHome is where each pod keeps its lore instance, on the pod's
	// own work volume.
	DefaultLoreHome = "/var/lib/kenward/lore"
	// DefaultNamePrefix prefixes every sandbox name this supervisor creates.
	DefaultNamePrefix = "kenward"
)

// IsolatedOptions configures an Isolated supervisor.
type IsolatedOptions struct {
	// Backend runs the pods. Nil builds a Podman backend with default naming.
	Backend sandbox.Backend
	// Image is the pod image reference. Required: there is no sensible default
	// for the artifact a household's private conversations run inside.
	Image string
	// NamePrefix prefixes sandbox names. Defaults to DefaultNamePrefix.
	NamePrefix string
	// NetworkPolicy is applied to every pod. The zero value is deliberately
	// widened to sandbox.NetworkPolicyOpen rather than the sandbox default of
	// internal-only, because a pod that cannot reach Telegram serves nobody.
	// An operator who wants egress restricted supplies NetworkPolicyFiltered
	// together with AllowDomains and EgressProxyAddr.
	NetworkPolicy sandbox.NetworkPolicy
	// AllowDomains and EgressProxyAddr pass through to the pod spec for
	// NetworkPolicyFiltered.
	AllowDomains    []string
	EgressProxyAddr string
	// LoreHome is the in-pod LORE_HOME. Defaults to DefaultLoreHome.
	LoreHome string
	// PollInterval is the liveness poll. Defaults to DefaultPollInterval.
	PollInterval time.Duration
	// RestartBackoff and MaxRestartBackoff schedule per-pod restarts. Defaults:
	// DefaultRestartBackoff, DefaultMaxRestartBackoff.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// HealthyReset is how long a pod must stay up before its backoff resets.
	// Defaults to DefaultHealthyReset.
	HealthyReset time.Duration
	// Logger receives lifecycle events. Nil discards.
	Logger *slog.Logger
	// LookupEnv resolves the bot token variables named in the configuration.
	// Nil means os.LookupEnv.
	LookupEnv config.LookupEnvFunc
	// Now supplies timestamps for health reporting. Nil means time.Now.
	Now func() time.Time
}

func (o IsolatedOptions) normalized() IsolatedOptions {
	if o.NamePrefix == "" {
		o.NamePrefix = DefaultNamePrefix
	}
	if o.NetworkPolicy == "" {
		o.NetworkPolicy = sandbox.NetworkPolicyOpen
	}
	if o.LoreHome == "" {
		o.LoreHome = DefaultLoreHome
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.RestartBackoff <= 0 {
		o.RestartBackoff = DefaultRestartBackoff
	}
	if o.MaxRestartBackoff <= 0 {
		o.MaxRestartBackoff = DefaultMaxRestartBackoff
	}
	if o.HealthyReset <= 0 {
		o.HealthyReset = DefaultHealthyReset
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// pod is one unit's sandbox: a member's, or the household group's.
type pod struct {
	key  unitKey
	name string
	spec sandbox.Spec
}

// Isolated runs one pod per enrolled member plus one for the household group, via
// keel/sandbox. Each pod holds its own bot token, its own lore instance behind its
// own LORE_HOME, and its own key; the host process holds none of them, which is
// the whole point of the mode. Inside every pod runs the same assistant.Unit that
// simple mode runs as a goroutine — the pod boundary is the only difference, and
// no unit can tell.
//
// Restarts are per-pod with per-pod backoff, so one member crash-looping burns
// their own schedule and never takes the household down. A pod is only ever
// created once and thereafter stopped and started: Destroy would take the pod's
// work volume with it, and the work volume is where the member's lore lives.
//
// An Isolated is single-use: construct, Start once, Stop once.
type Isolated struct {
	opts    IsolatedOptions
	logger  *slog.Logger
	backend sandbox.Backend
	tracker *tracker
	pods    []pod
	cfg     *config.Config

	draining  chan struct{}
	stoppedCh chan struct{}
	stopOnce  sync.Once
	stopErr   error
	wg        sync.WaitGroup

	mu      sync.Mutex
	started bool

	// testHookBackoff observes each backoff delay before it is slept, so tests
	// can assert the schedule without measuring wall time. Never set in
	// production.
	testHookBackoff func(unitKey, time.Duration)
}

// NewIsolated wires an Isolated supervisor over cfg.
//
// Anywhere other than Linux it returns ErrUnsupportedMode, always as an error and
// never as a downgrade: a household that asked for sealed memory and quietly got
// shared memory would believe something false, which is worse than being refused.
func NewIsolated(cfg *config.Config, opts IsolatedOptions) (*Isolated, error) {
	return newIsolated(cfg, opts, runtime.GOOS)
}

// newIsolated is NewIsolated with the platform injected, so the refusal is
// testable from any host.
func newIsolated(cfg *config.Config, opts IsolatedOptions, goos string) (*Isolated, error) {
	if goos != "linux" {
		return nil, fmt.Errorf("supervisor: isolated mode requires Linux, this host is %s: %w",
			goos, ErrUnsupportedMode)
	}
	if cfg == nil {
		return nil, errors.New("supervisor: nil configuration")
	}
	if cfg.Mode != config.ModeIsolated {
		return nil, fmt.Errorf("supervisor: isolated supervisor given mode %q", cfg.Mode)
	}
	opts = opts.normalized()
	if opts.Image == "" {
		return nil, errors.New("supervisor: isolated mode requires a pod image")
	}

	i := &Isolated{
		opts:      opts,
		logger:    opts.Logger,
		backend:   opts.Backend,
		tracker:   newTracker(opts.Now),
		cfg:       snapshotConfig(cfg),
		draining:  make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
	if i.backend == nil {
		i.backend = sandbox.NewPodmanBackend(sandbox.Config{}, opts.Logger)
	}

	var missing []string
	for _, m := range i.cfg.DomainMembers() {
		if !m.Enrolled() {
			i.tracker.addNotEnrolled(m.ID)
			continue
		}
		token, ok := opts.LookupEnv(m.BotTokenEnv)
		if m.BotTokenEnv == "" || !ok || token == "" {
			missing = append(missing, fmt.Sprintf("member %s: %s", m.ID, m.BotTokenEnv))
			continue
		}
		k := unitKey{member: m.ID}
		i.pods = append(i.pods, pod{
			key:  k,
			name: podName(opts.NamePrefix, "member-"+string(m.ID)),
			spec: i.podSpec(podName(opts.NamePrefix, "member-"+string(m.ID)), map[string]string{
				EnvMember:     string(m.ID),
				EnvLoreHome:   opts.LoreHome,
				m.BotTokenEnv: token,
			}),
		})
		i.tracker.add(k)
	}
	if i.cfg.Household.GroupChatID != 0 {
		env := i.cfg.Telegram.BotTokenEnv
		token, ok := opts.LookupEnv(env)
		if env == "" || !ok || token == "" {
			missing = append(missing, fmt.Sprintf("group: %s", env))
		} else {
			k := unitKey{group: true}
			name := podName(opts.NamePrefix, "group")
			i.pods = append(i.pods, pod{
				key:  k,
				name: name,
				spec: i.podSpec(name, map[string]string{
					EnvGroup:    "1",
					EnvLoreHome: opts.LoreHome,
					env:         token,
				}),
			})
			i.tracker.add(k)
		}
	}
	if len(missing) > 0 {
		// Refusing to start half-configured beats starting a household where one
		// member's pod silently never comes up.
		return nil, fmt.Errorf("supervisor: bot token environment variables missing or empty: %s",
			strings.Join(missing, "; "))
	}
	if len(i.pods) == 0 {
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return i, nil
}

func (i *Isolated) podSpec(name string, env map[string]string) sandbox.Spec {
	return sandbox.Spec{
		Name:            name,
		Image:           i.opts.Image,
		Level:           sandbox.LevelFast,
		Env:             env,
		NetworkPolicy:   i.opts.NetworkPolicy,
		AllowDomains:    append([]string(nil), i.opts.AllowDomains...),
		EgressProxyAddr: i.opts.EgressProxyAddr,
	}
}

// podName builds a sandbox name from the parts, restricted to the [A-Za-z0-9_-]
// alphabet sandbox.Spec.Name requires. Anything else becomes a hyphen; member ids
// are unique in configuration, so names stay unique.
func podName(prefix, suffix string) string {
	sanitize := func(s string) string {
		out := make([]byte, 0, len(s))
		for j := 0; j < len(s); j++ {
			c := s[j]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
				out = append(out, c)
			default:
				out = append(out, '-')
			}
		}
		return string(out)
	}
	return sanitize(prefix) + "-" + sanitize(suffix)
}

// Start brings every pod up and blocks until ctx is cancelled or Stop is called.
// A pod that cannot be created or started is retried on its own backoff schedule
// rather than failing the household: the machines a pod depends on come and go,
// and one member's trouble is never everyone's outage.
func (i *Isolated) Start(ctx context.Context) error {
	i.mu.Lock()
	if i.started {
		i.mu.Unlock()
		return errors.New("supervisor: already started")
	}
	select {
	case <-i.draining:
		i.mu.Unlock()
		return errors.New("supervisor: already stopped")
	default:
	}
	i.started = true
	i.mu.Unlock()

	i.logStartup()

	for _, p := range i.pods {
		i.wg.Add(1)
		go i.runPod(ctx, p)
	}

	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), DefaultDrainTimeout)
		defer cancel()
		_ = i.shutdown(stopCtx)
		return ctx.Err()
	case <-i.stoppedCh:
		return nil
	}
}

func (i *Isolated) logStartup() {
	for _, m := range i.cfg.DomainMembers() {
		if m.Enrolled() {
			i.logger.Info("supervisor: pod for member", "member", string(m.ID), "tiers", m.Tiers)
		} else {
			i.logger.Info("supervisor: member not enrolled, no pod", "member", string(m.ID))
		}
	}
	if i.cfg.Household.GroupChatID != 0 {
		i.logger.Info("supervisor: pod for household group", "tiers", i.cfg.Household.Tiers)
	}
	i.logger.Info("supervisor: started",
		"mode", privacy.ModeIsolated.String(),
		"privacy", privacy.Statement(privacy.ModeIsolated))
}

// runPod owns one pod's lifecycle: bring it up, watch it, and bring it back with
// backoff when it exits unexpectedly. Every backend call is bounded by its own
// timeout so a hung runtime cannot wedge shutdown.
func (i *Isolated) runPod(ctx context.Context, p pod) {
	defer i.wg.Done()
	bo := newBackoff(i.opts.RestartBackoff, i.opts.MaxRestartBackoff)

	for {
		i.tracker.set(p.key, StateStarting)
		if err := i.ensureRunning(ctx, p); err != nil {
			if i.stoppingOr(ctx) {
				return
			}
			i.logger.Warn("supervisor: pod failed to start", "pod", p.name, "error", err)
			i.tracker.fail(p.key, err)
			if !i.sleep(ctx, i.backoffDelay(p.key, bo)) {
				return
			}
			continue
		}
		i.tracker.set(p.key, StateReady)
		readyAt := i.opts.Now()
		didReset := false

		// Watch until it dies or we are told to stop.
		for {
			if !i.sleep(ctx, i.opts.PollInterval) {
				return
			}
			st, err := i.inspect(ctx, p.name)
			if err != nil || !st.Running {
				if i.stoppingOr(ctx) {
					return
				}
				if err == nil {
					err = errors.New("supervisor: pod exited unexpectedly")
				}
				i.logger.Warn("supervisor: pod down, restarting", "pod", p.name, "error", err)
				i.tracker.fail(p.key, err)
				break
			}
			if !didReset && i.opts.Now().Sub(readyAt) >= i.opts.HealthyReset {
				bo.reset()
				didReset = true
			}
		}
		if !i.sleep(ctx, i.backoffDelay(p.key, bo)) {
			return
		}
	}
}

func (i *Isolated) backoffDelay(k unitKey, bo *backoff) time.Duration {
	d := bo.next()
	if i.testHookBackoff != nil {
		i.testHookBackoff(k, d)
	}
	return d
}

// ensureRunning creates the pod if it does not exist and starts it if it is
// stopped. It never destroys: the pod's work volume holds the member's lore
// instance, and a restart policy that erased memory would be worse than no
// restart policy.
func (i *Isolated) ensureRunning(ctx context.Context, p pod) error {
	st, err := i.inspect(ctx, p.name)
	switch {
	case errors.Is(err, sandbox.ErrSandboxNotFound):
		cctx, cancel := i.callContext(ctx)
		defer cancel()
		_, cerr := i.backend.Create(cctx, p.spec)
		if cerr != nil {
			return fmt.Errorf("creating pod %s: %w", p.name, cerr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inspecting pod %s: %w", p.name, err)
	case st.Running:
		return nil
	default:
		sctx, cancel := i.callContext(ctx)
		defer cancel()
		if serr := i.backend.Start(sctx, p.name); serr != nil {
			return fmt.Errorf("starting pod %s: %w", p.name, serr)
		}
		return nil
	}
}

func (i *Isolated) inspect(ctx context.Context, name string) (sandbox.Status, error) {
	cctx, cancel := i.callContext(ctx)
	defer cancel()
	return i.backend.Inspect(cctx, name)
}

// callContext bounds one backend call so a hung container runtime cannot wedge a
// monitor goroutine past shutdown.
func (i *Isolated) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := 4 * i.opts.PollInterval
	if timeout < time.Second {
		timeout = time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func (i *Isolated) stoppingOr(ctx context.Context) bool {
	select {
	case <-i.draining:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// sleep waits d unless shutdown begins or ctx ends first.
func (i *Isolated) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-i.draining:
		return false
	case <-ctx.Done():
		return false
	}
}

// Stop drains the household pod by pod: each is stopped gracefully — SIGTERM,
// then wait — which is the signal the runtime inside every pod answers by
// finishing its in-flight turn, locking its own sessions and exiting. The host
// process holds no keys, so there is nothing further to lock here; in this mode
// session custody lives inside the pods, and their graceful stop is what locks
// them. Pods are stopped, never destroyed: a pod's work volume holds its member's
// lore instance and outlives every restart and every Stop.
//
// ctx bounds the graceful stops. Stop is idempotent and may be called before
// Start.
func (i *Isolated) Stop(ctx context.Context) error {
	return i.shutdown(ctx)
}

func (i *Isolated) shutdown(ctx context.Context) error {
	i.stopOnce.Do(func() {
		close(i.draining)

		var wg sync.WaitGroup
		for _, p := range i.pods {
			wg.Add(1)
			go func(p pod) {
				defer wg.Done()
				if err := i.backend.Stop(ctx, p.name); err != nil && !errors.Is(err, sandbox.ErrSandboxNotFound) {
					i.logger.Warn("supervisor: stopping pod", "pod", p.name, "error", err)
				}
			}(p)
		}
		wg.Wait()
		i.wg.Wait()
		if err := ctx.Err(); err != nil {
			i.stopErr = err
		}
		i.tracker.stopAll()
		close(i.stoppedCh)
	})
	<-i.stoppedCh
	return i.stopErr
}

// Health reports every pod's condition from the supervisor's own observations —
// the monitor goroutines write, Health reads a snapshot. It never calls the
// container runtime and never touches anything external, so it is callable before
// Start, after Stop, and while a pod is mid-crash-loop. Members who have not
// enrolled appear with StateUnknown and ErrNotEnrolled: they have no pod, which
// is a fact, not a failure.
func (i *Isolated) Health(_ context.Context) ([]UnitHealth, error) {
	return i.tracker.snapshot(), nil
}

// Isolated implements Supervisor.
var _ Supervisor = (*Isolated)(nil)
