package main

import (
	"errors"
	"fmt"
	"strings"

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

	fmt.Fprint(e.stdout, renderRevocation(rev))
	return exitOK
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
