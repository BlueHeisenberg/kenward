package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// davidsPodEnvironment is what a member's container actually holds: that member's own
// bot token and nothing else. No sibling's, and not the household's — deploy's compose
// file goes to some length to keep it that way, and D-007 is the reason.
func davidsPodEnvironment() map[string]string {
	return map[string]string{"KENWARD_BOT_TOKEN_DAVID": fakeDavidToken}
}

// TestRunStartsAMemberPodHoldingOnlyItsOwnToken is the defect this scoping fixes.
//
// Both isolated deployment paths (D-022) give a pod the whole household configuration
// and one unit's secrets. Validating the household's secrets in that pod refused every
// pod on the tokens it correctly did not have, and the only way to satisfy the check was
// to put every member's token in every member's container — precisely the failure the
// mode exists to prevent. The compose file shipped in deploy/ could not start.
func TestRunStartsAMemberPodHoldingOnlyItsOwnToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		vars map[string]string
	}{{
		name: "the compose path, member selected by flag",
		args: []string{"run", "--member", "david"},
		vars: davidsPodEnvironment(),
	}, {
		name: "the supervisor path, member selected by environment",
		args: []string{"run"},
		vars: map[string]string{
			"KENWARD_BOT_TOKEN_DAVID": fakeDavidToken,
			supervisor.EnvMember:      "david",
		},
	}, {
		name: "the group's pod, which holds the household token and no member's",
		args: []string{"run", "--group"},
		vars: map[string]string{"KENWARD_BOT_TOKEN_HOUSEHOLD": fakeGroupToken},
	}, {
		name: "the group's pod by environment",
		args: []string{"run"},
		vars: map[string]string{
			"KENWARD_BOT_TOKEN_HOUSEHOLD": fakeGroupToken,
			supervisor.EnvGroup:           "1",
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, isolatedYAML, tc.vars)
			h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				return stubSupervisor{}, nil
			}
			if code := h.run(tc.args...); code != exitOK {
				t.Fatalf("exit = %d, want 0: a pod must start on its own unit's secrets\n%s", code, h.both())
			}
			h.assertNoSecrets(t)
		})
	}
}

// TestRunStillRefusesAHouseholdNodeMissingAToken is the other half, and the one that
// must not regress. A process that runs every unit needs every unit's token: starting
// one member short would serve everybody else and drop that member in silence, which is
// the case the refusal was written for.
func TestRunStillRefusesAHouseholdNodeMissingAToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, davidsPodEnvironment())
	h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
		t.Error("the supervisor was built for a household node with two tokens missing")
		return stubSupervisor{}, nil
	}
	if code := h.run("run"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	for _, want := range []string{"KENWARD_BOT_TOKEN_HOUSEHOLD", "KENWARD_BOT_TOKEN_JORDAN"} {
		if !strings.Contains(h.stderr(), want) {
			t.Errorf("stderr does not name the missing %s:\n%s", want, h.stderr())
		}
	}
	h.assertNoSecrets(t)
}

// TestRunRefusesAPodMissingItsOwnToken: scoping narrows what is demanded, it does not
// excuse it. A pod with no token of its own has no way to receive a message.
func TestRunRefusesAPodMissingItsOwnToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, davidsPodEnvironment())
	if code := h.run("run", "--member", "jordan"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	if !strings.Contains(h.stderr(), "KENWARD_BOT_TOKEN_JORDAN") {
		t.Errorf("stderr does not name jordan's own missing token:\n%s", h.stderr())
	}
	if strings.Contains(h.stderr(), "KENWARD_BOT_TOKEN_DAVID") {
		t.Errorf("jordan's pod was asked for david's token:\n%s", h.stderr())
	}
}

// TestDoctorInAPodIsHealthy: the container's HEALTHCHECK runs `doctor` with no arguments
// of its own, so it takes the unit from the environment the pod already carries. Without
// that it would fail on every sibling secret the container correctly does not hold, and
// restart a working pod every thirty seconds forever.
func TestDoctorInAPodIsHealthy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vars map[string]string
		args []string
		// absent are the things this unit's report must not mention: another
		// member's name, or their private space.
		absent []string
	}{{
		name:   "david's pod, unit from the environment as the healthcheck sees it",
		vars:   map[string]string{"KENWARD_BOT_TOKEN_DAVID": fakeDavidToken, supervisor.EnvMember: "david"},
		args:   []string{"doctor"},
		absent: []string{"Jordan", "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19"},
	}, {
		name:   "david's pod, unit from the flag",
		vars:   davidsPodEnvironment(),
		args:   []string{"doctor", "--member", "david"},
		absent: []string{"Jordan", "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19"},
	}, {
		name:   "the group's pod",
		vars:   map[string]string{"KENWARD_BOT_TOKEN_HOUSEHOLD": fakeGroupToken, supervisor.EnvGroup: "1"},
		args:   []string{"doctor"},
		absent: []string{"David", "Jordan", "7d5047bb-d939-4539-b3db-8b6221a2e245"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, isolatedYAML, tc.vars)
			if code := h.run(tc.args...); code != exitOK {
				t.Fatalf("exit = %d, want 0: a pod holding its own unit's secrets is healthy\n%s", code, h.both())
			}
			out := h.stdout()
			if !strings.Contains(out, "this pod runs only") {
				t.Errorf("the report does not say which unit it is about:\n%s", out)
			}
			for _, s := range tc.absent {
				if strings.Contains(out, s) {
					t.Errorf("a pod's report mentions %q, which belongs to another pod:\n%s", s, out)
				}
			}
			h.assertNoSecrets(t)
		})
	}
}

// TestDoctorHouseholdWideStillFails: the zero selection is unchanged, so an operator
// running `doctor` at a terminal on an incomplete household is still told so.
func TestDoctorHouseholdWideStillFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, davidsPodEnvironment())
	if code := h.run("doctor"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	out := h.stdout()
	if strings.Contains(out, "this pod runs only") {
		t.Errorf("a household-wide report claimed to be a pod's:\n%s", out)
	}
	for _, want := range []string{"members[1].bot_token", "telegram.bot_token"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name the missing %s:\n%s", want, out)
		}
	}
	h.assertNoSecrets(t)
}
