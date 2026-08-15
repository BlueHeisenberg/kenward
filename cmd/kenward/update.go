package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	keelupdate "github.com/BlueHeisenberg/keel/update"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

func cmdUpdate(e *env, args []string) int {
	fs := newFlagSet(e, "update", "kenward update [--check] [--config PATH] [--data-dir PATH]")
	checkOnly := fs.Bool("check", false, "report what is available and change nothing")
	configPath := fs.String("config", "", "path to kenward.yaml")
	dataDir := fs.String("data-dir", "", "override the data directory")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("update takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	path := resolveConfigPath(e, *configPath)
	cfg, err := loadConfig(path, resolveDataDir(e, *dataDir), e.env())
	if err != nil {
		fmt.Fprint(e.stderr, renderConfigError(path, err))
		return exitUsage
	}

	channel := cfg.Update.Channel
	e.printf("Update channel: %s\n", channel)

	// `channel: off` is a fully supported way to run kenward forever, not a
	// degraded state, and it is said in those words so nobody reads it as a fault
	// to be fixed.
	if channel == config.UpdateOff {
		e.printf("\nUpdates are off. kenward will never fetch anything, check anything or replace\n")
		e.printf("itself, and it works indefinitely this way. Turn it on by setting\n")
		e.printf("update.channel to stable or edge in %s.\n", path)
		return exitOK
	}

	keys, err := trustedKeys()
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}
	if len(keys) == 0 {
		e.errorf("this build has no release signing keys compiled into it, so it cannot verify a\n" +
			"manifest signature. Refusing rather than fetching: an unverified update is a way\n" +
			"to run somebody else's code on this household's machine. Install a signed release\n" +
			"build, or update by hand.")
		return exitFailure
	}

	updater, err := keelupdate.New(keelupdate.Config{
		ManifestURL: releaseManifestURL,
		Keys:        keys,
		Channel:     keelupdate.Channel(channel),
		Current:     version.Version,
		// The staged binary is executed with these before the swap and must exit
		// 0. `version` touches nothing: no configuration, no lore, no network.
		PreflightArgs: []string{"version"},
		CheckInterval: cfg.Update.CheckInterval.Duration(),
		Health:        func(context.Context) error { return nil },
		Drain:         func(context.Context) error { return nil },
		Consent:       consentPrompt(e),
	})
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}

	status, err := updater.Check(e.context())
	if err != nil {
		if errors.Is(err, keelupdate.ErrChannelOff) {
			return exitOK
		}
		e.errorf("checking for an update: %v", err)
		return exitFailure
	}

	e.printf("Running:   %s\n", displayVersion(status.Current))
	e.printf("Platform:  %s\n", status.Platform)
	if !status.Available {
		reason := status.Reason
		if reason == "" {
			reason = "no newer release for this channel and platform"
		}
		e.printf("Available: nothing — %s\n", reason)
		return exitOK
	}
	e.printf("Available: %s\n", status.Latest)
	if status.Release != nil && status.Release.SecuritySensitive {
		e.printf("\nThis release is flagged as changing security-relevant defaults. It will ask\n")
		e.printf("before applying, whatever the size of the version bump.\n")
	}
	if status.Release != nil && strings.TrimSpace(status.Release.Notes) != "" {
		e.printf("\n%s\n", indent(strings.TrimSpace(status.Release.Notes), "  "))
	}

	if *checkOnly {
		e.printf("\n--check changes nothing. Run `kenward update` without it to apply.\n")
		return exitOK
	}

	err = updater.Apply(e.context(), *status.Release)
	switch {
	case err == nil:
		e.printf("\nUpdated to %s.\n", status.Latest)
		return exitOK
	case errors.Is(err, keelupdate.ErrRestartPending):
		// No Restart hook: this is an operator at a terminal, and restarting the
		// household's node out from under them is not this command's decision.
		e.printf("\nUpdated to %s on disk. Restart kenward to finish: the new binary health-checks\n", status.Latest)
		e.printf("itself on its next start and rolls back automatically if it does not come up.\n")
		return exitOK
	case errors.Is(err, keelupdate.ErrConsentDeclined):
		e.printf("\nNot applied. Nothing on disk changed.\n")
		return exitOK
	case errors.Is(err, keelupdate.ErrLocked):
		e.printf("\nAnother process on this machine is updating the same binary. Nothing was done.\n")
		return exitOK
	case errors.Is(err, keelupdate.ErrPreflight):
		e.errorf("the downloaded build would not run on this machine, so it was not installed.\n" +
			"kenward is still on the version it was. Nothing on disk changed.")
		return exitFailure
	default:
		e.errorf("applying the update: %v", err)
		return exitFailure
	}
}

// resumeUpdate finishes whatever update was in flight when this process last
// stopped. It runs early in `run`, before anything is served.
//
// Without it an update swaps the binary and then nothing ever commits or rolls it
// back: the journal keel/update wrote sits there, the health check that decides
// between keeping and reverting never runs, and a bad build stays installed. That is
// the wedged installation the automatic rollback exists to prevent.
//
// Health is exactly what docs/IMPLEMENTATION.md §9 says it is — the process started,
// lore answers, Telegram authorises — and deliberately not endpoint reachability. A
// household's machines are legitimately powered off; making them part of health
// would roll back a good update, re-apply it, and roll it back again forever.
func resumeUpdate(e *env, cfg *config.Config, logger *slog.Logger) {
	if cfg.Update.Channel == config.UpdateOff {
		return
	}
	keys, err := trustedKeys()
	if err != nil || len(keys) == 0 {
		// A build with no compiled-in release key never applied an update, so
		// there is nothing in flight to finish.
		return
	}
	updater, err := keelupdate.New(keelupdate.Config{
		ManifestURL:   releaseManifestURL,
		Keys:          keys,
		Channel:       keelupdate.Channel(cfg.Update.Channel),
		Current:       version.Version,
		PreflightArgs: []string{"version"},
		CheckInterval: cfg.Update.CheckInterval.Duration(),
		Health:        nodeHealth(e, cfg),
		Drain:         func(context.Context) error { return nil },
		// No Consent hook: consent is asked over Telegram by the automatic path
		// and at a terminal by `kenward update`. Resume only finishes a decision
		// somebody already made.
		Logger: logger,
	})
	if err != nil {
		logger.Warn("kenward", "event", "update", "detail", "could not build the updater", "err", err.Error())
		return
	}
	report, err := updater.Resume(e.context())
	if err != nil {
		logger.Warn("kenward", "event", "update", "detail", "resuming a pending update", "err", err.Error())
		return
	}
	if report.Outcome == keelupdate.OutcomeNone {
		return
	}
	logger.Info("kenward",
		"event", "update",
		"outcome", outcomeName(report.Outcome),
		"from", report.From,
		"to", report.To,
		"reason", report.Reason)
}

// nodeHealth is the health check a freshly swapped binary has to pass.
func nodeHealth(e *env, cfg *config.Config) keelupdate.HealthCheck {
	return func(ctx context.Context) error {
		if res := e.probes.loreProbe()(ctx, cfg); res.Err != nil {
			return fmt.Errorf("lore did not respond: %w", res.Err)
		}
		token, ok := e.env()(cfg.Telegram.BotTokenEnv)
		if !ok || token == "" {
			return fmt.Errorf("%s is not set", cfg.Telegram.BotTokenEnv)
		}
		if res := e.probes.telegramProbe()(ctx, token); res.Err != nil {
			return fmt.Errorf("telegram did not authorise: %w", res.Err)
		}
		return nil
	}
}

func outcomeName(o keelupdate.Outcome) string {
	switch o {
	case keelupdate.OutcomeCommitted:
		return "committed"
	case keelupdate.OutcomeRolledBack:
		return "rolled back"
	case keelupdate.OutcomeAborted:
		return "aborted"
	default:
		return "none"
	}
}

func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return v + " (built without version metadata)"
	}
	return v
}

// consentPrompt is the Consent hook for a command someone typed.
//
// A major version bump and a release flagged as changing security-relevant defaults
// must not be applied without a human agreeing. At a terminal, agreeing is typing
// yes. When there is nobody there — a pipe, a cron job, a script — the answer is no:
// a release that may move routing or privacy defaults is exactly the one that must
// not slip through because nothing was listening.
func consentPrompt(e *env) keelupdate.Consent {
	return func(_ context.Context, from, to keelupdate.Version, notes string) (bool, error) {
		fmt.Fprintf(e.stdout, "\nThis update needs your agreement before it is applied.\n")
		fmt.Fprintf(e.stdout, "  from: %s\n  to:   %s\n", from, to)
		if strings.TrimSpace(notes) != "" {
			fmt.Fprintf(e.stdout, "\n%s\n", indent(strings.TrimSpace(notes), "  "))
		}
		fmt.Fprintf(e.stdout, "\nIt is a major version or it changes security-relevant defaults. A release may\n")
		fmt.Fprintf(e.stdout, "never silently change routing policy or tier configuration, so this is asked\n")
		fmt.Fprintf(e.stdout, "rather than assumed.\n\nApply it? [y/N] ")

		if e.stdin == nil {
			fmt.Fprintf(e.stdout, "\nNo input available; taking that as no.\n")
			return false, nil
		}
		line, err := bufio.NewReader(e.stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintf(e.stdout, "\nNo answer; taking that as no.\n")
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}
