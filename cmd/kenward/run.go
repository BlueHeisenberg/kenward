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
	"github.com/BlueHeisenberg/kenward/internal/domain"
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
}

// supervisorFactory builds the thing `run` runs. It is a seam so that argument
// handling, mode selection and the startup summary can be tested without a bot
// token, a container runtime or a network.
type supervisorFactory func(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.Supervisor, error)

func cmdRun(e *env, args []string) int {
	fs := newFlagSet(e, "run", "kenward run [--config PATH] [--data-dir PATH] [--member ID | --group]")
	configPath := fs.String("config", "", "path to kenward.yaml (default: $KENWARD_CONFIG, then ./kenward.yaml, then the per-OS config location)")
	dataDir := fs.String("data-dir", "", "override the data directory (default: $KENWARD_DATA_DIR, then data_dir in the configuration)")
	member := fs.String("member", "", "isolated mode only: run this member's unit and nothing else")
	group := fs.Bool("group", false, "isolated mode only: run the household group's unit and nothing else")
	image := fs.String("image", "", "isolated mode only: the pod image the host supervisor starts pods from")
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

	path := resolveConfigPath(e, *configPath)
	cfg, cfgErr := loadConfig(path, resolveDataDir(e, *dataDir), e.secrets())
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
	if sel.member != "" {
		if _, ok := cfg.MemberByID(domain.MemberID(sel.member)); !ok {
			e.errorf("--member %s names no member in %s", sel.member, path)
			return exitUsage
		}
	}

	// internal/config neither defaults nor validates memory.lore_command, so a
	// hand-written file that omits the memory: block validates and then fails deep
	// inside the wiring with whatever the client says about spawning nothing. The
	// right place for this is validation; until it moves there, the failure at
	// least names the key and the file it is missing from.
	if len(cfg.Memory.LoreCommand) == 0 {
		e.errorf("%s does not say how to start lore: memory.lore_command is missing or empty.\n\n"+
			"lore is where everything kenward remembers lives, and kenward starts it as a\n"+
			"subprocess rather than talking to a server. Without it there is no retrieval, no\n"+
			"capture and no enrolment history. Add:\n\n"+
			"  memory:\n"+
			"    lore_command: [lore, mcp]\n\n"+
			"which is what `kenward setup` writes, and make sure a `lore` binary is on PATH.", path)
		return exitUsage
	}

	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, line := range startupSummary(cfg, sel) {
		logger.Info("kenward", line...)
	}

	factory := e.supervisors
	if factory == nil {
		factory = defaultSupervisor
	}
	sup, buildErr := factory(e, cfg, runOptions{
		configPath: path,
		dataDir:    cfg.DataDir,
		selection:  sel,
		image:      *image,
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

	served := make([]string, 0, len(members))
	for _, m := range members {
		if m.Enrolled() {
			served = append(served, string(m.ID))
		}
	}
	sort.Strings(served)
	servedList := strings.Join(served, ",")
	if servedList == "" {
		servedList = "(nobody has claimed an invite yet)"
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
