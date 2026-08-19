package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	keelupdate "github.com/BlueHeisenberg/keel/update"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/dashboard"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/updater"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// runOptions is everything `run` parsed.
type runOptions struct {
	configPath string
	dataDir    string
	selection  unitSelection
	// image is the pod image the isolated host supervisor starts pods from. It has
	// no configuration field: internal/supervisor requires one and offers no
	// default, on the grounds that there is no sensible default for the artifact a
	// household's private conversations run inside.
	image string
	// invites is a file of outstanding claim codes to import into this unit's own
	// invite store on the way up. It is how a code minted on the host reaches the
	// pod that has to redeem it, in both isolated deployment paths; empty
	// everywhere else, and a path that does not exist is simply no invites.
	invites string
	// revoked is a file recording that this unit's member has been revoked, applied
	// to this pod's own enrolment state on the way up. It is how a revocation
	// performed on the host reaches the pod that actually holds the binding, in both
	// isolated deployment paths; empty everywhere else, and a path that does not
	// exist is simply no revocation.
	revoked string
	// memory is the lore store this process has already opened, when it has: an
	// isolated unit opens one up front so that lore's sync daemon and the unit's own
	// reads and writes share a single handle on the pod's home. Nil everywhere else,
	// which leaves the supervisor to open its own as it always did. It is the
	// caller's to close, and startSyncDaemon's stop function is what closes it.
	memory memory.Memory
}

// supervisorFactory builds the thing `run` runs. It is a seam so that argument
// handling, mode selection and the startup summary can be tested without a bot
// token, a container runtime or a network.
type supervisorFactory func(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.Supervisor, error)

func cmdRun(e *env, args []string) int {
	fs := newFlagSet(e, "run", "kenward run [--config PATH] [--data-dir PATH] [--member ID | --group] [--invites PATH]")
	configPath := fs.String("config", "", "path to kenward.yaml (default: $KENWARD_CONFIG, then ./kenward.yaml, then the per-OS config location)")
	dataDir := fs.String("data-dir", "", "override the data directory (default: $KENWARD_DATA_DIR, then data_dir in the configuration)")
	member := fs.String("member", "", "isolated mode only: run this member's unit and nothing else")
	group := fs.Bool("group", false, "isolated mode only: run the household group's unit and nothing else")
	image := fs.String("image", "", "isolated mode only: the pod image the host supervisor starts pods from")
	invites := fs.String("invites", "", "isolated mode only: a file of outstanding claim codes to import into this unit's invite store")
	revoked := fs.String("revoked", "", "isolated mode only: a file recording that this unit's member has been revoked")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("run takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	sel, err := resolveUnitSelection(*member, *group, e.env())
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}

	// Scoped to the unit this process was told to be, so that a pod holding only its
	// own bot token — which is all D-007 lets it hold — is not refused for the
	// household's other tokens being where they belong, in the other pods.
	path := resolveConfigPath(e, *configPath)
	cfg, cfgErr := loadConfigForUnit(path, resolveDataDir(e, *dataDir), e.secrets(), sel.scope())
	if cfgErr != nil {
		fmt.Fprint(e.stderr, renderConfigError(path, cfgErr))
		return exitUsage
	}

	// A selector that does nothing is how someone ends up believing they are
	// isolated when they are not, so this is a refusal rather than a warning — and
	// it applies however the selector arrived.
	if cfg.Mode == config.ModeSimple && sel.single() {
		e.errorf("%s came from %s, and it is for isolated mode only. This configuration says\n"+
			"mode: simple, where one process runs every unit, so there is nothing for it to\n"+
			"select. kenward will not ignore it: a selector that silently does nothing is how a\n"+
			"household comes to believe it is isolated when it is one shared process.",
			sel.flagName(), sel.source)
		return exitUsage
	}
	// A --member naming nobody is caught by the load above, in internal/config, which
	// has to know about it anyway: the scope decides which secrets are demanded, so a
	// selector pointing at nobody would otherwise be a pod that demands none.

	// Before anything reads who is enrolled — the startup summary below included —
	// because a revocation recorded on the host is a fact about this pod's own
	// enrolment state and applying it later would leave every reader between here and
	// there believing a revoked member is still served. See applyRevocation.
	if err := applyRevocation(e, cfg, sel, *revoked); err != nil {
		e.errorf("%v", err)
		return exitFailure
	}

	// Whether this node's memory actually answers is a property of this machine rather
	// than of the file, and a validation that failed on one host would make `doctor`
	// useless for checking a configuration before shipping it. So the question is asked
	// here, on the machine, at the one moment a process commits to serving.
	if code := checkLore(e, cfg, sel); code != exitOK {
		return code
	}

	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, line := range startupSummary(cfg, sel) {
		logger.Info("kenward", line...)
	}

	// Started before the supervisor and stopped after it, so a pod is syncing for the
	// whole of the time it is serving. The store it opens is the one the unit then
	// uses: one handle, one daemon, one home.
	loreClient, stopSync := startSyncDaemon(e, cfg, sel, logger)
	defer stopSync()

	factory := e.supervisors
	if factory == nil {
		factory = defaultSupervisor
	}
	sup, buildErr := factory(e, cfg, runOptions{
		configPath: path,
		dataDir:    cfg.DataDir,
		selection:  sel,
		image:      *image,
		invites:    *invites,
		revoked:    *revoked,
		memory:     loreClient,
	}, logger)
	if buildErr != nil {
		err := buildErr
		switch {
		case errors.Is(err, supervisor.ErrUnsupportedMode):
			e.errorf("this configuration says mode: isolated, and isolated mode needs Linux with\n"+
				"Podman or Docker. This host is %s. kenward will not quietly run in simple mode\n"+
				"instead: a household that asked for sealed memory and got shared memory would\n"+
				"believe something false. Run this on Linux, or choose simple mode deliberately\n"+
				"with `kenward setup`.", e.os())
			return exitUsage
		case errors.Is(err, errNoPassphrase):
			e.errorf("%s", noPassphraseHelp())
			return exitUsage
		case errors.Is(err, supervisor.ErrNoUnits):
			e.errorf("this configuration produces nothing to run: no member has claimed an invite\n" +
				"and no group chat is configured. Mint a code with `kenward invite --name NAME`.")
			return exitUsage
		default:
			e.errorf("%v", err)
			return exitFailure
		}
	}

	// The updater is built after the supervisor because its drain is the
	// supervisor's own Stop, and it is never allowed to prevent kenward starting:
	// a household whose update configuration is wrong should still have a working
	// assistant. A nil *Scheduler is safe to Run and Resume.
	restart := newRestartSignal()
	sched, err := newScheduler(e, cfg, sel, updater.DrainFunc(func(ctx context.Context) error {
		return sup.Stop(ctx)
	}), restart, logger)
	if err != nil {
		logger.Warn("kenward", "event", "update",
			"detail", "auto-update is off for this run; the assistant is unaffected",
			"err", err.Error())
	}

	// Before anything is served: finish whatever update was in flight when this
	// process last stopped, so a swapped binary is committed or rolled back rather
	// than left half-applied with nothing ever deciding its fate. This runs even
	// when the channel is off — it fetches nothing, and an update in flight when
	// updates were turned off still deserves an answer.
	if code, done := resumeUpdate(e, sched, logger); done {
		return code
	}

	// The dashboard comes up beside the household and goes down with it. It is never
	// allowed to prevent kenward starting: a port already taken is a dashboard the
	// operator cannot reach, and a household with no assistant is worse.
	dash := startDashboard(e, cfg, path, logger)

	code := serve(e, sup, sched, restart, logger)
	stopDashboard(e, dash, logger)
	return code
}

// startDashboard brings the admin dashboard up if this household has one. It returns nil
// when the dashboard is off, which is what a configuration that has never mentioned it
// means, and nil is safe to hand to stopDashboard.
func startDashboard(e *env, cfg *config.Config, path string, logger *slog.Logger) *dashboard.Server {
	if !cfg.Dashboard.Enabled {
		return nil
	}
	srv, err := dashboard.New(dashboardDeps(e, path, cfg.DataDir, logger), cfg.Dashboard)
	if err != nil {
		logger.Warn("kenward", "event", "dashboard", "detail", "the admin dashboard is off for this run", "err", err.Error())
		return nil
	}
	if err := srv.Listen(); err != nil {
		logger.Warn("kenward", "event", "dashboard", "detail", "the admin dashboard could not take its port; the household is unaffected", "err", err.Error())
		return nil
	}

	// Printed rather than logged, and printed only when it is needed. A household
	// that has already made an account gets a line saying where the dashboard is; one
	// that has not gets the token, because the alternative is a listener nobody can
	// get into.
	token, err := srv.SetupTokenIfNeeded()
	if err != nil {
		logger.Warn("kenward", "event", "dashboard", "detail", "no setup token could be issued", "err", err.Error())
	}
	if token != "" {
		fmt.Fprint(e.stdout, "\n"+renderSetupToken(token)+"\n")
	}
	logger.Info("kenward", "event", "dashboard",
		"url", srv.URL(),
		"exposure", string(cfg.Dashboard.ExposureOrDefault()),
		"tls", cfg.Dashboard.TLS())

	go func() {
		if err := srv.Serve(); err != nil {
			logger.Warn("kenward", "event", "dashboard", "detail", "the admin dashboard stopped", "err", err.Error())
		}
	}()
	return srv
}

// stopDashboard drains the dashboard on the way down, on a context of its own: the
// signal context is already cancelled by the time this runs, and a shutdown that
// inherits a cancelled context finishes nothing.
func stopDashboard(e *env, srv *dashboard.Server, logger *slog.Logger) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.context()), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("kenward", "event", "dashboard", "detail", "the admin dashboard did not stop cleanly", "err", err.Error())
	}
}

// startSyncDaemon opens this pod's lore store and runs lore's sync daemon on it for as
// long as this unit serves. It returns the client the unit is to use — so that the
// daemon and every read and write in the process share one handle on one home — and the
// function that stops the daemon and closes the store, in that order.
//
// It is what makes the household's shared space real in isolated mode. Each pod has
// its own LORE_HOME and therefore its own lore account and its own id space, so the
// one `household.shared_space` in kenward.yaml is one space held by several accounts,
// and opening a store carries nothing between them. Until this existed, a member's pod
// reported the shared space missing and the household group conversation had memory in
// exactly one container; both deployment paths had it and neither said so. Membership
// in that space is still provisioned out of band, by the operator — see internal/memory's
// sync.go and docs/IMPLEMENTATION.md §8.
//
// # Which units get one
//
// Only an isolated unit, and that is a decision rather than an inheritance: it was worth
// re-asking once the daemon stopped costing a process. It still holds. A simple-mode
// node has one lore home holding every space, so there is nothing for a second instance
// to converge with; the daemon would find no peer it could exchange anything with and
// would advertise the household's store on the LAN for no gain at all. Cheap is not the
// same as free, and nothing is not the same as nothing useful. The isolated host
// supervisor gets none either: it holds no lore home, and each pod runs its own.
//
// # Failure
//
// Never fatal, deliberately, and unchanged: the daemon is what makes shared memory
// move, and private memory — the property the mode exists for — works without it.
// Refusing to serve would turn a partial outage into a total one. What tells an operator
// is `kenward doctor`, which asks the daemon itself over its admin port.
//
// A store that will not open returns no client, and the supervisor then builds its own
// and fails on the same fault with the message that path already has. It is not
// silenced: checkLore has already opened this same home successfully a moment ago, so
// reaching it means the store went away between the two.
func startSyncDaemon(e *env, cfg *config.Config, sel unitSelection, logger *slog.Logger) (memory.Memory, func()) {
	if cfg.Mode != config.ModeIsolated || !sel.single() {
		return nil, func() {}
	}
	client, err := memory.NewClient(memory.Config{LoreHome: loreHomeDir(e), Logger: logger})
	if err != nil {
		logger.Warn("kenward", "event", "memory",
			"detail", "this pod's lore store could not be opened for the sync daemon", "err", err.Error())
		return nil, func() {}
	}

	ctx, cancel := context.WithCancel(e.context())
	// A channel closed by the goroutine's own defer, created before the goroutine
	// exists: there is no counter to increment and therefore nothing to increment
	// outside a lock, and the stop function below cannot observe a half-registered
	// worker.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Zero interval: lore's own thirty seconds, plus a round on every local
		// write, because the store was opened with NotifyOnWrite. A non-nil return
		// is a daemon that never started, and there is nothing transient in the
		// list of ways that happens — see memory.Client.Serve. Logged, not retried,
		// and not fatal.
		if err := client.Serve(ctx, 0); err != nil {
			logger.Error("kenward", "event", "memory",
				"detail", "lore's sync daemon did not start; this pod's shared memory reaches nobody "+
					"and receives nothing, and its private memory is unaffected. `kenward doctor` reports it",
				"err", err.Error())
		}
	}()
	logger.Info("kenward", "event", "memory",
		"detail", "running lore's sync daemon in this process so this pod's shared space reaches the household's other pods",
		"lore_home", memory.DefaultLoreHome())
	return client, func() {
		cancel()
		<-done
		// After the daemon, never before: it holds this store for as long as it
		// runs, and the units that also hold it have already been drained.
		if err := client.Close(); err != nil {
			logger.Warn("kenward", "event", "memory", "detail", "this pod's lore store did not close cleanly", "err", err.Error())
		}
	}
}

// checkLore refuses to serve unless this node's memory actually answers. It returns
// exitOK when it does.
//
// It is a refusal rather than a warning, and that is the whole point of it. Nothing
// downstream fails on unreachable memory: a turn that cannot read a space degrades
// that space rather than failing the turn. So such a node starts cleanly, reports
// itself ready, authorises its bot, greets a member and then remembers nothing anyone
// says to it — no retrieval, no capture, no enrolment history — with the only trace a
// "space could not be read" line inside a prompt. A household assistant that has
// quietly stopped being one is worse than one that will not start, so it does not
// start.
//
// # It does not ask whether lore is installed, in any mode
//
// It used to, with a PATH lookup for memory.lore_command, on the reasoning that
// "spawning `lore mcp` is its only route to memory". That route is gone, and so is the
// last one after it. kenward opens the store in this process through lore's Go API,
// creates the home itself if the machine has never had one (lore.Init), creates spaces
// with lore.CreateSpace, and runs lore's own sync daemon in this process rather than as
// a subprocess (see startSyncDaemon). Every route to memory in every mode is a Go call,
// so there is no binary to look for and nothing to install. A refusal to start because
// one was absent would refuse a node that works, which is the strongest form of the
// thing this check exists to prevent — and the container measurement says so plainly:
// an isolated household of pods syncs its shared space with no `lore` on PATH anywhere
// inside them.
//
// The published image does carry the `lore` CLI, and that is not a contradiction. It is
// there for the one step that has no Go API and is not meant to have one — the `lore
// space invite` / `lore join` membership handshake, which a person runs inside a pod
// (see internal/memory/sync.go and docs/IMPLEMENTATION.md §8). kenward execs none of it.
//
// # Why a PATH lookup was never enough anyway
//
// An uninitialised store is the state every fresh volume is in, and lore refuses to
// open one. The program was on PATH, the old check passed, and the node started into
// exactly the silence the check exists to prevent. So the question asked here is the
// one that matters — not "is lore installed" but "does this store answer" — and it is
// the same handshake `doctor` performs, through the same seam, so the two cannot
// drift.
//
// The one process it does not apply to is the isolated host supervisor, and that is
// not an exemption but the plain fact of what it runs: it starts pods and holds no
// memory client, no transport and no key (see supervisor.Isolated). Each pod opens its
// own store over its own LORE_HOME and asks this question of itself on its own way up.
// Demanding memory of the host as well would refuse every correctly-configured
// isolated household on a machine whose lore lives only where it is actually used.
//
// # A store that answers is not the same as this household's store
//
// The check above asks whether lore answers. It used to be the whole of this function,
// and once kenward began creating its own home (see initLoreHome) that stopped being
// enough: a store kenward minted a moment ago answers perfectly and holds none of this
// household's memory. The refusal above could no longer fire for the commonest way
// memory goes missing — LORE_HOME pointing somewhere else — because there is no longer
// an uninitialised home to fail on. So the second question is asked here, and it is
// judged by judgeMemory, which `doctor` uses for its exit code too so that the two
// cannot say different things about one store.
//
// A store holding NONE of the spaces this unit is configured for is not this
// household's store, and that is fatal for exactly the reason at the top. A store
// holding some of them is one mistyped id, which is warned about and served through:
// refusing a household its assistant over one member's typo would be a second, larger
// outage than the one being prevented.
//
// The unit that may still hold none of them is an isolated pod with nothing of its own
// to make: a pod creates its own spaces on the way up (makeLoreSpaces), but it cannot
// join itself to the household's shared space, and that handshake happens INSIDE a
// running pod. A pod that refused to start until it had been invited could never be
// invited. judgeMemory draws that line and it is narrower than it was: a pod that does
// have a space of its own to make and holds none is no longer excused anything.
func checkLore(e *env, cfg *config.Config, sel unitSelection) int {
	if cfg.Mode == config.ModeIsolated && !sel.single() {
		return exitOK
	}
	// A fresh machine has no lore home, and a pod's own volume starts empty. Either
	// way kenward makes its own; see initLoreHome.
	created, code := initLoreHome(e, cfg, sel)
	if code != exitOK {
		return code
	}
	// And then the spaces that home is supposed to hold, for the units that make
	// their own; see makeOwnSpaces. Before the handshake below, so that a first
	// boot answers it the same way a fifth one does.
	if code := makeLoreSpaces(e, cfg, sel); code != exitOK {
		return code
	}

	ctx, cancel := context.WithTimeout(e.context(), loreHandshakeTimeout)
	defer cancel()
	res := e.probes.loreProbe()(ctx, cfg, sel.scope())
	if res.Err != nil {
		e.errorf("this node's lore store did not answer: %v\n\n"+
			"kenward will not start without memory. It opens the store in this process, so\n"+
			"there is no server to check and nothing to install — what failed is the store\n"+
			"itself, at %s.\n\n"+
			"The usual causes are a LORE_HOME pointing somewhere kenward cannot write, and a\n"+
			"store written by a newer lore than this build. `kenward doctor` reports the same\n"+
			"thing in more detail.",
			res.Err, memory.DefaultLoreHome())
		return exitFailure
	}

	v := judgeMemory(cfg, sel.scope(), res)
	switch {
	case v.Fatal:
		e.errorf("%s", wrongStoreRefusal(v.Missing, loreHomeFor(e), created))
		return exitFailure
	case len(v.Missing) > 0:
		// Served, and said out loud. Nothing downstream fails on a space that is not
		// there — the conversation using it simply stores nothing and retrieves
		// nothing — so without this line the only trace is a `doctor` nobody was told
		// to run.
		e.errorf("this store does not hold %s: %s\n"+
			"Those conversations will retrieve nothing and store nothing, and nothing else\n"+
			"will report it. Everything else serves normally; `kenward doctor` has the detail.",
			plural(len(v.Missing), "one space kenward.yaml names", "spaces kenward.yaml names"),
			joinSpaces(v.Missing))
	}
	return exitOK
}

// wrongStoreRefusal is what a node says when it has memory that is not its own.
//
// It names the space ids and the store, and nothing else. Space ids are already in
// kenward.yaml and in every `doctor` report; what is *in* a space is never this
// message's to show.
func wrongStoreRefusal(missing []domain.SpaceID, home string, created bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "this node's lore store holds none of the %d %s kenward.yaml names:\n\n",
		len(missing), plural(len(missing), "space", "spaces"))
	for _, s := range missing {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	fmt.Fprintf(&b, "\nThe store is %s", home)
	if created {
		b.WriteString(", and kenward created it a moment ago because there\nwas nothing there")
	}
	b.WriteString(". Either way it is not the store this household's memory was\n" +
		"put into, and kenward will not serve on it. A node that starts on the wrong store\n" +
		"authorises its bot, greets everybody and then remembers nothing anyone says to\n" +
		"it, with no error anywhere — which is worse than a node that will not start.\n\n" +
		"The usual cause is LORE_HOME naming a different store from the one the setup\n" +
		"wizard created these spaces in: a container handed a fresh volume, a service\n" +
		"running as another user, or a store that was moved or restored. Point LORE_HOME\n" +
		"at the store that holds them — `lore spaces` lists what a store holds — or run\n" +
		"`kenward setup` again against this one.")
	return b.String()
}

// joinSpaces renders a space list for one line of prose.
func joinSpaces(spaces []domain.SpaceID) string {
	out := make([]string, 0, len(spaces))
	for _, s := range spaces {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}

// loreHomeFor is the store this unit actually opened, for a message that has to name
// it. LORE_HOME through this process's own environment seam first, so that a test says
// what a test set; lore's own default otherwise.
func loreHomeFor(e *env) string {
	if home := loreHomeDir(e); home != "" {
		return home
	}
	return memory.DefaultLoreHome()
}

// loreHomeDir is the lore home this process will open.
//
// It reads the environment by lore's own convention rather than from kenward.yaml,
// because that is where the value actually lives: the supervisor sets LORE_HOME on every
// pod (supervisor.EnvLoreHome) and deploy/compose.isolated.yml sets it on every service.
// Nothing in kenward.yaml names it, and inventing a second place to state it would be a
// second thing to keep in step with lore.
//
// An unset variable yields "", and what happens then depends on the mode — see
// initLoreHome. Every isolated pod has LORE_HOME set, by the supervisor and by the
// compose file alike, so an isolated unit without one is somebody running a pod's
// command by hand on a host, and their ~/.lore is their own store rather than that
// member's. Simple mode is the opposite case: ~/.lore is exactly where this
// household's memory belongs, and it is what memory.DefaultLoreHome opens.
func loreHomeDir(e *env) string {
	h, ok := e.env()("LORE_HOME")
	if !ok {
		return ""
	}
	return strings.TrimSpace(h)
}

// loreDeviceName is the lore device name a simple-mode node registers itself under.
// An isolated pod uses the member's id instead, so that `lore devices` on a household's
// store names people rather than four identical rows.
const loreDeviceName = "kenward"

// initLoreHome gives this node's lore home an account, the first time it runs and never
// again.
//
// # Why this exists at all
//
// A pod's /work volume is created empty and LORE_HOME points inside it. lore refuses to
// open an uninitialised home, so checkLore, correctly, refuses to serve. It then refuses
// on every restart, forever, because nothing ever initialises the volume: not the
// supervisor, not the image, not `kenward setup`, not any documented step. The compose
// path has an operator step for it (deploy/compose.isolated.yml, step 4) and a
// bind-mount to reach the store with; a pod started by `kenward run` has neither, so
// isolated mode via the supervisor could not bring up a household that had never been
// brought up before — which is every household. It was found by driving real podman.
//
// A fresh simple-mode machine is the same problem with a friendlier failure. It used to
// be answered by telling the household to install lore and run `lore init`, which is the
// external install kenward is not supposed to have; the answer now is the same one the
// pod gets, against ~/.lore rather than a volume. Anyone who already runs lore keeps
// their store untouched — see Idempotence below — so kenward joins a machine that has
// one and equips a machine that does not.
//
// # Why the pod and not the host
//
// In isolated mode the four places this could live are the host supervisor, `kenward
// setup`, the image's entrypoint and the pod. The image's entrypoint *is* the pod — the
// final stage is distroless, with no shell to run a script in and the kenward binary as
// its ENTRYPOINT — so that is this. The other two are both the host reaching into a
// member's work volume to create their memory store, and a host that can write into a
// member's volume is one edit away from reading it back: the mode's whole claim is that
// the pod is the only process that touches its own volume, and the supervisor
// deliberately holds no path into one. So the pod does it, for itself, with what it
// already has.
//
// # Idempotence
//
// It asks lore to initialise the home every time and lets lore refuse. lore.Init
// creates nothing in a home that already holds an account.json, a device.json or a
// lore.db, and says so with ErrAlreadyInitialised, which memory.InitHome reports as
// created=false. That check used to be made here, by reading the directory and treating
// any non-empty one as taken — a rule written to stop a new account adopting an existing
// lore.db. lore names those three files instead of inferring them, so kenward's copy has
// gone: an existing store is never written to, never migrated and never re-keyed, and
// now that is enforced by the code that would have to do the writing.
//
// # What it does not do
//
// It does not create the spaces kenward.yaml names. Init makes an account, a device and
// one personal space, all with ids it chooses; household.shared_space and
// members[].private_space are ids that were written into the file when the household was
// set up. Both wizards make those with lore.CreateSpace on the machine they run on, which
// settles simple mode entirely. A pod's own volume is a store no wizard could ever reach,
// so a pod makes them for itself immediately after this — see makeLoreSpaces — at the
// configured ids rather than at ids of its own.
//
// # created is load-bearing, not bookkeeping
//
// It is returned rather than only printed because checkLore puts it in a refusal. "This
// store holds none of your spaces" and "this store holds none of your spaces and kenward
// made it thirty milliseconds ago" send an operator to two different places, and the
// second is by far the commoner one — a container handed a fresh volume.
func initLoreHome(e *env, cfg *config.Config, sel unitSelection) (created bool, code int) {
	if cfg.Mode == config.ModeIsolated && !sel.single() {
		return false, exitOK // the host supervisor; it holds no memory of its own.
	}
	home := loreHomeDir(e)
	if home == "" && cfg.Mode != config.ModeIsolated {
		// Simple mode's own store, which is where this household's memory lives.
		home = memory.DefaultLoreHome()
	}
	if home == "" {
		// An isolated unit with no LORE_HOME, or a user with no home directory at
		// all. Neither is a store this process may create; the handshake says so.
		return false, exitOK
	}

	device := loreDeviceName
	switch {
	case sel.group:
		device = "group"
	case sel.member != "":
		device = sel.member
	}
	ctx, cancel := context.WithTimeout(e.context(), loreInitTimeout)
	defer cancel()
	created, err := e.probes.loreInitProbe()(ctx, home, device)
	if err != nil {
		e.errorf("%s could not be initialised as this unit's lore store: %v\n\n"+
			"That directory is this unit's whole memory, and kenward will not serve without\n"+
			"one. In a pod it is the member's own work volume, which nothing outside the pod\n"+
			"can reach, so there is no operator step to fall back on — fix what the error\n"+
			"above says and restart.", home, err)
		return false, exitFailure
	}
	if !created {
		return false, exitOK // a store was already here, and it is not this process's to touch.
	}
	// The account and device ids lore minted went nowhere, and the recovery code with
	// them; see memory.InitHome. This line says what happened and nothing that is the
	// member's.
	who := sel.describe()
	if who == "no unit" {
		who = "this household"
	}
	e.printf("kenward: initialised a new lore store at %s for %s. It has an account, a device\n"+
		"and a personal space; the spaces kenward.yaml names come next, at the ids it names —\n"+
		"`kenward doctor` reports any that are still missing.\n",
		home, who)
	return true, exitOK
}

// makeLoreSpaces gives an isolated pod the spaces kenward.yaml configures it with, at
// the ids kenward.yaml configures them at, on every boot and only the first time it
// matters.
//
// # The step this replaces
//
// A pod's store was born holding an account, a device and a personal space, and none of
// the spaces the household had been told it had. The wizard runs on the host; a member's
// private space has to live in that member's own pod, on a volume the host cannot open,
// which is the whole claim of the mode. So the id in kenward.yaml named a space in the
// host's store and the pod's store held nothing by that name, `doctor` said `space "…"
// is not a space this lore store holds`, and the container sat unhealthy until an
// operator ran `lore space create` inside it, read the new id out of `lore spaces`, and
// pasted it over the one the wizard had written. Two ids for one space, a household that
// did not work until somebody did that per member, and no way for a member to be given
// their own memory without the operator handling their pod by hand.
//
// It was lore's half that was missing, not a decision anybody had taken: CreateSpace
// minted an id rather than accepting one. lore.CreateSpaceWithID accepts one and is
// idempotent, so the pod makes its own space at the id it was configured with, and the
// wizard's id is the only id there has ever been.
//
// # What is still not this function's
//
// Membership. The household's shared space is one space in several stores, and a store
// joins one through the invite handshake, which is a person deciding to share their
// memory with another person; lore exposes no API for it deliberately, and kenward would
// not use one if it did. So the group's pod creates the shared space and each member's
// pod joins it, and it is exactly the private space — one member's, in one pod, shared
// with nobody, with no membership decision in it at all — that no longer needs anyone.
//
// # Why it may refuse to start
//
// Because a pod that cannot make its own memory has none, and the node above will not
// serve without memory whatever the reason. The realistic failures are a store that is
// not writable and a configured id that is not a UUID; both are worth stopping on, and
// both say which it was.
func makeLoreSpaces(e *env, cfg *config.Config, sel unitSelection) int {
	ctx, cancel := context.WithTimeout(e.context(), loreInitTimeout)
	defer cancel()
	if err := e.probes.loreMakeProbe()(ctx, cfg, sel.scope()); err != nil {
		e.errorf("this unit's own lore spaces could not be created: %v\n\n"+
			"They are the ids kenward.yaml names for this unit, and this pod is the only\n"+
			"place they can be made — nothing outside it can reach its store. kenward will\n"+
			"not serve without memory, so fix what the error above says and restart.", err)
		return exitFailure
	}
	return exitOK
}

// loreInitTimeout bounds the one-off store creation above. It generates two key pairs and
// writes a handful of small files to a local volume; anything slower than this is a
// wedged filesystem, not a slow one.
const loreInitTimeout = 30 * time.Second

// loreHandshakeTimeout bounds the startup handshake above.
//
// The refusal it guards is a hard one, and that is a deliberate trade over the silent
// failure it replaces: a node that cannot reach memory does not start. It is affordable
// because there is no network in this path. `lore mcp` is a local subprocess over a
// local SQLite store, so its only transient failure is store contention, which
// internal/memory already retries with backoff before reporting anything. Whatever has
// not answered by now is not momentary, and a node that hangs on startup is worse than
// one that refuses and says why — a container runtime restarts it either way, but only
// the refusal leaves a line an operator can read.
const loreHandshakeTimeout = 30 * time.Second

// resumeUpdate finishes a pending update. The bool reports whether the process
// should stop now rather than serve.
func resumeUpdate(e *env, sched *updater.Scheduler, logger *slog.Logger) (int, bool) {
	report, err := sched.Resume(e.context())
	switch {
	case errors.Is(err, keelupdate.ErrRestartPending):
		// A rollback restored the previous binary. This process is the wrong
		// build to go on running.
		logger.Info("kenward", "event", "update",
			"detail", "the previous version has been restored; stopping so the service manager restarts onto it")
		return exitRestartRequested, true
	case errors.Is(err, keelupdate.ErrLocked):
		// A sibling process off the same install path is resuming. Serving is
		// still correct; it will be finished by whoever holds the lock.
		logger.Info("kenward", "event", "update", "detail", "another process is finishing an update on this install path")
	case err != nil:
		logger.Warn("kenward", "event", "update", "detail", "could not finish a pending update", "err", err.Error())
	case report.Outcome != keelupdate.OutcomeNone:
		logger.Info("kenward", "event", "update",
			"outcome", outcomeName(report.Outcome),
			"from", report.From, "to", report.To, "reason", report.Reason)
	}
	return exitOK, false
}

// serve runs the household and the update loop together, and drains both on SIGINT
// or SIGTERM.
//
// Start blocks. When the signal context is cancelled, Start returns and Stop is
// called with a fresh, bounded context: the signal context is already dead, and a
// drain that inherits a cancelled context finishes no turns at all, which is the
// opposite of draining.
func serve(e *env, sup supervisor.Supervisor, sched *updater.Scheduler, restart *restartSignal, logger *slog.Logger) int {
	ctx, cancel := context.WithCancel(e.context())
	defer cancel()

	// A restart request stops intake: the scheduler asks for one after it has
	// drained and swapped (or drained and failed to swap), and either way this
	// process has to go down.
	go func() {
		select {
		case <-restart.waitCh():
			cancel()
		case <-ctx.Done():
		}
	}()

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		err := sched.Run(ctx)
		switch {
		case err == nil, errors.Is(err, context.Canceled):
		case errors.Is(err, keelupdate.ErrRestartPending):
			// Belt and braces: this only happens if the Restart hook was not
			// honoured, and the process still has to come back.
			restart.request()
		default:
			// The loop is not supposed to end for any other reason, and the
			// household keeps working on the version it has either way.
			logger.Warn("kenward", "event", "update", "detail", "the update loop stopped", "err", err.Error())
		}
	}()

	startErr := sup.Start(ctx)

	// Deliberately no cancel() here. Start returns as soon as the supervisor
	// stops, and one of the ways it stops is the scheduler's own drain hook
	// calling Stop immediately before a swap. Cancelling the scheduler's context
	// at this point would abort that update between the drain and the swap,
	// leaving a household whose intake is already stopped on the version it had
	// and nothing to bring it back. So the update is given its bounded chance to
	// finish first; the signal path is unaffected, because a signal has already
	// cancelled this context and Run has already returned.
	if ctx.Err() == nil {
		select {
		case <-schedDone:
		case <-time.After(updateFinishGrace):
			logger.Warn("kenward", "event", "update",
				"detail", "an update was still in flight when the household stopped; giving up on it")
		}
	}
	cancel()

	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(e.context()), supervisor.DefaultDrainTimeout)
	defer drainCancel()
	logger.Info("kenward", "event", "draining",
		"detail", "no new messages; in-flight turns finish, then every session is locked")
	stopErr := sup.Stop(drainCtx)
	<-schedDone

	switch {
	case restart.wanted():
		logger.Info("kenward", "event", "stopped",
			"detail", "all sessions locked; exiting so the service manager restarts this node")
		return exitRestartRequested
	case startErr != nil && !errors.Is(startErr, context.Canceled) && !errors.Is(startErr, context.DeadlineExceeded):
		e.errorf("%v", startErr)
		return exitFailure
	case stopErr != nil && !errors.Is(stopErr, context.Canceled):
		// A drain that ran out of time cut a turn. Say so; do not report success.
		e.errorf("shutdown was not clean: %v", stopErr)
		return exitFailure
	}
	logger.Info("kenward", "event", "stopped", "detail", "all sessions locked")
	return exitOK
}

// updateFinishGrace bounds how long a shutdown waits for an update that is already
// past its drain.
//
// What happens in that window is a binary swap and a rename of the retained previous
// version — local filesystem work, not a download, which happened before the drain.
// A minute is far more than it needs and short enough that a wedged updater cannot
// hold a shutdown open indefinitely.
const updateFinishGrace = time.Minute

// startupSummary is the one thing an operator reads to know what this node will and
// will not do.
//
// docs/CLI.md asks for exactly one summary line per space, naming the mode, who is
// served, and that space's tier chain — and specifically that somebody can read it
// and know whether a private space can reach a provider. So `reaches_provider` is on
// every line, computed from the endpoints the chain's tiers actually name rather
// than from the tier's name, because a tier called "local" whose endpoint is a
// provider is exactly the mistake this line exists to catch.
func startupSummary(cfg *config.Config, sel unitSelection) [][]any {
	local := localTiers(cfg)
	members := cfg.DomainMembers()

	// Scoped to what this process actually serves, same as sel governs the topology
	// line and which per-space lines appear below: a member pod names only its own
	// member (resolveUnitSelection and the config load already guarantee that name is
	// real, so there is nothing left to look up), a group pod serves no individual
	// member at all, and the household node names everyone who has claimed an invite.
	// This one value is reused on every line below, so fixing it here fixes all of
	// them together.
	var servedList string
	switch {
	case sel.group:
		servedList = "(none: this pod serves the household group, not individual members)"
	case sel.member != "":
		servedList = sel.member
	default:
		served := make([]string, 0, len(members))
		for _, m := range members {
			if m.Enrolled() {
				served = append(served, string(m.ID))
			}
		}
		sort.Strings(served)
		servedList = strings.Join(served, ",")
		if servedList == "" {
			servedList = "(nobody has claimed an invite yet)"
		}
	}

	mode := string(cfg.Mode)
	scopeLabel := "every unit in this process"
	if sel.single() {
		scopeLabel = "this pod runs only " + sel.label()
	}

	var lines [][]any
	lines = append(lines, []any{
		"event", "starting",
		"version", version.Short(),
		"mode", mode,
		"topology", scopeLabel,
		"members_served", servedList,
	})

	for _, m := range members {
		if sel.member != "" && string(m.ID) != sel.member {
			continue
		}
		if sel.group {
			continue
		}
		if m.SharedOnly {
			// No space of theirs to name, and no chain of theirs to report as
			// reaching a provider or not. Their conversation is the household's
			// and is covered by the shared space's own line below.
			continue
		}
		lines = append(lines, []any{
			"event", "space",
			"mode", mode,
			"space", string(m.Private),
			"conversation", m.Name + " (private)",
			"members_served", servedList,
			"tiers", "[" + strings.Join(m.Tiers, ", ") + "]",
			"reaches_provider", !staysHome(local, m.Tiers),
			"enrolled", m.Enrolled(),
		})
	}
	if sel.member == "" && cfg.Household.SharedSpace != "" {
		lines = append(lines, []any{
			"event", "space",
			"mode", mode,
			"space", cfg.Household.SharedSpace,
			"conversation", cfg.Household.Name + " (household group)",
			"members_served", servedList,
			"tiers", "[" + strings.Join(cfg.Household.Tiers, ", ") + "]",
			"reaches_provider", !staysHome(local, cfg.Household.Tiers),
			"enrolled", cfg.Household.GroupChatID != 0,
		})
	}
	return lines
}

// privacyModeFor maps the configured mode onto internal/privacy's own. The two
// enumerations are separate deliberately: internal/privacy must not depend on the
// shape of a configuration file to state what a topology protects.
func privacyModeFor(m config.Mode) privacy.Mode {
	if m == config.ModeIsolated {
		return privacy.ModeIsolated
	}
	return privacy.ModeSimple
}
