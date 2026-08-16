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

	// Whether memory.lore_command is *shaped* like a command is internal/config's, and
	// it deliberately stops there: whether the program exists is a property of this
	// machine, not of the file, and a validation that failed on one host would make
	// `doctor` useless for checking a configuration before shipping it. So the machine
	// asks here, on the machine, at the one moment a process commits to serving.
	if code := checkLore(e, cfg, sel); code != exitOK {
		return code
	}

	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, line := range startupSummary(cfg, sel) {
		logger.Info("kenward", line...)
	}

	// Started before the supervisor and stopped after it, so a pod is syncing for the
	// whole of the time it is serving.
	stopSync := startSyncDaemon(e, cfg, sel, logger)
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

	return serve(e, sup, sched, restart, logger)
}

// startSyncDaemon runs `lore serve` beside this unit for as long as it serves, and
// returns the function that stops it.
//
// It is what makes the household's shared space real in isolated mode. Each pod has
// its own LORE_HOME and therefore its own lore account and its own id space, so the
// one `household.shared_space` in kenward.yaml is one space held by several accounts,
// and `lore mcp` — which never syncs — carries nothing between them. Until this
// existed, a member's pod reported the shared space missing and the household group
// conversation had memory in exactly one container; both deployment paths had it and
// neither said so. Membership in that space is still provisioned out of band, by the
// operator, exactly as `lore init` is — see internal/memory's sync.go and
// docs/IMPLEMENTATION.md §8.
//
// Only an isolated unit gets one, and the condition is the one checkLore already
// draws. A simple-mode node has one lore home holding every space, so there is
// nothing for a second instance to converge with and a daemon would advertise a
// household's whole store on its LAN for no gain. The isolated host supervisor gets
// none either: it holds no lore home at all, and each pod runs its own.
//
// Failure to start is never fatal here, deliberately: the daemon is what makes shared
// memory move, and private memory — the property the mode exists for — works without
// it. Refusing to serve would turn a partial outage into a total one. What tells an
// operator is `kenward doctor`, which asks the daemon itself.
func startSyncDaemon(e *env, cfg *config.Config, sel unitSelection, logger *slog.Logger) func() {
	if cfg.Mode != config.ModeIsolated || !sel.single() || len(cfg.Memory.LoreCommand) == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(e.context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		memory.RunSyncDaemon(ctx, memory.Config{
			Command: cfg.Memory.LoreCommand[0],
			Logger:  logger,
		}, e.stderr, logger)
	}()
	logger.Info("kenward", "event", "memory",
		"detail", "running lore's sync daemon so this pod's shared space reaches the household's other pods",
		"lore_home", memory.DefaultLoreHome())
	return func() {
		cancel()
		<-done
	}
}

// checkLore refuses to serve unless lore actually answers on this machine. It returns
// exitOK when it does.
//
// It is a refusal rather than a warning, and that is the whole point of it. Nothing
// downstream fails on a missing lore: memory.NewClient only checks the command is
// non-empty and does not spawn anything until the first call, and a turn that cannot
// read a space degrades that space rather than failing the turn. So a node without
// lore starts cleanly, reports itself ready, authorises its bot, greets a member and
// then remembers nothing anyone says to it — no retrieval, no capture, no enrolment
// history — with the only trace a "space could not be read" line inside a prompt. A
// household assistant that has quietly stopped being one is worse than one that will
// not start, so it does not start.
//
// It matters most in isolated mode, where it is easiest to arrive here by accident.
// The image deliberately carries no lore (see the Dockerfile), and a pod the host
// supervisor starts cannot be given one by the compose file's route: keel's
// sandbox.Spec has no bind-mount, so the operator's only option there is an image
// that already carries it. Until this check existed, the default `kenward run` in
// isolated mode — which starts pods from the published image — produced a household
// of pods with no memory at all and said nothing about it.
//
// The one process it does NOT apply to is the isolated host supervisor, and that is
// not an exemption but the plain fact of what it runs: it starts pods and holds no
// memory client, no transport and no key (see supervisor.Isolated). Each pod spawns
// its own `lore mcp` inside itself, over its own LORE_HOME, and asks this question of
// its own image on its own way up. Demanding lore of the host as well would refuse
// every correctly-configured isolated household on a machine whose lore lives only
// where it is actually used — which is exactly what happened the first time this was
// run against real podman:
//
//	$ kenward run --config kenward.yaml --image localhost/kenward:nolore
//	kenward: memory.lore_command starts "lore", and there is no such program on this
//	machine's PATH. …
//
// with no pod started, on a host that was never going to touch lore.
//
// # Why a PATH lookup is not enough
//
// It was, until the compose paths were run for the first time. `lore mcp` exits before
// the MCP handshake when LORE_HOME holds no account — "no account at
// /home/nonroot/.lore/account.json (run `lore init`)" — and an uninitialised store is
// the state every fresh volume is in. The program was on PATH, the check passed, and
// the node started into exactly the silence the check exists to prevent. So the
// question asked here is the one that matters: not "is lore installed" but "does lore
// answer", which only a handshake can settle. It is the same handshake `doctor`
// performs, through the same seam, so the two cannot drift.
//
// Only lore failing to answer is fatal. A space lore does not hold is one space's
// problem and `doctor`'s to report; refusing a household its assistant over a
// mistyped space id would be a second, larger outage than the one being prevented.
func checkLore(e *env, cfg *config.Config, sel unitSelection) int {
	if cfg.Mode == config.ModeIsolated && !sel.single() {
		return exitOK
	}
	cmd := cfg.Memory.LoreCommand
	if len(cmd) == 0 {
		return exitOK // internal/config has already reported this.
	}
	if _, err := e.look()(cmd[0]); err != nil {
		e.errorf("memory.lore_command starts %q, and there is no such program on this machine's\n"+
			"PATH. kenward will not start without it: spawning `lore mcp` is its only route to\n"+
			"memory, so this node would run, answer, and record nothing — and nothing else\n"+
			"would report it.\n\n"+
			"In a container the image deliberately does not carry lore (see the Dockerfile).\n"+
			"Supply it one of two ways, built for the image's own OS and architecture:\n"+
			"  - bind-mount it at /usr/local/bin/lore, which is what\n"+
			"    deploy/compose.isolated.yml does for every service;\n"+
			"  - or build a derived image that COPYs it there. That is the only route open to\n"+
			"    a pod started by `kenward run` in isolated mode, which has no bind-mount to\n"+
			"    offer; pass the derived image with --image.", cmd[0])
		return exitFailure
	}

	ctx, cancel := context.WithTimeout(e.context(), loreHandshakeTimeout)
	defer cancel()
	if res := e.probes.loreProbe()(ctx, cfg, sel.scope()); res.Err != nil {
		e.errorf("%q is on this machine's PATH but did not answer: %v\n\n"+
			"kenward will not start without memory. Spawning `lore mcp` is its only route to\n"+
			"it, so this node would run, answer, and record nothing — and nothing else would\n"+
			"report it.\n\n"+
			"The usual cause is a LORE_HOME that was never initialised. `lore mcp` exits before\n"+
			"the MCP handshake when there is no account there. Initialise it once, against the\n"+
			"same LORE_HOME this node uses, and create the spaces kenward.yaml names:\n\n"+
			"    lore init --name kenward\n"+
			"    lore space create <name>\n\n"+
			"then put the ids `lore spaces` prints into household.shared_space and each\n"+
			"member's private_space. `kenward doctor` reports the same thing in more detail.",
			cmd[0], res.Err)
		return exitFailure
	}
	return exitOK
}

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
