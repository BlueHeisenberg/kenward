package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
)

func cmdInvite(e *env, args []string) int {
	fs := newFlagSet(e, "invite", "kenward invite --name NAME [--ttl 24h] [--config PATH] [--data-dir PATH]")
	name := fs.String("name", "", "the person this code is for, as they are known in the household")
	ttl := fs.Duration("ttl", enrol.DefaultTTL, "how long the code stays redeemable")
	configPath := fs.String("config", "", "path to kenward.yaml")
	dataDir := fs.String("data-dir", "", "override the data directory")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if strings.TrimSpace(*name) == "" {
		e.errorf("invite needs --name NAME")
		fs.Usage()
		return exitUsage
	}
	if *ttl <= 0 {
		e.errorf("--ttl must be positive; a code that has already expired is not an invite")
		return exitUsage
	}

	path := resolveConfigPath(e, *configPath)
	cfg, err := loadConfig(path, resolveDataDir(e, *dataDir), e.secrets())
	if err != nil {
		fmt.Fprint(e.stderr, renderConfigError(path, err))
		return exitUsage
	}

	// The member has to be declared before a code is minted for them. A code that
	// cannot bind is worse than no code: it is handed to a person, it fails in
	// their chat with no explanation the operator can see, and the failure looks
	// like enrolment rather than a missing four lines of YAML.
	member, ok := resolveDeclaredMember(cfg, *name)
	if !ok {
		e.errorf("%s", undeclaredMemberHelp(cfg, path, *name))
		return exitUsage
	}
	if member.Enrolled() {
		e.errorf("%s has already claimed an invite. Run `kenward revoke %s` first if they need to\n"+
			"bind a different Telegram account.", member.Name, member.ID)
		return exitUsage
	}

	// Minting needs no Binder: `kenward invite` writes a digest and never enrols
	// anyone. Handing it one would give a command that only prints a code the
	// ability to change who is served.
	claimer, err := enrol.New(inviteStore(cfg), nil, enrol.WithTTL(*ttl))
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}
	// The code is minted against the declared member's own name, so that the id
	// enrol derives from it is the id the configuration already uses.
	code, err := claimer.Mint(e.context(), member.Name, *ttl)
	if err != nil {
		e.errorf("minting a claim code: %v", err)
		return exitFailure
	}

	// And now the digest has to reach the process that will be asked to redeem it. In
	// simple mode that is this machine's own store, already written. In isolated mode
	// it is the member's pod, which reads its own store on its own volume and has no
	// sight of this one, so the digest is exported to that member's seed file for the
	// deployment to carry in — see inviteSeedDirName. Without it the operator hands
	// over a code the pod has never heard of and the claim is refused in silence,
	// which is the one failure mode enrolment cannot explain to the person hitting it.
	if cfg.Mode == config.ModeIsolated {
		seed := inviteSeedStore(inviteSeedDir(cfg), member.ID)
		now := e.now()
		// Two ids for one person, because Mint derives one from the name while the
		// pod is found by the id the configuration gave them, and a household whose
		// `id: dave` carries `name: David` has both. Matching only one of them would
		// write an empty seed and report success — the exact failure this whole file
		// exists to stop, arriving one layer down. The file itself is named for the
		// configured id: that is what the supervisor and the compose file look up.
		minted := enrol.MemberIDFor(member.Name)
		_, err := copyInvites(e.context(), seed, inviteStore(cfg), func(c enrol.Code) bool {
			return c.Live(now) && (c.MemberID == member.ID || c.MemberID == minted)
		})
		if err != nil {
			// The code exists in this node's store and will not reach the pod, so it
			// is useless. Say so rather than printing it: an unusable code handed to a
			// person fails in their chat, where nobody can see why.
			e.errorf("the code was minted but could not be written to %s, so %s's pod will\n"+
				"never see it: %v\n\n"+
				"Fix the path and run this again. The unusable code expires on its own.",
				seed.Path(), member.Name, err)
			return exitFailure
		}
	}

	fmt.Fprint(e.stdout, renderInvite(member.Name, code, *ttl))
	if cfg.Mode == config.ModeIsolated {
		fmt.Fprint(e.stdout, renderIsolatedDelivery(member.ID))
	}
	return exitOK
}

// renderIsolatedDelivery is the extra line isolated mode needs.
//
// A pod is handed its member's invites when the container is created, so a code minted
// while that container is already running is on the host and nowhere else until it is
// recreated. That is the same staleness the host's own view of enrolment has, and it is
// the operator's to act on, so it is said here rather than discovered when the member
// reports that nothing happened.
func renderIsolatedDelivery(id domain.MemberID) string {
	return fmt.Sprintf("\nThis household is isolated, so the code travels to %s's pod when that pod is\n"+
		"next created. If it is already running, restart kenward before handing the code over.\n", id)
}

// renderInvite is docs/CLI.md's invite output.
//
// It prints the code and the three facts a person handing it over needs: it works
// once, it expires, and until it is used the bot will not reply to that person at
// all. Nothing else — no QR, no deep link, nothing that would leak the code into a
// chat log, and no second copy anywhere: this is the only moment the code exists in
// the clear, and losing it means minting another.
func renderInvite(name, code string, ttl time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Claim code for %s:\n\n", name)
	fmt.Fprintf(&b, "    %s\n\n", code)
	fmt.Fprintf(&b, "Give this to %s and have them message the bot. It works once and expires in %s.\n",
		name, humanDuration(ttl))
	fmt.Fprintf(&b, "Until they use it, the bot will not reply to them at all.\n")
	return b.String()
}

// humanDuration renders a TTL the way somebody says it out loud.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return countOf(int(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return countOf(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return countOf(int(d/time.Minute), "minute")
	default:
		return d.String()
	}
}

func countOf(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
