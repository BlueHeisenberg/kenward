package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
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
	cfg, cfgErr := loadConfig(path, resolveDataDir(e, *dataDir), e.env())
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

	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, line := range startupSummary(cfg, sel) {
		logger.Info("kenward", line...)
	}

	// Before anything is served: finish whatever update was in flight when this
	// process last stopped, so a swapped binary is committed or rolled back rather
	// than left in a half-applied state nothing ever resolves.
	resumeUpdate(e, cfg, logger)

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

	return serve(e, sup, logger)
}

// serve starts the supervisor and drains it on SIGINT or SIGTERM.
//
// Start blocks. When the process signal context is cancelled, Start returns and Stop
// is called with a fresh, bounded context: the signal context is already dead, and a
// drain that inherits a cancelled context finishes no turns at all, which is the
// opposite of draining.
func serve(e *env, sup supervisor.Supervisor, logger *slog.Logger) int {
	ctx := e.context()
	startErr := sup.Start(ctx)

	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supervisor.DefaultDrainTimeout)
	defer cancel()
	logger.Info("kenward", "event", "draining",
		"detail", "no new messages; in-flight turns finish, then every session is locked")
	stopErr := sup.Stop(drainCtx)

	switch {
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
