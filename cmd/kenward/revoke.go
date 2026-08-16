package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
)

func cmdRevoke(e *env, args []string) int {
	fs := newFlagSet(e, "revoke", "kenward revoke MEMBER [--config PATH] [--data-dir PATH]")
	configPath := fs.String("config", "", "path to kenward.yaml")
	dataDir := fs.String("data-dir", "", "override the data directory")
	positionals, code, ok := parseWithPositionals(e, fs, args)
	if !ok {
		return code
	}
	if len(positionals) != 1 {
		e.errorf("revoke takes exactly one member id")
		fs.Usage()
		return exitUsage
	}
	id := domain.MemberID(positionals[0])

	path := resolveConfigPath(e, *configPath)
	cfg, err := loadConfig(path, resolveDataDir(e, *dataDir), e.secrets())
	if err != nil {
		fmt.Fprint(e.stderr, renderConfigError(path, err))
		return exitUsage
	}

	// Before anything is cleared: a telegram_id written into kenward.yaml by hand is
	// not kenward's to remove, and clearing the state file around it revokes nothing.
	// See declaredTelegramID.
	switch declared, derr := declaredTelegramID(path, id); {
	case derr != nil:
		e.errorf("re-reading %s: %v", path, derr)
		return exitFailure
	case declared != 0:
		e.errorf("%s", declaredTelegramIDHelp(path, id, declared))
		return exitUsage
	}

	binder, err := newBinder(cfg)
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}
	claimer, err := enrol.New(inviteStore(cfg), binder)
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}

	rev, err := claimer.Revoke(e.context(), id)
	if err != nil {
		if errors.Is(err, enrol.ErrUnknownMember) {
			e.errorf("%v", err)
			return exitUsage
		}
		e.errorf("revoking %s: %v", id, err)
		return exitFailure
	}

	// In isolated mode the binding this was supposed to clear is not here. The claim
	// was redeemed inside the member's own pod, against the state file on that pod's
	// own volume, and this process has cleared a record that pod has never read: the
	// operator saw success and the pod carried on serving the revoked account, which
	// is worse than a visible failure. What crosses is a record the pod applies when
	// it is next created — the same one-way, create-time channel a claim code takes,
	// for the same reason. See revocationDirName.
	//
	// It is not conditional on the host believing the member enrolled, and must not
	// be: in this mode the host cannot know either way, which is the whole point.
	recorded := ""
	if cfg.Mode == config.ModeIsolated {
		recorded, err = writeRevocation(revocationDir(cfg), rev.Member.ID, e.now())
		if err != nil {
			e.errorf("the revocation could not be written where %s's pod will be given it: %v\n\n"+
				"Nothing has been revoked: that pod holds the binding and this is the only way to\n"+
				"reach it. Fix the path and run this again.", rev.Member.Name, err)
			return exitFailure
		}
		rev.Deferred = true
	}

	fmt.Fprint(e.stdout, renderRevocation(rev))
	fmt.Fprint(e.stdout, renderRevocationDelivery(rev.Member.ID, recorded))
	return exitOK
}

// declaredTelegramID reports the telegram_id kenward.yaml itself states for a member,
// which is a different question from the one the loaded configuration answers.
//
// The loaded configuration has already had the state file merged into it, so a member
// carries an id whether an operator wrote one down or a claim recorded one. Only the
// second is kenward's to clear. Unbinding around a hand-written telegram_id empties the
// state file, prints a revocation, and serves the same account again on the next start,
// because the file the operator owns still names it — the same silent success this
// command has in isolated mode, arriving by a different route and in both modes.
//
// So the file is read again, undecorated. A second decode of a small YAML file is a
// cheap way to be sure which of the two sources an id came from.
func declaredTelegramID(path string, id domain.MemberID) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		return 0, err
	}
	for _, m := range cfg.Members {
		if domain.MemberID(m.ID) == id {
			return m.TelegramID, nil
		}
	}
	return 0, nil
}

// declaredTelegramIDHelp names the one line to delete.
//
// kenward does not rewrite kenward.yaml — that file is the operator's, comments and
// all — so this refuses rather than half-acting, and refuses before anything has been
// cleared so that running it again after the edit starts from an unchanged household.
func declaredTelegramIDHelp(path string, id domain.MemberID, telegramID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s declares telegram_id: %d for member %s, and kenward does not rewrite your\n", path, telegramID, id)
	fmt.Fprintf(&b, "configuration. Clearing the enrolment record around it would revoke nothing: the\n")
	fmt.Fprintf(&b, "next start would read that line and serve the account again.\n\n")
	fmt.Fprintf(&b, "Delete this line from members[%s]:\n\n", id)
	fmt.Fprintf(&b, "      telegram_id: %d\n\n", telegramID)
	fmt.Fprintf(&b, "then run this command again. A telegram_id belongs in the enrolment record, which\n")
	fmt.Fprintf(&b, "a claim writes and this command clears; written by hand it is a permission only\n")
	fmt.Fprintf(&b, "you can withdraw.\n")
	return b.String()
}

// renderRevocation says what was done and, at least as prominently, what was not.
//
// The key-rotation sentence comes from enrol.Revocation.Warning rather than from
// here. kenward cannot rotate a lore space key — it has no authority over one — and a
// revocation that reads as complete when the member may still hold a working key is a
// false security claim. Those are worse than no claim at all, because somebody acts
// on them.
func renderRevocation(rev enrol.Revocation) string {
	var b strings.Builder
	// enrol.Revocation.Warning already names the space and says the key has not
	// been rotated. Restating it here in different words would be a second copy of
	// a security claim, and two copies is how one of them drifts.
	fmt.Fprintf(&b, "%s\n", rev.Warning())
	if rev.KeyRotationRequired() {
		fmt.Fprintf(&b, "\nThis command has not rotated it and cannot: kenward has no authority over a lore\n")
		fmt.Fprintf(&b, "space key. Until you rotate it in lore yourself, the revocation is partial.\n")
	}
	return b.String()
}

// renderRevocationDelivery is when this takes effect, which is not now in either mode.
//
// A running node decided who it serves when it started: simple mode built its units and
// its member set then, and isolated mode's pods hold their own bindings and read the
// record this wrote only when they are created. Neither notices a file changing under
// them, and there is no channel from this process to a running one — so the restart is
// the operator's to perform, and saying so is the difference between a revocation that
// happens and one that is believed to have happened.
func renderRevocationDelivery(id domain.MemberID, recorded string) string {
	if recorded == "" {
		return "\nIf kenward is running, restart it. A running node decided who it serves when it\n" +
			"started and does not re-read this while it runs.\n"
	}
	return fmt.Sprintf("\nThis household is isolated, so %s's binding lives in %s's own pod and this command\n"+
		"cannot reach it. The revocation is recorded at\n\n    %s\n\n"+
		"and that pod applies it the next time it is created. Restart kenward now — until\n"+
		"you do, that pod is still serving them.\n", id, id, recorded)
}
