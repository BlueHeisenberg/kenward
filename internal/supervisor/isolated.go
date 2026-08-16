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
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// Environment variables set on every pod. When IsolatedOptions.ConfigFile is
// given, pods are also started with the same argv the compose deployment uses —
// `--config=… --data-dir=… --member=ID | --group` — so the two deployment paths
// share one contract; the variables remain for an entrypoint that prefers them.
const (
	// EnvMember names the member a pod serves.
	EnvMember = "KENWARD_MEMBER"
	// EnvGroup marks the household group's pod. Its value is "1".
	EnvGroup = "KENWARD_GROUP"
	// EnvLoreHome is lore's own isolation switch: one LORE_HOME is one lore
	// instance, which is exactly what makes one lore per pod work.
	EnvLoreHome = "LORE_HOME"
	// EnvDataDir is the pod's kenward data directory — wrapped keys, enrolment
	// state. The entrypoint maps it to `--data-dir`.
	EnvDataDir = "KENWARD_DATA_DIR"
)

// Defaults for IsolatedOptions.
const (
	// DefaultPollInterval is how often each pod's liveness is inspected.
	DefaultPollInterval = 15 * time.Second
	// DefaultHealthyReset is how long a restarted pod must stay up before its
	// backoff schedule returns to base.
	DefaultHealthyReset = time.Minute
	// DefaultLoreHome is where each pod keeps its lore instance. It lives under
	// /work deliberately: /work is the pod's named volume — the only path that
	// survives the pod's container being recreated — and a lore home anywhere
	// else would be erased by the first rolling update.
	DefaultLoreHome = "/work/lore"
	// DefaultPodDataDir is the pod's kenward data directory, under /work for
	// the same reason: a member's wrapped key must outlive the container.
	DefaultPodDataDir = "/work/kenward"
	// DefaultNamePrefix prefixes every sandbox name this supervisor creates.
	DefaultNamePrefix = "kenward"
	// DefaultRollTimeout bounds how long a rolling update waits for one
	// recreated pod to come back healthy before declaring the roll failed.
	DefaultRollTimeout = 2 * time.Minute
	// PodConfigPath is where the household configuration is provisioned inside a
	// pod — the same path the compose deployment mounts it at.
	PodConfigPath = "/etc/kenward/kenward.yaml"
	// podCredentialsDir is the synthetic CREDENTIALS_DIRECTORY a pod is given
	// when its token's source is a systemd credential: the credential is
	// provisioned as a 0600 file there, mirroring on the inside exactly what
	// systemd did on the outside, so the pod's own resolver finds it without
	// the configuration changing shape.
	podCredentialsDir = "/run/kenward/credentials"
)

// RollError reports where a rolling update stopped. Everything before the named
// unit was recreated and healthy; everything after it was left untouched on its
// working old pod.
type RollError struct {
	// Member is the member whose pod failed; empty when Group is true.
	Member domain.MemberID
	// Group marks the household group's pod.
	Group bool
	// Err is what went wrong.
	Err error
}

func (e *RollError) Error() string {
	unit := "member " + string(e.Member)
	if e.Group {
		unit = "the household group"
	}
	return fmt.Sprintf("supervisor: rolling update stopped at %s; later pods left on the previous image: %v", unit, e.Err)
}

func (e *RollError) Unwrap() error { return e.Err }

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
	// ConfigFile is the path to the household's kenward.yaml on the host. When
	// set, its contents are provisioned into every pod at PodConfigPath and the
	// pod is started with the compose-identical argv
	// (`--config=… --data-dir=… --member=ID | --group`). When empty, unit
	// selection travels as environment only and the image is expected to carry
	// or locate its own configuration.
	ConfigFile string
	// PollInterval is the liveness poll. Defaults to DefaultPollInterval.
	PollInterval time.Duration
	// RestartBackoff and MaxRestartBackoff schedule per-pod restarts. Defaults:
	// DefaultRestartBackoff, DefaultMaxRestartBackoff.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// HealthyReset is how long a pod must stay up before its backoff resets.
	// Defaults to DefaultHealthyReset.
	HealthyReset time.Duration
	// RollTimeout bounds how long Roll waits for one recreated pod to become
	// healthy before stopping the roll. Defaults to DefaultRollTimeout.
	RollTimeout time.Duration
	// Logger receives lifecycle events. Nil discards.
	Logger *slog.Logger
	// Secrets resolves each pod's bot token on the host, from whichever source
	// the configuration states — a file, an environment variable, or a systemd
	// credential. Nil builds a resolver over LookupEnv, the real filesystem and
	// the CREDENTIALS_DIRECTORY systemd supplies.
	Secrets *config.Secrets
	// LookupEnv seeds the default Secrets resolver. Nil means os.LookupEnv.
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
	if o.RollTimeout <= 0 {
		o.RollTimeout = DefaultRollTimeout
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
	// base is the pod's spec without its secret. specFor completes it at
	// Create and Recreate time, so the token's value never rests on a struct a
	// logger could print, and a rotated token file or credential is picked up
	// by the next recreation without a supervisor restart.
	base sandbox.Spec
	// token resolves this pod's own bot token through config's Secrets API —
	// file, environment variable or systemd credential, whichever the
	// configuration states for this unit.
	token func() (config.Secret, error)
	// Exactly one of these carries the token into the pod, mirroring the
	// stated source: set as an environment variable of this name, provisioned
	// as a 0600 file at this path, or provisioned as this credential under
	// podCredentialsDir.
	tokenEnv  string
	tokenFile string
	tokenCred string

	// opMu serialises lifecycle operations on this pod, which the sandbox
	// backend requires per id. The monitor takes it with TryLock and stands
	// aside when it cannot — a rolling update owning the pod must not race a
	// monitor trying to "rescue" the container it is deliberately replacing.
	opMu sync.Mutex
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
// created once and thereafter stopped, started and recreated — never purged:
// Purge takes the pod's work volume with it, and the work volume is where the
// member's lore lives.
// After the host binary updates, Roll recreates pods on the new image, one at a
// time — see Roll for the constraints that shape it.
//
// An Isolated is single-use: construct, Start once, Stop once.
type Isolated struct {
	opts    IsolatedOptions
	logger  *slog.Logger
	backend sandbox.Backend
	tracker *tracker
	pods    []*pod
	cfg     *config.Config
	rollMu  sync.Mutex

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
	var configYAML []byte
	if opts.ConfigFile != "" {
		b, err := os.ReadFile(opts.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("supervisor: reading configuration for pods: %w", err)
		}
		configYAML = b
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

	secrets := opts.Secrets
	if secrets == nil {
		secrets = config.NewSecrets(config.SecretOptions{LookupEnv: opts.LookupEnv})
	}

	// A pod name is the isolation boundary made physical: one name is one
	// container, one work volume, one lore instance, one bot token. Sanitisation
	// is not injective — "a.b" and "a-b" both become "a-b" — and configuration
	// validation rejects only empty and exactly-duplicate ids, so two members can
	// arrive here mapping to one name. Whoever came second would find the first
	// member's pod already running and report itself ready on it. The mapping is
	// therefore proved injective here, at the layer where the damage would
	// happen, rather than trusted from the layer above.
	names := make(map[string]string, len(i.cfg.Members)+1)
	claimName := func(name, owner string) error {
		if prev, ok := names[name]; ok {
			return fmt.Errorf("supervisor: %s and %s both name pod %q; refusing to start rather than serve one from the other's pod, with the other's lore volume and the other's bot token",
				prev, owner, name)
		}
		names[name] = owner
		return nil
	}

	var missing []string
	for _, mc := range i.cfg.Members {
		m := mc.Domain()
		if !m.Enrolled() {
			i.tracker.addNotEnrolled(m.ID)
			continue
		}
		// Prove the token readable now — a household must never start
		// half-configured — and drop the value: pods resolve again at create
		// time, through the same closure.
		mc := mc
		tokenFn := func() (config.Secret, error) { return mc.BotToken(secrets) }
		if _, err := tokenFn(); err != nil {
			missing = append(missing, fmt.Sprintf("member %s: %v", m.ID, err))
			continue
		}
		k := unitKey{member: m.ID}
		name := podName(opts.NamePrefix, "member-"+string(m.ID))
		if err := claimName(name, "member "+string(m.ID)); err != nil {
			return nil, err
		}
		p := &pod{
			key:   k,
			name:  name,
			token: tokenFn,
			base: i.podSpec(name, map[string]string{
				EnvMember:   string(m.ID),
				EnvLoreHome: opts.LoreHome,
				EnvDataDir:  DefaultPodDataDir,
			}, "--member="+string(m.ID), configYAML),
		}
		if err := p.setDelivery(mc.BotTokenRef()); err != nil {
			missing = append(missing, fmt.Sprintf("member %s: %v", m.ID, err))
			continue
		}
		i.pods = append(i.pods, p)
		i.tracker.add(k)
	}
	if i.cfg.Household.GroupChatID != 0 {
		cfgSnapshot := i.cfg
		tokenFn := func() (config.Secret, error) { return cfgSnapshot.BotToken(secrets) }
		if _, err := tokenFn(); err != nil {
			missing = append(missing, fmt.Sprintf("group: %v", err))
		} else {
			k := unitKey{group: true}
			name := podName(opts.NamePrefix, "group")
			if err := claimName(name, "the household group"); err != nil {
				return nil, err
			}
			p := &pod{
				key:   k,
				name:  name,
				token: tokenFn,
				base: i.podSpec(name, map[string]string{
					EnvGroup:    "1",
					EnvLoreHome: opts.LoreHome,
					EnvDataDir:  DefaultPodDataDir,
				}, "--group", configYAML),
			}
			if err := p.setDelivery(cfgSnapshot.BotTokenRef()); err != nil {
				missing = append(missing, fmt.Sprintf("group: %v", err))
			} else {
				i.pods = append(i.pods, p)
				i.tracker.add(k)
			}
		}
	}
	if len(missing) > 0 {
		// Refusing to start half-configured beats starting a household where one
		// member's pod silently never comes up.
		return nil, fmt.Errorf("supervisor: bot tokens unresolvable: %s",
			strings.Join(missing, "; "))
	}
	if len(i.pods) == 0 {
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return i, nil
}

// podSpec builds one pod's complete description. When the household
// configuration was supplied, the pod also gets the compose-identical argv and
// the configuration provisioned at PodConfigPath, so the sandbox-managed and
// compose-managed deployments run the same contract.
func (i *Isolated) podSpec(name string, env map[string]string, unitFlag string, configYAML []byte) sandbox.Spec {
	spec := sandbox.Spec{
		Name:            name,
		Image:           i.opts.Image,
		Level:           sandbox.LevelFast,
		Env:             env,
		NetworkPolicy:   i.opts.NetworkPolicy,
		AllowDomains:    append([]string(nil), i.opts.AllowDomains...),
		EgressProxyAddr: i.opts.EgressProxyAddr,
	}
	if len(configYAML) > 0 {
		spec.Command = []string{
			"--config=" + PodConfigPath,
			"--data-dir=" + DefaultPodDataDir,
			unitFlag,
		}
		spec.Files = []sandbox.File{{
			Path: PodConfigPath,
			Data: append([]byte(nil), configYAML...),
			// The configuration holds no secrets — tokens and keys are named by
			// environment variable only — and the compose deployment mounts the
			// same file world-readable.
			Mode: 0o644,
		}}
	}
	return spec
}

// setDelivery decides how the pod's token travels into it, mirroring the source
// the configuration states so the pod's own resolver — reading the provisioned
// configuration — finds the token exactly where the host did: an environment
// variable stays an environment variable, a token file becomes a 0600 file at
// the same path, and a systemd credential becomes the same credential under a
// synthetic CREDENTIALS_DIRECTORY.
func (p *pod) setDelivery(ref config.SecretRef) error {
	switch {
	case ref.File != "":
		if !strings.HasPrefix(ref.File, "/") {
			return fmt.Errorf("bot token file %q must be an absolute path to be provisioned into a pod", ref.File)
		}
		p.tokenFile = ref.File
	case ref.Env != "":
		p.tokenEnv = ref.Env
	case ref.Credential != "":
		p.tokenCred = ref.Credential
	default:
		return errors.New("bot token has no stated source and no credential name")
	}
	return nil
}

// specFor completes a pod's spec with its token, resolved now. Create and
// Recreate call it; nothing else sees the value.
func (i *Isolated) specFor(p *pod) (sandbox.Spec, error) {
	token, err := p.token()
	if err != nil {
		return sandbox.Spec{}, fmt.Errorf("resolving bot token for pod %s: %w", p.name, err)
	}
	spec := p.base
	env := make(map[string]string, len(p.base.Env)+2)
	for k, v := range p.base.Env {
		env[k] = v
	}
	spec.Env = env
	spec.Files = append([]sandbox.File(nil), p.base.Files...)

	switch {
	case p.tokenFile != "":
		spec.Files = append(spec.Files, sandbox.File{
			Path: p.tokenFile,
			Data: []byte(token.Value()),
			Mode: 0o600,
		})
	case p.tokenCred != "":
		spec.Env["CREDENTIALS_DIRECTORY"] = podCredentialsDir
		spec.Files = append(spec.Files, sandbox.File{
			Path: podCredentialsDir + "/" + p.tokenCred,
			Data: []byte(token.Value()),
			Mode: 0o600,
		})
	default:
		spec.Env[p.tokenEnv] = token.Value()
	}
	return spec, nil
}

// podName builds a sandbox name from the parts, restricted to the [A-Za-z0-9_-]
// alphabet sandbox.Spec.Name requires. Anything else becomes a hyphen, which
// makes the mapping lossy: distinct member ids can produce one name, and unique
// ids therefore do not imply unique names. newIsolated proves the names it
// builds are distinct before any pod exists.
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
//
// What it observes is the container, and only the container. StateReady here
// means the pod's process is running — docs/IMPLEMENTATION.md §9's "health =
// process up" — and never that the unit inside it answered anything: a pod whose
// image starts and whose unit then wedges is reported ready. Observing the unit
// itself would need a readiness surface inside the pod that does not exist; the
// backend's Inspect returns liveness and nothing else.
//
// Each tick takes the pod's operation lock with TryLock and stands aside when a
// rolling update holds it: a pod that is down because Roll is deliberately
// replacing it is not a pod that needs rescuing, and the backend requires
// lifecycle calls on one id to be serialised anyway.
func (i *Isolated) runPod(ctx context.Context, p *pod) {
	defer i.wg.Done()
	bo := newBackoff(i.opts.RestartBackoff, i.opts.MaxRestartBackoff)
	i.tracker.set(p.key, StateStarting)

	up := false
	var readyAt time.Time
	didReset := false

	for {
		if i.stoppingOr(ctx) {
			return
		}
		if !p.opMu.TryLock() {
			// A rolling update owns the pod; take a fresh look once it lets go.
			up = false
			didReset = false
			if !i.sleep(ctx, i.opts.PollInterval) {
				return
			}
			continue
		}

		if !up {
			err := i.ensureRunning(ctx, p)
			p.opMu.Unlock()
			if err != nil {
				if i.stoppingOr(ctx) {
					return
				}
				i.logger.Warn("supervisor: pod failed to start", "pod", p.name, "error", err)
				i.tracker.fail(p.key, err)
				if !i.sleep(ctx, i.backoffDelay(p.key, bo)) {
					return
				}
				i.tracker.set(p.key, StateStarting)
				continue
			}
			i.tracker.set(p.key, StateReady)
			up = true
			readyAt = i.opts.Now()
			didReset = false
			if !i.sleep(ctx, i.opts.PollInterval) {
				return
			}
			continue
		}

		st, err := i.inspect(ctx, p.name)
		p.opMu.Unlock()
		if err != nil || !st.Running {
			if i.stoppingOr(ctx) {
				return
			}
			if err == nil {
				err = errors.New("supervisor: pod exited unexpectedly")
			}
			i.logger.Warn("supervisor: pod down, restarting", "pod", p.name, "error", err)
			i.tracker.fail(p.key, err)
			up = false
			if !i.sleep(ctx, i.backoffDelay(p.key, bo)) {
				return
			}
			i.tracker.set(p.key, StateStarting)
			continue
		}
		if !didReset && i.opts.Now().Sub(readyAt) >= i.opts.HealthyReset {
			bo.reset()
			didReset = true
		}
		if !i.sleep(ctx, i.opts.PollInterval) {
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
// stopped. It never purges: the pod's work volume holds the member's lore
// instance, and a restart policy that erased memory would be worse than no
// restart policy.
func (i *Isolated) ensureRunning(ctx context.Context, p *pod) error {
	st, err := i.inspect(ctx, p.name)
	switch {
	case errors.Is(err, sandbox.ErrSandboxNotFound):
		spec, serr := i.specFor(p)
		if serr != nil {
			return serr
		}
		cctx, cancel := i.callContext(ctx)
		defer cancel()
		_, cerr := i.backend.Create(cctx, spec)
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
// them. Pods are stopped, never purged: a pod's work volume holds its member's
// lore instance and outlives every restart, every rolling update and every Stop.
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
			go func(p *pod) {
				defer wg.Done()
				// The operation lock serialises this against a monitor tick or a
				// rolling update mid-step on the same pod.
				p.opMu.Lock()
				defer p.opMu.Unlock()
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

// Roll brings every pod up to the current image by recreating each in turn:
// graceful stop — SIGTERM, so the pod finishes its in-flight turn and locks its
// session exactly as any drain — then recreation from the pod's full spec on the
// current image, then a wait for the new pod to hold running before the next pod
// is touched. One member is briefly unavailable at a time; the household is
// never entirely down. The order is deterministic: members in configuration file
// order, the household group last, so the group's pod — the one every member
// shares — stays on the working old image until every member's own pod has
// proven the new one.
//
// On the first failure Roll stops and returns a *RollError naming the broken
// unit. Pods after it are left untouched on their working old image: a rolling
// update that keeps rolling through failures converts one broken member into a
// broken household, which is the opposite of what rolling is for. The failed
// pod's own monitor keeps trying to bring it back on its usual backoff.
//
// Recreation goes through sandbox.Backend.Recreate, which preserves the pod's
// work volume structurally — no path through it can delete the member's lore.
// Nothing here calls Purge, the one method that does. Only one Roll may run at
// a time, and only on a started supervisor.
func (i *Isolated) Roll(ctx context.Context) error {
	i.mu.Lock()
	started := i.started
	i.mu.Unlock()
	if !started {
		return errors.New("supervisor: rolling update requires a started supervisor")
	}
	if i.stoppingOr(ctx) {
		return errors.New("supervisor: rolling update abandoned: supervisor is stopping")
	}
	if !i.rollMu.TryLock() {
		return errors.New("supervisor: a rolling update is already in progress")
	}
	defer i.rollMu.Unlock()

	for _, p := range i.pods {
		if err := i.rollOne(ctx, p); err != nil {
			return &RollError{Member: p.key.member, Group: p.key.group, Err: err}
		}
		i.logger.Info("supervisor: pod rolled to current image", "pod", p.name)
	}
	return nil
}

// rollOne replaces one pod. It holds the pod's operation lock throughout, so the
// monitor stands aside rather than fighting the replacement.
func (i *Isolated) rollOne(ctx context.Context, p *pod) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	i.tracker.set(p.key, StateStarting)

	// Graceful stop is the drain: the pod's runtime answers SIGTERM by finishing
	// its in-flight turn, locking its sessions and exiting.
	{
		sctx, cancel := i.callContext(ctx)
		err := i.backend.Stop(sctx, p.name)
		cancel()
		if err != nil && !errors.Is(err, sandbox.ErrSandboxNotFound) {
			err = fmt.Errorf("stopping pod %s for recreation: %w", p.name, err)
			i.tracker.fail(p.key, err)
			return err
		}
	}

	{
		spec, err := i.specFor(p)
		if err != nil {
			i.tracker.fail(p.key, err)
			return err
		}
		cctx, cancel := i.callContext(ctx)
		_, err = i.backend.Recreate(cctx, spec)
		cancel()
		if err != nil {
			err = fmt.Errorf("recreating pod %s: %w", p.name, err)
			i.tracker.fail(p.key, err)
			return err
		}
	}

	if err := i.awaitRunning(ctx, p); err != nil {
		i.tracker.fail(p.key, err)
		return err
	}
	i.tracker.set(p.key, StateReady)
	return nil
}

// awaitRunning waits for a freshly recreated pod to be observed running on two
// consecutive polls — running once and then gone is the signature of an image
// that crashes on startup, and moving on after a single sighting would roll that
// crash across the household.
//
// Two sightings of a running container is the whole of the check, and the name
// says so: it catches an image that will not stay up, not a unit that starts and
// then serves nobody. A roll can therefore complete across a household whose new
// image runs and wedges. Catching that would need the pod to report its own
// readiness, which nothing in the image does today.
func (i *Isolated) awaitRunning(ctx context.Context, p *pod) error {
	deadline := i.opts.Now().Add(i.opts.RollTimeout)
	seenRunning := false
	for {
		if !i.sleep(ctx, i.opts.PollInterval) {
			return errors.New("supervisor: stopped while waiting for recreated pod")
		}
		st, err := i.inspect(ctx, p.name)
		switch {
		case err == nil && st.Running && seenRunning:
			return nil
		case err == nil && st.Running:
			seenRunning = true
		case seenRunning:
			// It came up and went straight back down: the new image is broken.
			if err == nil {
				err = errors.New("pod exited immediately after recreation")
			}
			return fmt.Errorf("pod %s failed after recreation: %w", p.name, err)
		}
		if i.opts.Now().After(deadline) {
			return fmt.Errorf("pod %s did not become healthy within %s", p.name, i.opts.RollTimeout)
		}
	}
}

// Health reports every pod's condition from the supervisor's own observations —
// the monitor goroutines write, Health reads a snapshot. It never calls the
// container runtime and never touches anything external, so it is callable before
// Start, after Stop, and while a pod is mid-crash-loop.
//
// In this mode StateReady is an observation of the container, not of the member's
// conversation: it says the pod is running, and a pod that runs while the unit
// inside it is wedged reports it. See runPod. Members who have not
// enrolled appear with StateNotEnrolled: they have no pod, which is a known
// situation, not a failure.
func (i *Isolated) Health(_ context.Context) ([]UnitHealth, error) {
	return i.tracker.snapshot(), nil
}

// Isolated implements Supervisor.
var _ Supervisor = (*Isolated)(nil)
