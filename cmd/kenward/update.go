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
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/updater"
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
	cfg, err := loadConfig(path, resolveDataDir(e, *dataDir), e.secrets())
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

	// The manual command and the scheduled path must judge a manifest and a build
	// by the same rules. A `kenward update` that skipped the health check, or
	// accepted a replayed manifest the scheduler refuses, would be two behaviours
	// wearing one name — and the one somebody runs by hand is the one they would
	// reach for precisely when the automatic path has already refused.
	probes := nodeHealthProbes(e, cfg, unitSelection{})
	up, err := keelupdate.New(keelupdate.Config{
		ManifestURL: releaseManifestURL,
		Keys:        keys,
		Channel:     keelupdate.Channel(channel),
		Current:     version.Version,
		// The staged binary is executed with these before the swap and must exit
		// 0. `version` touches nothing: no configuration, no lore, no network.
		PreflightArgs:  []string{"version"},
		CheckInterval:  cfg.Update.CheckInterval.Duration(),
		MaxManifestAge: updater.DefaultMaxManifestAge,
		StableDelay:    updater.DefaultStableDelay,
		Health: func(ctx context.Context) error {
			if err := probes.Lore(ctx); err != nil {
				return err
			}
			return probes.Telegram(ctx)
		},
		// Drain is nil: a CLI invocation is not serving anybody, so there is no
		// turn in flight to wait for. The running node does its own draining when
		// the service manager stops it.
		Consent: consentPrompt(e),
	})
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}

	status, err := up.Check(e.context())
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

	err = up.Apply(e.context(), *status.Release)
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

// newScheduler builds the periodic update checker `run` starts.
//
// It is never a reason to refuse to start. internal/updater says so in its own
// documentation and makes a nil *Scheduler safe to Run and Resume for exactly this
// reason: a household whose update configuration is wrong should still have a working
// assistant. The caller logs the error and carries on serving.
//
// drain is the supervisor's own drain rather than a second mechanism. The supervisor
// is the one component that actually knows whether a turn is in flight, and two
// sources of that truth would eventually disagree — at which point a restart lands in
// the middle of somebody's conversation.
func newScheduler(e *env, cfg *config.Config, sel unitSelection, drain updater.Drainer, restart *restartSignal, logger *slog.Logger) (*updater.Scheduler, error) {
	keys, err := trustedKeys()
	if err != nil {
		return nil, err
	}
	return updater.New(updater.Options{
		Channel:       keelupdate.Channel(cfg.Update.Channel),
		CheckInterval: cfg.Update.CheckInterval.Duration(),
		ManifestURL:   releaseManifestURL,
		Keys:          keys,
		Health:        nodeHealthProbes(e, cfg, sel),
		Drain:         drain,
		// Consent is deliberately nil, which means this scheduler never applies a
		// major version or a release flagged as changing security-relevant
		// defaults — it logs the refusal once per version and leaves it — while
		// patch and minor releases keep applying.
		//
		// The intended implementation is a question in the household chat over
		// kenward's own transport, and it is not wired because this layer cannot
		// reach that transport cleanly. The supervisor owns the bot and does not
		// expose it, and Telegram long-polling admits one consumer per token: a
		// second transport built here would either never see the member's tap
		// (so every request would time out, which internal/updater correctly
		// treats as a refusal anyway) or would steal updates from the units and
		// break the assistant to ask a question about updating it.
		//
		// Refusing is the safe failure and it is what the documentation
		// promises. Wiring it properly means the supervisor exposing a way to
		// ask the household group something; that is its decision, not this
		// layer's.
		Consent: nil,
		// Restart records the request and lets the serve loop finish its own
		// shutdown before the process exits; the service manager then brings
		// kenward back on whichever binary is now installed. It is supplied
		// rather than left nil because the scheduler calls it after a failed
		// swap too — see restartSignal for why that case is the important one.
		Restart: restart.hook(),
		Logger:  logger,
	})
}

// nodeHealthProbes is the health check a freshly swapped binary has to pass.
//
// It is what docs/IMPLEMENTATION.md §9 says health is — the process started, lore
// answers, this node's bot token authorises — and deliberately not endpoint
// reachability. A household's inference machines are legitimately powered off; making
// them part of health would roll a good update back, re-apply it on the next check,
// and roll it back again, forever. It is the same set of checks `doctor` exits
// non-zero for, minus the endpoints, which is not a coincidence: they are the
// conditions under which this process cannot do its job at all.
func nodeHealthProbes(e *env, cfg *config.Config, sel unitSelection) updater.HealthProbes {
	return updater.HealthProbes{
		Lore: func(ctx context.Context) error {
			if res := e.probes.loreProbe()(ctx, cfg); res.Err != nil {
				return fmt.Errorf("lore did not respond: %w", res.Err)
			}
			return nil
		},
		Telegram: func(ctx context.Context) error {
			// Resolved here rather than captured when the probes were built, so
			// a rotated credential file is read as it stands at the moment health
			// is judged — which, after a binary swap, is a different moment.
			sec, err := healthToken(cfg, sel, e.secrets())
			if err != nil {
				return fmt.Errorf("the bot token could not be read: %w", err)
			}
			if !sec.IsSet() {
				return errors.New("no bot token is configured")
			}
			if res := e.probes.telegramProbe()(ctx, sec.Value()); res.Err != nil {
				return fmt.Errorf("telegram did not authorise the token from %s: %w", sec.Source(), res.Err)
			}
			return nil
		},
	}
}

// healthToken resolves the bot token whose authorisation is this process's health.
//
// A member's pod holds that member's own token and nothing else, so checking the
// household's token there would test a credential the process does not have — and
// would fail health on a perfectly good pod, which, since health decides rollback,
// would roll that pod back forever.
func healthToken(cfg *config.Config, sel unitSelection, secrets *config.Secrets) (config.Secret, error) {
	if sel.member != "" {
		if m, ok := cfg.MemberByID(domain.MemberID(sel.member)); ok {
			for _, mc := range cfg.Members {
				if domain.MemberID(mc.ID) == m.ID {
					return mc.BotToken(secrets)
				}
			}
		}
	}
	return cfg.BotToken(secrets)
}

// healthTokenRef names, without resolving, the secret healthToken would read. It is
// for messages and tests; the value is never fetched through it.
func healthTokenRef(cfg *config.Config, sel unitSelection) config.SecretRef {
	if sel.member != "" {
		for _, mc := range cfg.Members {
			if mc.ID == sel.member {
				return mc.BotTokenRef()
			}
		}
	}
	return cfg.BotTokenRef()
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
	return func(_ context.Context, req keelupdate.ConsentRequest) (keelupdate.Decision, error) {
		fmt.Fprintf(e.stdout, "\nThis update needs your agreement before it is applied.\n")
		fmt.Fprintf(e.stdout, "  from: %s\n  to:   %s\n", req.From, req.To)
		if strings.TrimSpace(req.Notes) != "" {
			fmt.Fprintf(e.stdout, "\n%s\n", indent(strings.TrimSpace(req.Notes), "  "))
		}
		// Say which of the two reasons this is. "There is a major version" and
		// "this release changes security-relevant behaviour" call for different
		// amounts of thought, and collapsing them into one sentence would hide
		// the one that matters.
		if req.SecuritySensitive {
			fmt.Fprintf(e.stdout, "\nThis release is flagged as changing security-relevant behaviour — which can\n")
			fmt.Fprintf(e.stdout, "include routing policy or tier defaults, the settings that decide whether a\n")
			fmt.Fprintf(e.stdout, "private conversation may reach a provider. Read the notes above before\n")
			fmt.Fprintf(e.stdout, "agreeing.\n")
		} else {
			fmt.Fprintf(e.stdout, "\nThis is a major version. A release may never silently change routing policy or\n")
			fmt.Fprintf(e.stdout, "tier configuration, so it is asked rather than assumed.\n")
		}
		fmt.Fprintf(e.stdout, "\nApply it? [y/N] ")

		// Unanswered rather than declined wherever nobody actually said no: keel
		// re-asks on the next cycle for the first and remembers the second, and a
		// pipe that ended is not somebody deciding.
		if e.stdin == nil {
			fmt.Fprintf(e.stdout, "\nNo input available; not applying, and this will be asked again.\n")
			return keelupdate.DecisionUnanswered, nil
		}
		line, err := bufio.NewReader(e.stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintf(e.stdout, "\nNo answer; not applying, and this will be asked again.\n")
			return keelupdate.DecisionUnanswered, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return keelupdate.DecisionApproved, nil
		case "n", "no":
			return keelupdate.DecisionDeclined, nil
		default:
			// Anything else is not a yes, and is not a considered no either.
			fmt.Fprintf(e.stdout, "\nThat was not a yes or a no; not applying.\n")
			return keelupdate.DecisionUnanswered, nil
		}
	}
}
