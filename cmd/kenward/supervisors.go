package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
	"github.com/BlueHeisenberg/kenward/internal/version"
)

// defaultPodImageRepo is the repository the isolated host supervisor starts pods
// from when --image is not given.
//
// internal/supervisor requires an image and offers no default, deliberately: there
// is no sensible default for the artifact a household's private conversations run
// inside. What is defensible is running pods of the same build as the host process,
// which is what this produces, and an operator who wants otherwise says so with
// --image. A `dev` build has no published tag, so it is refused rather than guessed
// at.
const defaultPodImageRepo = "ghcr.io/blueheisenberg/kenward"

// podImageFileName is the file under the data directory recording which pod image
// the host supervisor last brought the household up on.
//
// It exists because the question cannot be asked of anything else. This host
// self-updates by swapping its own binary and exiting; the process that has to
// notice the pods are now a version behind is the next one, and keel's Inspect
// reports whether a container runs but never what image it runs. So the fact is
// written down. See supervisor.IsolatedOptions.ImageStatePath.
const podImageFileName = "pod-image"

// defaultSupervisor picks the supervisor from the configuration's mode and from
// whether this process was asked to be one unit.
//
// The three cases are distinct and none of them may quietly become another:
//
//   - simple, no unit selection: every unit as a goroutine in this process.
//   - isolated, no unit selection: the host supervisor, one pod per member. Linux
//     only; anywhere else it refuses rather than downgrading.
//   - isolated, one unit selected: this process is inside a pod and is that one
//     unit.
//
// simple with a unit selection is rejected before we get here, in cmdRun, because it
// is a usage error rather than a wiring decision.
func defaultSupervisor(e *env, cfg *config.Config, opts runOptions, logger *slog.Logger) (supervisor.Supervisor, error) {
	switch {
	case cfg.Mode == config.ModeIsolated && opts.selection.single():
		return newSingleUnitSupervisor(e, cfg, opts, logger)

	case cfg.Mode == config.ModeIsolated:
		iso, err := isolatedOptions(e, opts, logger)
		if err != nil {
			return nil, err
		}
		return supervisor.NewIsolated(cfg, iso)

	default:
		claimer, err := newClaimer(cfg, logger)
		if err != nil {
			return nil, err
		}
		// The session manager is built here rather than left for the supervisor
		// to build, because it has to be unlocked before any unit runs. A manager
		// nothing ever unlocks holds no key, and a node holding no key answers
		// every direct message with the locked notice while its group chat works
		// perfectly — which is the shape of failure nobody diagnoses.
		sessions, onEnrol, err := startSessions(e, cfg, logger, cfg.DomainMembers())
		if err != nil {
			return nil, err
		}
		return supervisor.NewSimple(cfg, supervisor.SimpleOptions{
			// Enrol is supplied so claim codes work while the household runs:
			// without it a member who has been handed a code has nothing to
			// present it to until the operator restarts the node.
			Enrol: claimer,
			// And UnlockOnEnrol so that claim finishes the job: the node
			// passphrase is this process's to use, so a member who claims now
			// gets the same provision-and-unlock a member present at startup
			// got.
			UnlockOnEnrol: onEnrol,
			Sessions:      sessions,
			Secrets:       e.secrets(),
			Logger:        logger,
			LookupEnv:     e.env(),
			Now:           e.now,
		})
	}
}

// isolatedOptions is the wiring defaultSupervisor hands to supervisor.NewIsolated,
// separated from the construction the way singleUnitOptions is and for a sharper
// reason: NewIsolated refuses on anything but Linux, so a test that went through it
// could not assert what this decides from any other machine.
func isolatedOptions(e *env, opts runOptions, logger *slog.Logger) (supervisor.IsolatedOptions, error) {
	image := opts.image
	if image == "" {
		v := version.Short()
		if v == "" || v == "dev" {
			return supervisor.IsolatedOptions{}, fmt.Errorf("isolated mode needs a pod image and this is a %q build, "+
				"which has no published tag: pass --image REF", v)
		}
		image = defaultPodImageRepo + ":" + v
	}
	// Beside the state file and the session store, under the same data directory,
	// because it is the same kind of thing: what this node knows about the household
	// it is running, rather than what the operator configured. Empty only if there is
	// no data directory at all, which config.ApplyDefaults does not permit.
	imageState := ""
	seeds := ""
	revocations := ""
	if opts.dataDir != "" {
		imageState = filepath.Join(opts.dataDir, podImageFileName)
		seeds = filepath.Join(opts.dataDir, inviteSeedDirName)
		revocations = filepath.Join(opts.dataDir, revocationDirName)
	}
	return supervisor.IsolatedOptions{
		Image: image,
		// D-023's last mile. `kenward invite` writes each member's outstanding codes
		// to their own file under here; this is what carries that file into that
		// member's pod, where the claim is actually redeemed. Without it the operator
		// mints a code on this machine, hands it over, and the member's pod — which
		// reads its own store on its own volume and has never seen this one — refuses
		// it in the silence enrolment owes a stranger.
		InviteSeedDir: seeds,
		// The same last mile in the other direction, and the only one revocation has.
		// `kenward revoke` records a member's revocation here because the binding it
		// has to clear was written inside that member's pod, on a volume this host
		// must not write; this is what carries the record in, and the pod clears its
		// own binding on the way up. Without it `kenward revoke` empties a host record
		// no pod has ever read, reports success, and the pod goes on serving the
		// account the operator believes they have just cut off.
		RevocationDir: revocations,
		// Without this the pods never move. `ensureRunning` starts a pod that exists
		// and creates one that does not; nothing in the restart path replaces a
		// running container, so after this host self-updated and came back on the new
		// binary every member's pod would keep serving from the previous image
		// forever — the household node upgraded and its members silently did not,
		// which is worse than not updating at all, because the operator has every
		// reason to believe the update completed. supervisor.Roll is the one thing
		// that fixes that and it had no caller; this is the caller. See
		// docs/IMPLEMENTATION.md §9, "one member at a time".
		ImageStatePath: imageState,
		// D-022: the two isolated deployment paths must express the same thing. A
		// non-empty ConfigFile is what makes that true — the household configuration
		// is provisioned into every pod at supervisor.PodConfigPath and the pod is
		// started with the compose-identical argv, instead of the image's own CMD.
		//
		// Left empty, as it was, a pod ran that CMD: `run --config
		// /etc/kenward/kenward.yaml --data-dir /var/lib/kenward` (see the Dockerfile),
		// against a path this deployment path never provisions. Every pod exited on a
		// missing configuration and was restarted forever. The `--data-dir` in that
		// same CMD is the quieter half: an explicit flag beats the KENWARD_DATA_DIR
		// the supervisor sets, so a pod that did somehow start would keep its member's
		// wrapped key off /work — the one volume Recreate preserves — and the first
		// rolling update would take it.
		//
		// The whole household file goes in, not a per-member slice of it, because that
		// is what the compose path mounts and because there is nothing in it to
		// withhold: configuration names secrets and never holds one, and Isolated
		// provisions only this pod's own token beside it. What a member's pod gains is
		// the household roster — ids, spaces, and the *names* of other members' token
		// variables — which grants no access to any of them.
		//
		// The path is used on the host and never travels into a pod, so unlike
		// bot_token_file it needs no absolute form: relative resolves against the same
		// working directory loadConfig already read it from.
		ConfigFile: opts.configPath,
		// The same resolver this command's own checks use, so a pod's token
		// is read from whichever source the configuration names rather than
		// from the environment alone.
		Secrets:   e.secrets(),
		Logger:    logger,
		LookupEnv: e.env(),
		Now:       e.now,
	}, nil
}

// startSessions reads the passphrase, provisions any of the given members who has no
// key yet, unlocks them, and hands back the manager the units will use together with
// the hook that does the same for one member who claims later.
//
// members is passed in rather than read from cfg because the two callers serve
// different sets: simple mode serves the whole household from one process, and a pod
// serves exactly one member. Handing a pod the household's member list would have it
// provision keys for people it must never hold. The hook carries that same rule
// forward past startup: the supervisor calls it only for a member this process is
// entitled to serve.
//
// The hook closes over the passphrase, which is what makes mid-run enrolment work and
// is a real cost, so it is stated rather than buried. The byte buffer read from the
// credential file is still zeroed on return — that part of the passphrase type's
// promise is unchanged. What survives is the string copy reveal() already made and
// that only the garbage collector could ever have reclaimed: it now lives as long as
// the process. Against that, this process already holds every one of these members'
// keys unwrapped in the same memory for its whole life, which D-019 says plainly, so
// what a memory dump additionally learns is the passphrase itself rather than
// anything it protects here. The alternative is a household where onboarding ends in
// a lock message.
func startSessions(e *env, cfg *config.Config, logger *slog.Logger, members []domain.Member) (session.Sessions, supervisor.UnlockOnEnrol, error) {
	pass, err := readPassphrase(e, passphraseRefFor(cfg, members))
	if err != nil {
		if errors.Is(err, errNoPassphrase) {
			return nil, nil, errNoPassphrase
		}
		return nil, nil, err
	}
	defer pass.zero()
	secret := pass.reveal()

	store := session.NewFileStore(sessionStorePath(cfg))
	mgr, err := session.NewManager(sessionMode(cfg.Mode), store,
		session.WithIdleTimeout(cfg.Session.IdleTimeout.Duration()))
	if err != nil {
		return nil, nil, fmt.Errorf("building the session manager: %w", err)
	}

	rep, err := unlockSessions(e.context(), mgr, store, members, secret)
	if err != nil {
		mgr.Close()
		// session.ErrBadPassphrase is deliberately indistinguishable from an
		// unknown member, so this cannot say which of the two it was — only that
		// the passphrase this node was given does not open what is on disk.
		if errors.Is(err, session.ErrBadPassphrase) {
			return nil, nil, fmt.Errorf("the passphrase from %s does not unwrap the keys in %s.\n"+
				"kenward will not start with keys it cannot open: it would answer every private\n"+
				"message with the locked notice", pass.source, store.Path())
		}
		return nil, nil, err
	}
	// Ids only. The passphrase appears in no log line, here or anywhere.
	logger.Info("kenward",
		"event", "sessions",
		"unlocked", len(rep.Unlocked),
		"provisioned", len(rep.Provisioned),
		"custody", sessionMode(cfg.Mode).String(),
		"source", pass.source)

	onEnrol := func(ctx context.Context, m domain.Member) error {
		_, err := unlockSessions(ctx, mgr, store, []domain.Member{m}, secret)
		return err
	}
	return mgr, onEnrol, nil
}

// passphraseRefFor names the passphrase source this process's configuration states, or
// nil when it states none and readPassphrase's own mechanisms are the whole answer.
//
// It is derived here rather than passed in because the rule is exactly the shape of
// what the two callers already differ by:
//
//   - Isolated mode serving one member is a pod, and a pod unwraps that member's key
//     under that member's own passphrase — members[].passphrase_env / _file.
//   - Simple mode serves the household from one process under one node passphrase, and
//     session.passphrase_env / _file is where a file may now name it. Optional, so a
//     household using the systemd credential, KENWARD_PASSPHRASE or a terminal states
//     nothing and this resolves to NotFound, which readPassphrase treats as "carry on".
//   - Isolated mode's group pod holds no key at all, so it gets nil: a node passphrase
//     there would unwrap nothing, and reading one would be reading a secret it has no
//     business holding.
func passphraseRefFor(cfg *config.Config, members []domain.Member) *config.SecretRef {
	if cfg.Mode != config.ModeIsolated {
		ref := cfg.SessionPassphraseRef()
		return &ref
	}
	if len(members) != 1 {
		return nil
	}
	for _, mc := range cfg.Members {
		if domain.MemberID(mc.ID) != members[0].ID {
			continue
		}
		ref := mc.PassphraseRef()
		return &ref
	}
	return nil
}

// newClaimer builds the enrolment claimer the running node uses to process claim
// codes from senders it does not yet serve.
func newClaimer(cfg *config.Config, logger *slog.Logger) (*enrol.Claimer, error) {
	binder, err := newBinder(cfg)
	if err != nil {
		return nil, err
	}
	// The explanation's third message describes the buttons this household will
	// actually show, so it has to know which policy is in force. The household's
	// language is the one the greeting arrives in — it is sent before the member has
	// been able to say anything — and it is what a member who skips the language
	// question inherits.
	opts := []enrol.Option{
		enrol.WithPersonas(enrol.BinderPersonas(binder)),
		enrol.WithLogger(logger),
		enrol.WithLanguage(cfg.Household.Persona.Language),
		// The explanation's first message tells a member who can read what they say
		// here, and that answer is the mode's. Sealing against the operator exists
		// only in isolated mode, so only an isolated household is told about it.
		enrol.WithPrivacyMode(privacyModeFor(cfg.Mode)),
	}
	if cfg.Capture.PrivateWrites == config.PrivateWriteAsk {
		opts = append(opts, enrol.WithAskPrivateWrites())
	}
	// Through AgentPerMember rather than the field: the tutorial asks a member to
	// name their agent only where they are getting one, and simple mode answers false
	// however household.agents is written. A member asked to name an assistant that
	// no arrangement gives them would be answering a question with no consequence.
	if cfg.AgentPerMember() {
		opts = append(opts, enrol.WithOneEach())
	}
	c, err := enrol.New(inviteStore(cfg), binder, opts...)
	if err != nil {
		return nil, fmt.Errorf("building the enrolment claimer: %w", err)
	}
	return c, nil
}
