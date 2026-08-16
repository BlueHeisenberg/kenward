package main

import (
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// unitSelection is which single unit this process was asked to be, if any.
//
// It exists for isolated mode, where each pod runs exactly one unit: a member's pod
// is david's, the household's is the group's. In simple mode neither is given and one
// process runs every unit.
type unitSelection struct {
	member string
	group  bool
	// source names where the selection came from, for error messages.
	source string
}

func (u unitSelection) single() bool { return u.member != "" || u.group }

// scope is this selection as internal/config's, for the checks that depend on which
// unit is being run — which secrets have to resolve, above all. The zero selection
// becomes the zero scope, the whole household.
func (u unitSelection) scope() config.UnitScope {
	return config.UnitScope{Member: u.member, Group: u.group}
}

func (u unitSelection) label() string {
	if u.group {
		return "the household group"
	}
	return "member " + u.member
}

// flagName is the flag this selection would have been spelled with, for the
// simple-mode refusal.
func (u unitSelection) flagName() string {
	if u.group {
		return "--group"
	}
	return "--member"
}

func (u unitSelection) describe() string {
	if u.group {
		return "the household group"
	}
	if u.member != "" {
		return "member " + u.member
	}
	return "no unit"
}

// resolveUnitSelection decides which unit this process serves, from the flags and
// from the environment, in that order of precedence.
//
// Isolated mode has two deployment paths and they select a unit differently. An
// operator's compose file runs one container per member and passes `--member=david`
// as an argument; the host supervisor starts pods through keel's sandbox and passes
// KENWARD_MEMBER or KENWARD_GROUP as environment. Both must work: environment
// configuration is the norm for containers, and an explicit argument should win when
// somebody types one.
//
// Where the two sources disagree, nothing wins. A silent precedence rule here would
// mean a pod quietly serving a member other than the one its operator believes,
// which surfaces as one person's private memory appearing in another's conversation
// — a very long way from where the mistake was made. Saying which two sources
// disagreed, and what each of them said, costs one error message and saves that.
func resolveUnitSelection(flagMember string, flagGroup bool, lookupEnv config.LookupEnvFunc) (unitSelection, error) {
	flagMember = strings.TrimSpace(flagMember)
	if flagMember != "" && flagGroup {
		return unitSelection{}, fmt.Errorf(
			"--member %s and --group both given: a pod runs exactly one unit, so there is no\n"+
				"reading of that command line where both are true", flagMember)
	}

	envMember := ""
	if v, ok := lookupEnv(supervisor.EnvMember); ok {
		envMember = strings.TrimSpace(v)
	}
	envGroup, err := envFlagSet(lookupEnv, supervisor.EnvGroup)
	if err != nil {
		return unitSelection{}, err
	}
	if envMember != "" && envGroup {
		return unitSelection{}, fmt.Errorf(
			"%s=%s and %s are both set in the environment: a pod runs exactly one unit",
			supervisor.EnvMember, envMember, supervisor.EnvGroup)
	}

	fromFlags := unitSelection{member: flagMember, group: flagGroup, source: "the command line"}
	fromEnv := unitSelection{member: envMember, group: envGroup, source: "the environment"}

	switch {
	case !fromFlags.single() && !fromEnv.single():
		return unitSelection{}, nil
	case fromFlags.single() && !fromEnv.single():
		return fromFlags, nil
	case !fromFlags.single() && fromEnv.single():
		return fromEnv, nil
	case fromFlags.member == fromEnv.member && fromFlags.group == fromEnv.group:
		// They agree. The flag is the one that was typed, so it is the one named.
		return fromFlags, nil
	default:
		return unitSelection{}, fmt.Errorf(
			"the command line and the environment disagree about which unit this process serves:\n"+
				"  the command line says %s (%s)\n"+
				"  the environment says %s (%s)\n"+
				"kenward will not pick one. A pod serving a member other than the one its operator\n"+
				"believes shows up as somebody's private memory in the wrong conversation, a long\n"+
				"way from here. Remove whichever of the two is wrong.",
			fromFlags.describe(), flagSpelling(fromFlags),
			fromEnv.describe(), envSpelling(fromEnv))
	}
}

func flagSpelling(u unitSelection) string {
	if u.group {
		return "--group"
	}
	return "--member " + u.member
}

func envSpelling(u unitSelection) string {
	if u.group {
		return supervisor.EnvGroup + "=1"
	}
	return supervisor.EnvMember + "=" + u.member
}

// envFlagSet reads a boolean environment variable.
//
// internal/supervisor documents KENWARD_GROUP's value as "1". Empty, "0" and "false"
// are read as absent, because a compose file that sets a variable to nothing is a
// compose file that meant to leave it out. Anything else is a usage error rather than
// a guess: KENWARD_GROUP=david is somebody who meant KENWARD_MEMBER.
func envFlagSet(lookupEnv config.LookupEnvFunc, name string) (bool, error) {
	v, ok := lookupEnv(name)
	if !ok {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q is not a yes or a no; set it to 1 for the household group's pod, or leave it out", name, v)
	}
}
