package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	// PodInvitesPath is where a member's outstanding claim codes are provisioned
	// inside their pod, and the same path the compose deployment mounts them at.
	//
	// It is beside the configuration and deliberately not under /work. /work is the
	// pod's own volume, where the pod keeps the invite store it actually redeems
	// against and marks consumed; writing here instead means the host delivers a
	// seed and never touches what the pod has recorded. The pod imports it on the
	// way up — see PodCommand's --invites and cmd/kenward.
	PodInvitesPath = "/etc/kenward/invites.json"
	// PodRevokedPath is where a member's revocation record is provisioned inside
	// their pod, and the same path the compose deployment mounts it at.
	//
	// Beside the invites and, like them, deliberately not under /work: the host
	// delivers a fact and never touches what the pod has recorded. The pod reads it
	// on the way up and applies it to its own state file — see PodCommand's
	// --revoked and cmd/kenward.
	PodRevokedPath = "/etc/kenward/revoked.json"
	// PodGroupFlag selects the household group's unit on a pod's command line.
	PodGroupFlag = "--group"
	// podCredentialsDir is the synthetic CREDENTIALS_DIRECTORY a pod is given
	// when one of its secrets' sources is a systemd credential: the credential is
	// provisioned as a 0600 file there, mirroring on the inside exactly what
	// systemd did on the outside, so the pod's own resolver finds it without
	// the configuration changing shape.
	podCredentialsDir = "/run/kenward/credentials"
	// podSecretUID and podSecretGID own every secret file provisioned into a pod.
	//
	// They are the fixed non-root identity the image runs as — `USER
	// nonroot:nonroot`, uid=gid=65532, baked into the distroless base the
	// Dockerfile's final stage uses and inherited by any image derived from it,
	// which is the supported way to add `lore`. Without them keel provisions the
	// file root-owned, and a 0600 file owned by root is a file the pod's own
	// process cannot open:
	//
	//	kenward: /etc/kenward/kenward.yaml cannot be served (problem):
	//	  - members[1].passphrase_file: /run/kenward/eve.pass: permission denied
	//
	// which is the whole of the file and credential sources broken in this
	// deployment path while the environment one worked, found by running the mode
	// against real podman. Loosening the mode instead would be worse and would not
	// even work: config.Secrets refuses a secret file group- or world-readable, so
	// 0644 trades one refusal for another and gives every process in the container
	// the token on the way past.
	podSecretUID = 65532
	podSecretGID = 65532
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
	// InviteSeedDir is the host directory holding one file of outstanding claim
	// codes per member, written by `kenward invite`. When set, a member's own file
	// is provisioned into that member's pod at PodInvitesPath at create time, and
	// the pod imports it into the invite store on its own volume.
	//
	// This is the only thing that makes D-023 reachable through this deployment
	// path. `kenward invite` mints on the host; the claim is redeemed inside the
	// pod, against a store on the pod's volume that the host cannot see and must
	// not write. Without the seed the operator hands over a code the pod has never
	// heard of, and enrolment answers a stranger with the silence it owes one.
	//
	// What travels is hashed and this member's alone. A record is a PBKDF2 digest
	// of an 80-bit code (see internal/enrol), so it is not redeemable in itself,
	// and the file holds no other member's records. Nothing travels the other way:
	// this supervisor reads a host file and writes into a fresh container, and no
	// path here reads anything out of a member's volume.
	//
	// Empty provisions nothing, which is what a household that mints no invites
	// wants and what every already-enrolled member gets.
	InviteSeedDir string
	// RevocationDir is the host directory holding one record per revoked member,
	// written by `kenward revoke`. When set, a member's own record is provisioned
	// into that member's pod at PodRevokedPath at create time, and the pod clears
	// the matching binding from the state file on its own volume on the way up.
	//
	// It is InviteSeedDir's other direction and exists for the harder half of the
	// same crossing. A claim is redeemed inside the pod and the binding is written
	// there, so `kenward revoke` on the host has nothing to clear and used to report
	// success while the pod went on serving the revoked account. The host cannot
	// reach into that volume to fix it — that is the property this mode is for — so
	// the fact travels the one way anything travels here, and lands when the pod is
	// next created. That delay is real and `kenward revoke` says so.
	//
	// Empty provisions nothing, which is what a household that has revoked nobody
	// has.
	RevocationDir string
	// ConfigFile is the path to the household's kenward.yaml on the host. When
	// set, its contents are provisioned into every pod at PodConfigPath and the
	// pod is started with the compose-identical argv
	// (`--config=… --data-dir=… --member=ID | --group`). When empty, unit
	// selection travels as environment only and the image is expected to carry
	// or locate its own configuration.
	//
	// The path is kept, not just its contents: provisioning happens at Create and
	// Recreate time only, so a pod older than this file is holding a configuration
	// the operator has since replaced, and recreateStalePods needs the path to ask.
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
	// ImageStatePath is the host file recording which Image this supervisor last
	// brought the household up on. Start compares it with Image and, when the two
	// differ, rolls every pod onto the current one before the monitors run — see
	// Start. Empty disables the check, and then nothing ever rolls: pods keep
	// whatever image they were created with until an operator intervenes.
	//
	// A file is used because the pod's image is not observable: sandbox.Status
	// reports liveness and endpoints, not what the container was built from. The
	// closest honest question this supervisor can ask is "is this the image I
	// last brought them up on", and that has to be written down to be asked.
	ImageStatePath string
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
	// base is the pod's spec without its secrets. specFor completes it at
	// Create and Recreate time, so no secret's value ever rests on a struct a
	// logger could print, and a rotated token file or credential is picked up
	// by the next recreation without a supervisor restart.
	base sandbox.Spec
	// secrets are the values this pod cannot run without: its own bot token, for a
	// member's pod the passphrase that unwraps that member's key, and the API key of
	// every endpoint this unit's own tier chain reaches.
	//
	// No sibling's token and no sibling's passphrase, ever — that is the mode. An
	// endpoint key is the one thing here that can be shared, because it is one
	// provider account rather than one member's secret; see endpointSecrets, which
	// says exactly what that costs.
	secrets []podSecret
	// inviteSeed is the host file of this member's outstanding claim codes, or
	// empty. It is read at Create and Recreate time and never held, the same way
	// a secret is, so a code minted after this supervisor started still reaches a
	// pod that is recreated afterwards. The group's pod never has one: D-023 puts
	// the claim on the member's own bot, so the household's has no claimer.
	inviteSeed string
	// revocation is the host file recording that this pod's member has been
	// revoked, or empty. Read at Create and Recreate time exactly like inviteSeed,
	// which is what makes a revocation recorded while the household runs reach the
	// pod on its next creation — the only moment the host can tell it anything.
	revocation string
	// configFile is the host's kenward.yaml — IsolatedOptions.ConfigFile — or empty
	// when none was supplied. Unlike the two above it is the same file for every pod
	// and the group's has it too, and it is here rather than read off the supervisor
	// for one reason: stale asks about the host files a pod may not be holding, and
	// this is one of them. See stale, and the defect that put it there.
	configFile string
	// enrolled records whether this pod's member had claimed their invite when the
	// supervisor read the configuration. It changes nothing about the pod — a
	// claim-only pod is started from the same spec, because the process inside
	// decides for itself which of the two it is — and only what a running one is
	// reported as. See upState.
	enrolled bool

	// opMu serialises lifecycle operations on this pod, which the sandbox
	// backend requires per id. The monitor takes it with TryLock and stands
	// aside when it cannot — a rolling update owning the pod must not race a
	// monitor trying to "rescue" the container it is deliberately replacing.
	opMu sync.Mutex
}

// upState is what a running pod means for the unit inside it.
//
// For an enrolled member, and for the group, it is StateReady with the meaning that
// constant carries here: the container is up, and nothing finer was observed. For a
// member who has not claimed their invite it is StateNotEnrolled, which is the same
// answer the pod's own process gives about itself while it sits claim-only (see
// Single) — the pod is running and waiting for a code, and calling that "ready" would
// tell an operator the member is being served when nobody has arrived yet.
//
// It applies only to a pod observed running. A claim-only pod that will not start is
// StateFailed with its error and its restart count, exactly like any other: "waiting
// for someone to accept an invitation" and "this container is crash-looping" are
// different facts and an operator has to be able to tell them apart. That is the
// whole reason enrolment is no longer a reason to have no pod at all — before this,
// the two were indistinguishable, because a pod that was never created and a pod that
// could not start both reported StateNotEnrolled and neither existed.
//
// What it cannot see is a claim that lands after the supervisor started. The code is
// redeemed inside the pod, against the pod's own invite store on its own volume, so
// the host never learns of it and goes on reporting StateNotEnrolled for a member who
// is by then being served, until the next `kenward run` re-reads the configuration.
func (p *pod) upState() State {
	if p.enrolled {
		return StateReady
	}
	return StateNotEnrolled
}

// Isolated runs one pod per configured member — claimed or not, see upState and
// D-023 — plus one for the household group, via
// keel/sandbox. Each pod holds its own bot token, its own lore instance behind its
// own LORE_HOME, and its own key, wrapped under its own passphrase. Inside every pod
// runs the same assistant.Unit that simple mode runs as a goroutine — the pod
// boundary is the only difference, and no unit can tell.
//
// What this host process holds, stated plainly because it is the property the mode
// exists to provide. It holds no member's wrapped key, no member's lore instance and
// no member's plaintext, ever: those live in the pods' work volumes and the pods'
// memory. What it does do, at Create and Recreate time and only there, is read each
// member's bot token and each member's passphrase in order to hand them to that one
// member's pod, and drop them again — a supervisor that starts a process with a
// secret must hold that secret for the length of the call, and there is no way to
// deliver one to a child without it. So the honest claim is not "the host never sees
// a member's secret" but three narrower ones that are true: the host retains nothing,
// no member's secret is ever given to another member's pod or to the group's, and the
// passphrase it forwards unwraps a key it does not have. Simple mode's one operator
// passphrase over one shared store keeps none of those.
//
// Restarts are per-pod with per-pod backoff, so one member crash-looping burns
// their own schedule and never takes the household down. A pod is only ever
// created once and thereafter stopped, started and recreated — never purged:
// Purge takes the pod's work volume with it, and the work volume is where the
// member's lore lives.
// After the host binary updates, Roll recreates pods on the new image, one at a
// time — see Roll for the constraints that shape it, and rollIfImageChanged for
// what makes Start fire it.
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

	// prove reads a secret now — a household must never start half-configured — and
	// drops the value: pods resolve again at create time, through the same closure.
	prove := func(what string, ref config.SecretRef, get func() (config.Secret, error)) (podSecret, error) {
		if _, err := get(); err != nil {
			return podSecret{}, err
		}
		return newPodSecret(what, ref, get)
	}

	var missing []string
	for _, mc := range i.cfg.Members {
		m := mc.Domain()
		if m.SharedOnly {
			// No pod, and this is the only place that decides so. A pod is a
			// member's own bot, their own key and their own lore volume, and this
			// member has none of the three: there is nothing for a container to
			// hold and nothing for it to serve. Their conversations — the group's
			// and their own chat with kenward — both run in the group pod, on the
			// household's bot, which is where the household's memory already is.
			//
			// Skipped without a health record on purpose, unlike an unenrolled
			// member, who has a pod that is waiting for them. This member is not
			// waiting for anything; a permanently absent unit reported every time
			// `doctor` runs would be a fault nobody can clear.
			continue
		}
		// A member who has not claimed their invite gets a pod like everybody else,
		// and that is D-023 rather than a convenience: in this mode a member's bot
		// exists *before* they claim, the operator hands over a code, and the member
		// redeems it in a conversation with their own bot. Skipping them here left
		// that sequence reachable through the compose path — which starts a service
		// per member regardless — and unreachable through `kenward run`, so the one
		// deployment path that is supposed to manage itself was the one that could
		// not onboard anybody.
		//
		// Nothing about the pod differs. The process inside decides for itself
		// whether it has a member to serve or a code to wait for (see Single), from
		// the same configuration, with the same two secrets — both of which
		// internal/config already requires of an unenrolled member for exactly this
		// reason, since the claim provisions their key under their own passphrase in
		// their own pod, with nobody at a terminal to be asked for one.
		mc := mc
		// A member's pod needs two secrets and no others: the bot nobody else
		// speaks on, and the passphrase that unwraps that member's key and no
		// other member's. Both are proved here so that a household with one
		// unreadable secret is refused rather than started with one pod that
		// crash-loops out of sight.
		token, terr := prove("bot token", mc.BotTokenRef(),
			func() (config.Secret, error) { return mc.BotToken(secrets) })
		pass, perr := prove("session passphrase", mc.PassphraseRef(),
			func() (config.Secret, error) { return mc.Passphrase(secrets) })
		// And the provider keys this member's own tier chain reaches — see
		// endpointSecrets. Not "and no others": that is the whole of what a member's
		// pod may hold, and an endpoint key is shared with every sibling whose chain
		// reaches the same endpoint, because it is one provider account.
		keys, kerrs := endpointSecrets(i.cfg, config.UnitScope{Member: string(m.ID)}, secrets)
		if terr != nil || perr != nil || len(kerrs) > 0 {
			for _, err := range append([]error{terr, perr}, kerrs...) {
				if err != nil {
					missing = append(missing, fmt.Sprintf("member %s: %v", m.ID, err))
				}
			}
			continue
		}
		k := unitKey{member: m.ID}
		name := podName(opts.NamePrefix, "member-"+string(m.ID))
		if err := claimName(name, "member "+string(m.ID)); err != nil {
			return nil, err
		}
		p := &pod{
			key:        k,
			name:       name,
			enrolled:   m.Enrolled(),
			secrets:    append([]podSecret{token, pass}, keys...),
			inviteSeed: perMemberPath(opts.InviteSeedDir, m.ID),
			revocation: perMemberPath(opts.RevocationDir, m.ID),
			configFile: opts.ConfigFile,
			base: i.podSpec(name, map[string]string{
				EnvMember:   string(m.ID),
				EnvLoreHome: opts.LoreHome,
				EnvDataDir:  DefaultPodDataDir,
			}, "--member="+string(m.ID), configYAML),
		}
		i.pods = append(i.pods, p)
		i.tracker.add(k)
	}
	if i.cfg.Household.GroupChatID != 0 {
		cfgSnapshot := i.cfg
		// The group pod gets a token and deliberately no passphrase. It serves the
		// shared space and holds no member's key, so a passphrase here would be a
		// secret that unwraps nothing — and a household passphrase sitting in the
		// one pod every member talks to is exactly the shape of thing this mode
		// exists to keep out of it.
		token, err := prove("bot token", cfgSnapshot.BotTokenRef(),
			func() (config.Secret, error) { return cfgSnapshot.BotToken(secrets) })
		// The group conversation routes on household.tiers, so its pod needs the keys
		// that chain reaches for exactly the reason a member's does.
		keys, kerrs := endpointSecrets(i.cfg, config.UnitScope{Group: true}, secrets)
		if err != nil || len(kerrs) > 0 {
			for _, e := range append([]error{err}, kerrs...) {
				if e != nil {
					missing = append(missing, fmt.Sprintf("group: %v", e))
				}
			}
		} else {
			k := unitKey{group: true}
			name := podName(opts.NamePrefix, "group")
			if err := claimName(name, "the household group"); err != nil {
				return nil, err
			}
			i.pods = append(i.pods, &pod{
				key:  k,
				name: name,
				// The group has nobody to enrol; its pod is ready when it runs.
				enrolled:   true,
				secrets:    append([]podSecret{token}, keys...),
				configFile: opts.ConfigFile,
				base: i.podSpec(name, map[string]string{
					EnvGroup:    "1",
					EnvLoreHome: opts.LoreHome,
					EnvDataDir:  DefaultPodDataDir,
				}, PodGroupFlag, configYAML),
			})
			i.tracker.add(k)
		}
	}
	if len(missing) > 0 {
		// Refusing to start half-configured beats starting a household where one
		// member's pod silently never comes up.
		return nil, fmt.Errorf("supervisor: this household cannot be started as configured: %s",
			strings.Join(missing, "; "))
	}
	if len(i.pods) == 0 {
		return nil, fmt.Errorf("supervisor: %w", ErrNoUnits)
	}
	return i, nil
}

// PodCommand is the command line a pod is started with, for the unit
// `--member=ID` or `--group` names. Both isolated deployment paths use it: this
// supervisor puts it in the pod's sandbox.Spec, and deploy/compose.isolated.yml
// writes the same list in every service's `command:`.
//
// It begins with the subcommand, and that is the whole reason this is a function
// rather than three strings written out twice. The image's ENTRYPOINT is the bare
// binary and `run` lives in its CMD (see the Dockerfile, which says so), so
// anything supplied here REPLACES `run` rather than adding to it. A list that
// starts with a flag reaches cmd/kenward's dispatch as the command name, and the
// pod dies before it reads anything:
//
//	$ podman logs sbx-kenward-member-david
//	kenward: unknown command "--config=/etc/kenward/kenward.yaml"
//	(exit 2, on every restart, forever)
//
// TestPodCommandIsSomethingThisBinaryRuns in cmd/kenward puts this list through
// the real dispatcher, because that is the layer that decides whether it is a
// command at all.
// A member's pod also gets --invites, naming the claim codes provisioned into it, and
// --revoked, naming the revocation record, and the group's deliberately gets neither:
// D-023 puts the claim conversation on the member's own bot, so the household's pod has
// no claimer, nothing to import and no binding of its own to clear. A member's pod whose
// seed or record was never provisioned finds no file, which reads as no invites
// outstanding and no revocation rather than as a failure.
func PodCommand(unitFlag string) []string {
	argv := []string{
		"run",
		"--config=" + PodConfigPath,
		"--data-dir=" + DefaultPodDataDir,
	}
	if unitFlag != PodGroupFlag {
		argv = append(argv, "--invites="+PodInvitesPath, "--revoked="+PodRevokedPath)
	}
	return append(argv, unitFlag)
}

// perMemberPath names one member's file under dir, for the two host directories that
// hold one file per member: outstanding invites and revocations. It must agree with
// what `kenward invite` and `kenward revoke` write — cmd/kenward owns those directory
// names and writes `<id>.json` into them — and with what the compose deployment mounts
// by hand.
func perMemberPath(dir string, id domain.MemberID) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, string(id)+".json")
}

// podFile reads one of a pod's per-member host files and turns it into the file to
// provision at podPath.
//
// A missing file is not an error: most members have no invite outstanding and almost
// none have been revoked, and a household that has done neither has no directory at
// all. An unreadable one is, because the alternatives are a pod that starts and
// silently cannot be claimed, and a pod that starts and silently goes on serving a
// revoked account.
//
// what names the file in that error and nowhere else.
func podFile(what, hostPath, podPath string) (sandbox.File, bool, error) {
	if hostPath == "" {
		return sandbox.File{}, false, nil
	}
	data, err := os.ReadFile(hostPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return sandbox.File{}, false, nil
	case err != nil:
		return sandbox.File{}, false, fmt.Errorf("reading %s from %s: %w", what, hostPath, err)
	}
	// 0600 and this pod's own uid, like a secret, though neither of these is one.
	// Invite digests are unredeemable and a revocation is a name and a time; knowing
	// which invites are outstanding is still knowing where to aim, and each of these
	// files has exactly one reader.
	return sandbox.File{
		Path: podPath,
		Data: data,
		Mode: 0o600,
		UID:  podSecretUID,
		GID:  podSecretGID,
	}, true, nil
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
		spec.Command = PodCommand(unitFlag)
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

// podSecret is one value a pod cannot run without, together with how it travels in.
//
// Exactly one of env, file and cred is set, mirroring the source the configuration
// states for that secret: an environment variable stays an environment variable, a
// `*_file` becomes a 0600 file at the same path, and a systemd credential becomes the
// same credential name under a synthetic CREDENTIALS_DIRECTORY. Mirroring is what lets
// the pod's own resolver — reading the same provisioned configuration — find the value
// exactly where the host found it, with no second mechanism to keep in step.
//
// resolve is called at Create and Recreate time and never before, so the value never
// rests on this struct.
type podSecret struct {
	// what names the secret for an error message. Never any part of the value.
	what    string
	resolve func() (config.Secret, error)
	env     string
	file    string
	cred    string
}

// newPodSecret decides how one secret travels into a pod.
func newPodSecret(what string, ref config.SecretRef, resolve func() (config.Secret, error)) (podSecret, error) {
	s := podSecret{what: what, resolve: resolve}
	switch {
	case ref.File != "":
		if !strings.HasPrefix(ref.File, "/") {
			return podSecret{}, fmt.Errorf("%s file %q must be an absolute path to be provisioned into a pod", what, ref.File)
		}
		s.file = ref.File
	case ref.Env != "":
		s.env = ref.Env
	case ref.Credential != "":
		s.cred = ref.Credential
	default:
		return podSecret{}, fmt.Errorf("%s has no stated source and no credential name", what)
	}
	return s, nil
}

// endpointSecrets are the provider keys one unit's pod has to be given: the key of
// every endpoint its own tier chain can reach, and no other.
//
// It exists because config.secretRefs already scopes endpoint keys this way, and the
// pod validates its own configuration on the way up. A member whose chain reaches a
// cloud endpoint is therefore *required* to hold that provider's key — and until this,
// a supervisor-started pod was given exactly two secrets, so that member's pod refused
// its own configuration at startup and never ran. The compose path let an operator add
// the variable by hand; this path had no equivalent, so `tiers: [local, cloud]` was
// simply unavailable under `kenward run`.
//
// EndpointsForUnit is the same function validation scopes with, deliberately: two
// implementations of "which endpoints does this unit reach" would eventually disagree,
// and the way that failure presents is a pod that validates clean and then cannot
// route, or one that holds a key its chain forbids it to use.
//
// # What a pod learns, stated plainly
//
// An endpoint key is not a per-member secret and cannot be made into one. It belongs to
// the provider account the household pays for, so if david's chain and jordan's chain
// both reach `openrouter`, both pods hold that one key and each could spend the other's
// budget on it. That is a real reduction from "a pod holds its own secrets and no
// sibling's", and it is inherent rather than an oversight: sharing a provider account is
// what the configuration said. What still holds is the narrower claim: a pod is given a
// key only where its own chain reaches the endpoint, so a local-only member's pod holds
// none at all, and no pod ever holds a key for a tier it may not route to. A household
// that wants two members not to share a key gives them two endpoints with two
// `api_key_env`s, and each pod then holds only its own.
//
// A key resolved from a systemd credential travels as a credential, a file as a file, an
// environment variable as a variable — newPodSecret's mirroring, unchanged. An endpoint
// that supplied no key at all is skipped rather than refused, because the household's own
// inference machine needs none; a *stated* source that cannot be read is a fault, which is
// exactly what EndpointConfig.APIKey already distinguishes and what validation reports.
func endpointSecrets(cfg *config.Config, scope config.UnitScope, secrets *config.Secrets) ([]podSecret, []error) {
	var (
		out  []podSecret
		errs []error
		seen = make(map[[3]string]bool)
	)
	for _, ec := range cfg.EndpointsForUnit(scope) {
		ec := ec
		what := fmt.Sprintf("the API key for endpoint %q", ec.Name)
		sec, err := ec.APIKey(secrets)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
			continue
		}
		if !sec.IsSet() {
			continue // this endpoint needs no key, which is the usual local case.
		}
		s, err := newPodSecret(what, ec.APIKeyRef(), func() (config.Secret, error) { return ec.APIKey(secrets) })
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Two endpoints on one variable is legitimate — internal/config says so where
		// it dedups the same references — and provisioning it twice would resolve the
		// same value twice for one slot in the pod's environment.
		k := [3]string{s.env, s.file, s.cred}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out, errs
}

// specFor completes a pod's spec with its secrets, resolved now. Create and
// Recreate call it; nothing else sees the values.
func (i *Isolated) specFor(p *pod) (sandbox.Spec, error) {
	spec := p.base
	env := make(map[string]string, len(p.base.Env)+len(p.secrets)+1)
	for k, v := range p.base.Env {
		env[k] = v
	}
	spec.Env = env
	spec.Files = append([]sandbox.File(nil), p.base.Files...)

	// Read now, not at construction, so a code minted or a revocation recorded while
	// the household is running reaches this member the next time their pod is created
	// — which for a member who has not claimed is every restart and every rolling
	// update, and costs nothing, because a claim-only pod is by definition serving
	// nobody. For a revocation it is the only delivery there is, which is why
	// `kenward revoke` tells the operator to restart.
	for _, f := range []struct{ what, host, pod string }{
		{"outstanding invites", p.inviteSeed, PodInvitesPath},
		{"the revocation record", p.revocation, PodRevokedPath},
	} {
		switch file, ok, err := podFile(f.what, f.host, f.pod); {
		case err != nil:
			return sandbox.Spec{}, fmt.Errorf("preparing pod %s: %w", p.name, err)
		case ok:
			spec.Files = append(spec.Files, file)
		}
	}

	for _, s := range p.secrets {
		v, err := s.resolve()
		if err != nil {
			return sandbox.Spec{}, fmt.Errorf("resolving %s for pod %s: %w", s.what, p.name, err)
		}
		switch {
		case s.file != "":
			spec.Files = append(spec.Files, sandbox.File{
				Path: s.file,
				Data: []byte(v.Value()),
				Mode: 0o600,
				UID:  podSecretUID,
				GID:  podSecretGID,
			})
		case s.cred != "":
			spec.Env["CREDENTIALS_DIRECTORY"] = podCredentialsDir
			spec.Files = append(spec.Files, sandbox.File{
				Path: podCredentialsDir + "/" + s.cred,
				Data: []byte(v.Value()),
				Mode: 0o600,
				UID:  podSecretUID,
				GID:  podSecretGID,
			})
		default:
			spec.Env[s.env] = v.Value()
		}
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
//
// Before the monitors run it rolls the household onto the current image when the
// image has changed since the last start — see rollIfImageChanged, which is where
// the host's own self-update finally reaches the pods — and recreates the pods
// whose provisioned configuration, invites or revocations they may not have; see
// recreateStalePods, which is what makes "restart kenward" true.
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
	// The pods are counted onto the WaitGroup here, under the lock that has just
	// found draining open, rather than one at a time in the launch loop below.
	// Down there the Add sits after a roll and a stale-pod recreation, either of
	// which can take as long as the backend does — and a Stop arriving in that
	// window closes draining, stops the pods and reaches wg.Wait() before a single
	// Add has happened. Adding to a WaitGroup another goroutine is already waiting
	// on is the misuse the race detector reports; what it means here is a pod
	// monitor still being launched while shutdown believes it has waited for them
	// all. i.pods is fixed at construction, so its length is known now.
	i.wg.Add(len(i.pods))
	i.mu.Unlock()

	i.logStartup()
	i.rollIfImageChanged(ctx)
	i.recreateStalePods(ctx)

	for _, p := range i.pods {
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
			i.logger.Info("supervisor: pod for member, claim-only until they redeem their code",
				"member", string(m.ID), "tiers", m.Tiers)
		}
	}
	if i.cfg.Household.GroupChatID != 0 {
		i.logger.Info("supervisor: pod for household group", "tiers", i.cfg.Household.Tiers)
	}
	i.logger.Info("supervisor: started",
		"mode", privacy.ModeIsolated.String(),
		"privacy", privacy.Statement(privacy.ModeIsolated))
}

// rollIfImageChanged brings the pods onto the current image when they are not
// already on it. It is the whole of the wiring behind docs/IMPLEMENTATION.md §9's
// "isolated mode updates pods one member at a time", and it runs here — in Start,
// before the monitors — for want of any other moment that can observe the change.
//
// What can be observed is narrow. keel's Inspect reports whether a container runs,
// never what it was built from, so "are the pods stale" cannot be asked of the
// runtime. And the host does not learn it is a new build at the instant it becomes
// one: it self-updates by swapping the binary and exiting, and the process that
// notices is the next one, which is this one. So the question asked is the one this
// process can answer honestly — is the image I would start pods from the image I
// recorded last time — and the answer is written down in ImageStatePath. Without
// this the household node upgrades itself and every member's pod keeps serving from
// the previous image indefinitely, which is worse than not updating at all: the
// operator has every reason to believe the update completed.
//
// ensureRunning cannot do this job. It starts a pod that exists and creates one that
// does not; it never replaces a running container, and it must not — that is the
// restart path, and a restart that recreated pods would roll a new image out to the
// whole household at once on the first crash.
//
// Failures are logged and never fatal. Roll stops at the first broken pod and leaves
// every later one on its working old image, which is the point of rolling; this
// leaves the recorded image unchanged too, so the next start tries again rather than
// recording a state the household is not in. The household serves throughout, on
// whichever mixture of images it ended up with — §9's rule that nothing in the
// update path may stop kenward serving outranks arriving at one version.
// recreateStalePods replaces, once at Start and before the monitors run, the member
// pods whose provisioned per-member files they may not be holding.
//
// It is what makes `kenward invite` and `kenward revoke` telling the operator to
// restart kenward a true instruction rather than a comforting one. Both write a file
// on the host that reaches a pod only when the pod is created, and a restart does not
// create anything: ensureRunning starts a container that exists — deliberately, see
// rollIfImageChanged for why it must not do more — so before this, an operator who
// revoked a member, was told to restart, and restarted, got a household in which that
// member's pod came back up on the container-layer `/etc/kenward` it was built with,
// never saw the revocation, and went on serving the account. Found by running the mode
// against real podman; nothing in the module could see it, because a fake backend has
// no container layer to be stale.
//
// Which pods, and why only those. A pod with a revocation record on the host is
// recreated because that record is the entire delivery mechanism for an action whose
// point is to take effect. An unenrolled member's pod with a seed is recreated for the
// same reason and at the same price D-023 already pays: it is serving nobody, so
// replacing it interrupts nothing. A pod older than the household's kenward.yaml is
// recreated because provisioning is the only delivery there is for that file too, and
// that one reaches the group's pod as well as the members'. Every other pod is started,
// not replaced.
//
// What a recreate costs here, since one of those three can now fire on a pod that is
// serving somebody. Nothing in flight is dropped: this runs once in Start, before the
// monitors, and Stop has already stopped every pod on the way out of the previous
// `kenward run` — so in the restart the operator was told to perform there is no turn
// to lose. Where a supervisor died without draining and left containers up, replace
// stops the pod first and that stop is a SIGTERM the pod's runtime answers by finishing
// its turn and locking its sessions. What is deliberately absent is a watcher: nothing
// recreates a pod because a file changed under a running household. The operator edits
// and restarts, which is what §8 tells them to do, and the recreate happens at the one
// moment the household is already down.
//
// When one of those is actually rebuilt. A revoked member's record is never deleted —
// nothing can know when the pod has consumed it — so the record's mere existence cannot
// be the question, or that member's pod would be rebuilt on every start for as long as
// the record exists. The question asked instead is whether the pod predates the file:
// keel reports each sandbox's creation time (sandbox.Status.CreatedAt, unchanged by
// Start and advanced by Recreate), and a pod created after the file's mtime is already
// holding that file's current contents and is left alone. So the record is delivered
// once and the rebuild happens once.
//
// Every uncertain answer recreates — an unknown creation time, an unreadable file, a
// clock or a filesystem too coarse to separate the two. A needless rebuild costs one
// container and preserves the work volume; a missed one leaves a revoked member served.
// fileNewerThan states each of those cases and why.
//
// Unlike Roll this does not wait for the replacement to come up. A rolling update waits
// because it is proving a new image one unit at a time; here the image is unchanged and
// the pods are independent, so waiting would only hold every other member's monitor
// behind one crash-looping pod's timeout. The pod's own monitor takes it from here.
// Failures are logged and never fatal, for the same reason they are not in Roll.
//
// It runs after a roll rather than instead of one, and asks each pod the same question
// regardless of whether the roll reached it. That is deliberate and it is why the
// question is per-pod: a roll stops at its first failure and leaves every later pod
// untouched, so treating "a roll happened" as "every pod is current" would skip exactly
// the pods a failed roll did not reach. A pod the roll did recreate now has a creation
// time later than its files and is skipped on its own evidence, not on an assumption.
func (i *Isolated) recreateStalePods(ctx context.Context) {
	for _, p := range i.pods {
		// A first pass against an unknown creation time, on the rule that a pod which
		// is not stale even when nothing is known about its age cannot become stale
		// once something is. It skips the Inspect for a pod with none of these files
		// at all — which, now that the household configuration counts, means a
		// deployment that provisions no configuration rather than the common case.
		// recreateOne asks the real question.
		if !p.stale(time.Time{}) {
			continue
		}
		switch err := i.recreateOne(ctx, p); {
		case errors.Is(err, errPodAbsent):
			// Never created, so it will be created from the current files.
		case errors.Is(err, errPodCurrent):
			// Created after every file it would be given: it already holds them.
		case err != nil:
			i.logger.Warn("supervisor: could not recreate pod to give it the current configuration, invites and revocations; it may be serving a revoked account or an outdated kenward.yaml",
				"pod", p.name, "error", err)
		default:
			i.logger.Info("supervisor: pod recreated so it reads the current configuration, invites and revocations", "pod", p.name)
		}
	}
}

// stale reports whether this pod, created at createdAt, may not be holding the
// current contents of the host files provisioned into it.
// createdAt is the pod's sandbox.Status.CreatedAt. See recreateStalePods.
func (p *pod) stale(createdAt time.Time) bool {
	// The household configuration is one of those files, for every pod including
	// the group's, and until this was here it was the one nobody asked about:
	// newIsolated reads kenward.yaml once, podSpec puts the bytes in the spec, and
	// only Create and Recreate write a spec's files. An operator who edited
	// kenward.yaml and restarted got a host supervisor on the new file and pods
	// still serving the old one, with nothing anywhere saying so — host-side
	// `doctor` reads the new file and is satisfied.
	//
	// That is not a cosmetic drift. docs/IMPLEMENTATION.md §8's own first-run recipe
	// ends in exactly this state: the household's lore spaces can only be created
	// inside a pod, so the operator creates them after the pods exist and then writes
	// the minted ids into kenward.yaml — into a file the pods will never read. Every
	// pod looks healthy and every turn that touches memory fails, days later, against
	// the placeholder id the pods were built with.
	//
	// It is compared without createdAtSkew, unlike the two below, because the
	// timing that tolerance exists for is reversed here. A revocation record is
	// written just *before* the restart that must deliver it, so a coarse
	// filesystem rounding its mtime down can hide a genuinely newer record and
	// the window is worth buying. kenward.yaml is written just *before the pods
	// are created from it* — on every first run — so the same window would call
	// a pod stale for holding the very bytes it was built from, and rebuild the
	// whole household twice on `kenward setup` followed by `kenward run`. What
	// the strict comparison gives up is an edit made inside one tick of a
	// two-second-granularity filesystem of a pod's creation, which costs that
	// edit one further restart rather than losing it.
	if fileNewerThan(p.configFile, createdAt, 0) {
		return true
	}
	if p.key.group {
		// The group's pod is given neither of the per-member files: it has no claimer
		// and holds no member's binding.
		return false
	}
	if fileNewerThan(p.revocation, createdAt, createdAtSkew) {
		return true
	}
	return !p.enrolled && fileNewerThan(p.inviteSeed, createdAt, createdAtSkew)
}

// createdAtSkew is how much older than a host file a pod may be observed to be
// and still be treated as possibly predating it.
//
// It exists because the two timestamps being compared are recorded by different
// machinery and only one of them is exact. keel parses the pod's creation time
// verbatim from podman's `Created`, which is RFC3339Nano; the host file's is
// whatever its filesystem could store. ext4 keeps nanoseconds, ext3 keeps whole
// seconds and FAT/exFAT keeps two, and every one of those truncates *downwards*
// — a record written at 10.9 on a two-second filesystem reads back as 10.0, so a
// pod created at 10.5 looks newer than a record that is in fact newer than it.
// Two seconds covers the coarsest of those. Clock skew proper does not arise on
// the path this runs on, because podman's Created and the file's mtime are both
// stamped by this host's clock; a data directory on a remote filesystem with its
// own clock is the case no fixed tolerance can bound, and it errs the same way as
// everything else here.
//
// It is deliberately small. The tolerance is a window after the pod's creation in
// which an older file still counts as newer, so an operator who records a
// revocation and restarts within two seconds buys one extra rebuild on the
// following start and nothing worse. A generous tolerance — a minute, say — would
// undo the whole point for exactly the sequence the operator is told to perform.
const createdAtSkew = 2 * time.Second

// fileNewerThan reports whether the host file at path exists and may be newer
// than a pod created at createdAt — that is, whether that pod may not be holding
// its current contents. skew widens the window in favour of "newer"; see
// createdAtSkew for what it buys, and stale for the one file that passes zero
// because the timing that tolerance exists for runs the other way.
//
// Every uncertain answer is "yes", and that asymmetry is the whole design. A
// needless recreation costs one container rebuild and preserves the work volume
// structurally; a missed one means a revoked member's pod goes on serving the
// account, which is the defect recreateStalePods exists to close. So:
//
//   - A zero createdAt means unknown, not old, and is treated as stale. Zero
//     reaches here from a QemuBackend sandbox made before keel wrote creation
//     markers, from a podman `Created` field that would not parse, and from an
//     Inspect that failed for some reason other than the sandbox being absent.
//     None of those is evidence that the pod is current.
//   - A stat that fails for any reason other than the file not existing is also
//     stale. The file may well be a revocation record; if it is unreadable,
//     replace will fail on it in podFile and say so loudly, which is the outcome
//     to want over quietly leaving a revoked member served.
func fileNewerThan(path string, createdAt time.Time, skew time.Duration) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false
	case err != nil, createdAt.IsZero():
		return true
	}
	return fi.ModTime().After(createdAt.Add(-skew))
}

// errPodCurrent reports that a pod was created after every per-member file the
// host would provision into it, so it is already holding their current contents
// and must not be rebuilt. Not a failure.
var errPodCurrent = errors.New("supervisor: pod already holds the current files")

// recreateOne replaces one pod that already exists and may not be holding its
// current per-member files, and does not wait for it.
func (i *Isolated) recreateOne(ctx context.Context, p *pod) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	// One Inspect answers both questions: whether there is a pod at all, and how
	// old it is. Any failure other than absence leaves CreatedAt zero, which stale
	// reads as unknown and therefore stale — see fileNewerThan.
	st, err := i.inspect(ctx, p.name)
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return errPodAbsent
	}
	if !p.stale(st.CreatedAt) {
		return errPodCurrent
	}
	return i.replace(ctx, p)
}

// replace stops a pod and recreates it from its current spec, which is what picks up
// the files read at Create and Recreate time. The caller holds the pod's operation
// lock.
//
// The stop is the drain: the pod's runtime answers SIGTERM by finishing its in-flight
// turn, locking its sessions and exiting. Recreation goes through
// sandbox.Backend.Recreate, which preserves the pod's work volume structurally — no
// path through it can delete the member's lore.
func (i *Isolated) replace(ctx context.Context, p *pod) error {
	{
		sctx, cancel := i.callContext(ctx)
		err := i.backend.Stop(sctx, p.name)
		cancel()
		if err != nil && !errors.Is(err, sandbox.ErrSandboxNotFound) {
			return fmt.Errorf("stopping pod %s for recreation: %w", p.name, err)
		}
	}
	spec, err := i.specFor(p)
	if err != nil {
		return err
	}
	cctx, cancel := i.callContext(ctx)
	defer cancel()
	if _, err := i.backend.Recreate(cctx, spec); err != nil {
		return fmt.Errorf("recreating pod %s: %w", p.name, err)
	}
	return nil
}

func (i *Isolated) rollIfImageChanged(ctx context.Context) {
	path := i.opts.ImageStatePath
	if path == "" {
		return
	}
	prev, err := os.ReadFile(path)
	recorded := strings.TrimSpace(string(prev))
	switch {
	case err == nil && recorded == i.opts.Image:
		return
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		// Unreadable is not "unchanged": rolling on a bad read costs one round of
		// recreations, and skipping it costs a household stuck on an old image.
		i.logger.Warn("supervisor: could not read the recorded pod image; rolling to be sure",
			"path", path, "error", err)
	}

	i.logger.Info("supervisor: pod image differs from the one these pods were brought up on; rolling one unit at a time",
		"recorded", recorded, "current", i.opts.Image)
	if err := i.Roll(ctx); err != nil {
		i.logger.Error("supervisor: the rolling update did not finish; later pods are still on their previous image and the household keeps serving",
			"error", err)
		return
	}
	if err := writeImageRecord(path, i.opts.Image); err != nil {
		i.logger.Warn("supervisor: the pods are on the current image but recording it failed, so the next start will roll them again",
			"path", path, "error", err)
	}
}

// writeImageRecord notes which image the pods are now on. It is not written
// atomically: the worst a torn write can cause is one redundant roll on the next
// start, and a redundant roll preserves every work volume exactly as a needed one
// does.
func writeImageRecord(path, image string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(image+"\n"), 0o600)
}

// recoverPump contains a panic raised by a pod-supervising goroutine itself —
// the monitor loop in runPod, or a pod's own stop in shutdown — outside
// whatever that goroutine was doing to one pod. One member's pod misbehaving
// is exactly the failure isolated mode exists to contain from the rest of the
// household; a panic reaching all the way to the process would defeat that
// for every pod, not just this one.
//
// This mirrors runner.recoverPump rather than reusing it: Isolated and runner
// are unrelated types (no shared base), so the method can't be called across
// them, but the pattern — log, tracker.fail, tracker.set(StateStopped) — is
// the same one, not a second convention.
func (i *Isolated) recoverPump(k unitKey, what string) {
	rec := recover()
	if rec == nil {
		return
	}
	err := fmt.Errorf("supervisor: %s panicked: %v", what, rec)
	i.logger.Error("supervisor: pod goroutine crashed; the rest of the household keeps running",
		"pump", what, "member", k.member, "group", k.group, "error", err)
	i.tracker.fail(k, err)
	i.tracker.set(k, StateStopped)
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
	defer i.recoverPump(p.key, "pod monitor")
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
			// The unlock is deferred inside this closure, not just called after
			// ensureRunning returns, so a panic from the backend still releases
			// the lock instead of wedging it for every future Roll or Stop on
			// this pod: recoverPump contains the panic, but only this releases
			// what it was holding.
			err := func() error {
				defer p.opMu.Unlock()
				return i.ensureRunning(ctx, p)
			}()
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
			i.tracker.set(p.key, p.upState())
			up = true
			readyAt = i.opts.Now()
			didReset = false
			if !i.sleep(ctx, i.opts.PollInterval) {
				return
			}
			continue
		}

		// Same reasoning as above: the unlock must survive a panicking Inspect.
		st, err := func() (sandbox.Status, error) {
			defer p.opMu.Unlock()
			return i.inspect(ctx, p.name)
		}()
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
		// Under the lock, so that closing draining and Start's wg.Add cannot
		// interleave: either Start got there first and its pods are on the group
		// before the Wait below, or this did and Start returns "already stopped"
		// without adding anything.
		i.mu.Lock()
		close(i.draining)
		i.mu.Unlock()

		var wg sync.WaitGroup
		for _, p := range i.pods {
			wg.Add(1)
			go func(p *pod) {
				defer wg.Done()
				defer i.recoverPump(p.key, "pod stop")
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
// A pod that has never been created is skipped rather than rolled: it is on no
// image, and its own monitor creates it from the current spec.
//
// Recreation goes through sandbox.Backend.Recreate, which preserves the pod's
// work volume structurally — no path through it can delete the member's lore.
// Nothing here calls Purge, the one method that does. Only one Roll may run at
// a time, and only on a started supervisor. Start calls it for a household whose
// image has changed; see rollIfImageChanged.
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
		switch err := i.rollOne(ctx, p); {
		case errors.Is(err, errPodAbsent):
			i.logger.Info("supervisor: no pod to roll; its monitor will create it on the current image", "pod", p.name)
		case err != nil:
			return &RollError{Member: p.key.member, Group: p.key.group, Err: err}
		default:
			i.logger.Info("supervisor: pod rolled to current image", "pod", p.name)
		}
	}
	return nil
}

// errPodAbsent reports that a pod has never been created, so there is no old
// image to roll off. It is not a failure and must never stop a roll.
var errPodAbsent = errors.New("supervisor: pod does not exist")

// rollOne replaces one pod. It holds the pod's operation lock throughout, so the
// monitor stands aside rather than fighting the replacement.
func (i *Isolated) rollOne(ctx context.Context, p *pod) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	// A pod that has never been created is on no image at all, so there is nothing
	// to replace: its monitor creates it from the current spec moments from now.
	// The backend would tolerate a Recreate here and make one, but a first start
	// would then queue every pod's creation behind the next pod's health wait for
	// no gain.
	if _, err := i.inspect(ctx, p.name); errors.Is(err, sandbox.ErrSandboxNotFound) {
		return errPodAbsent
	}

	i.tracker.set(p.key, StateStarting)

	// The stop-and-recreate is replace; what a roll adds is the wait below.
	if err := i.replace(ctx, p); err != nil {
		i.tracker.fail(p.key, err)
		return err
	}

	if err := i.awaitRunning(ctx, p); err != nil {
		i.tracker.fail(p.key, err)
		return err
	}
	i.tracker.set(p.key, p.upState())
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
// inside it is wedged reports it. See runPod. A member who has not claimed their
// invite has a pod like everybody else (D-023) and, while it runs, appears with
// StateNotEnrolled rather than StateReady — waiting on someone to accept an
// invitation, which is a known situation and not a failure. If that pod stops
// running it is StateFailed like any other; see upState for why the two must not
// collapse into one another.
func (i *Isolated) Health(_ context.Context) ([]UnitHealth, error) {
	return i.tracker.snapshot(), nil
}

// Isolated implements Supervisor.
var _ Supervisor = (*Isolated)(nil)
