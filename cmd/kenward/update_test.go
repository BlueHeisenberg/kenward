package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	keelupdate "github.com/BlueHeisenberg/keel/update"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

func keelVersion(major, minor, patch int) keelupdate.Version {
	return keelupdate.Version{Major: major, Minor: minor, Patch: patch}
}

// TestUpdateSaysSoWhenTheChannelIsOff.
//
// docs/CLI.md: prints the channel in use, and says so plainly when it is off.
// `channel: off` is a fully supported way to run kenward forever, not a degraded
// state waiting to be fixed, and the wording has to read that way.
func TestUpdateSaysSoWhenTheChannelIsOff(t *testing.T) {
	t.Parallel()
	off := simpleYAML + "update:\n  channel: off\n"
	h := newHarness(t, off, fullEnvironment())

	if code := h.run("update"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "Update channel: off") {
		t.Errorf("update does not print the channel:\n%s", out)
	}
	for _, want := range []string{"never fetch anything", "works indefinitely"} {
		if !strings.Contains(out, want) {
			t.Errorf("update does not say off is supported (%q):\n%s", want, out)
		}
	}
}

// TestUpdateCheckPrintsTheChannel: --check reports and changes nothing.
//
// This build has no release signing keys compiled in, so it refuses before touching
// the network — which is the correct behaviour and is what this asserts. An updater
// that cannot verify a signature must refuse rather than fetch: it is a remote code
// execution channel into somebody's house.
func TestUpdateRefusesWithoutTrustedKeys(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(releaseTrustedKeys) != "" {
		t.Skip("this build has release keys compiled in")
	}
	h := newHarness(t, simpleYAML, fullEnvironment())

	if code := h.run("update", "--check"); code != exitFailure {
		t.Fatalf("exit = %d, want %d\n%s", code, exitFailure, h.both())
	}
	if !strings.Contains(h.stdout(), "Update channel: stable") {
		t.Errorf("update did not print the channel before refusing:\n%s", h.stdout())
	}
	for _, want := range []string{"no release signing keys", "cannot verify", "Refusing"} {
		if !strings.Contains(h.stderr(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, h.stderr())
		}
	}
}

// TestTrustedKeysRejectsRubbish. A key that is not a key is a build fault, not
// something to fetch a manifest with anyway.
func TestTrustedKeysRejectsRubbish(t *testing.T) {
	t.Parallel()

	if _, err := parseTrustedKeys("not base64 at all!!"); err == nil {
		t.Error("a non-base64 key was accepted")
	}
	if _, err := parseTrustedKeys("dG9vIHNob3J0"); err == nil { // valid base64, wrong length
		t.Error("a key of the wrong length was accepted")
	}

	keys, err := parseTrustedKeys("")
	if err != nil || len(keys) != 0 {
		t.Errorf("an empty key list should be no keys and no error; got %v, %v", len(keys), err)
	}

	// A real pair of keys, so rotation — ship a build trusting both, start signing
	// with the new one, drop the old one a release later — is known to parse.
	one := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	two := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, ed25519.PublicKeySize))
	keys, err = parseTrustedKeys(one + " , " + two)
	if err != nil {
		t.Fatalf("two valid keys were rejected: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("got %d keys, want 2: a build must be able to trust an old and a new key at once", len(keys))
	}
}

// TestHealthNeverProbesAnEndpoint.
//
// This is the one that matters beyond tidiness. keel/update's health check decides
// whether a freshly swapped binary is kept or rolled back, and a household's
// inference machines are legitimately switched off most of the time. If endpoint
// reachability were part of health, a perfectly good update would be rolled back,
// re-applied on the next check, and rolled back again — forever, ending in a wedged
// installation. So the endpoint probe must not be reached at all.
func TestHealthNeverProbesAnEndpoint(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes.endpoint = func(context.Context, routing.Endpoint) endpointResult {
		t.Error("health probed an endpoint; a sleeping household machine would roll back a good update")
		return endpointResult{}
	}
	cfg := mustLoad(t, simpleYAML)

	probes := nodeHealthProbes(h.e, cfg, unitSelection{})
	if err := probes.Lore(h.e.context()); err != nil {
		t.Errorf("lore probe: %v", err)
	}
	if err := probes.Telegram(h.e.context()); err != nil {
		t.Errorf("telegram probe: %v", err)
	}
}

// TestHealthFailsOnTheSameThingsDoctorDoes: lore down or Telegram refusing are the
// conditions under which this process cannot do its job at all.
func TestHealthFailsOnTheSameThingsDoctorDoes(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, simpleYAML)

	t.Run("lore down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		h.e.probes = loreDown(healthyProbes())
		if err := nodeHealthProbes(h.e, cfg, unitSelection{}).Lore(h.e.context()); err == nil {
			t.Error("health passed with lore down")
		}
	})
	t.Run("telegram refuses", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		h.e.probes = telegramRefuses(healthyProbes())
		if err := nodeHealthProbes(h.e, cfg, unitSelection{}).Telegram(h.e.context()); err == nil {
			t.Error("health passed with Telegram refusing the token")
		}
	})
	t.Run("every endpoint asleep is still healthy", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		h.e.probes = everythingAsleep(healthyProbes())
		p := nodeHealthProbes(h.e, cfg, unitSelection{})
		if err := p.Lore(h.e.context()); err != nil {
			t.Errorf("lore: %v", err)
		}
		if err := p.Telegram(h.e.context()); err != nil {
			t.Errorf("telegram: %v", err)
		}
	})
}

// TestHealthChecksThePodsOwnToken.
//
// A member's pod holds that member's token and nothing else. Checking the
// household's token there would test a credential the process does not have, and
// would fail health on a perfectly good pod — which, since health decides rollback,
// would roll every pod back forever.
func TestHealthChecksThePodsOwnToken(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, isolatedYAML)

	if got := healthTokenRef(cfg, unitSelection{member: "david"}).Env; got != "KENWARD_BOT_TOKEN_DAVID" {
		t.Errorf("david's pod would check %q, not their own token", got)
	}
	if got := healthTokenRef(cfg, unitSelection{group: true}).Env; got != "KENWARD_BOT_TOKEN_HOUSEHOLD" {
		t.Errorf("the group pod would check %q", got)
	}
	if got := healthTokenRef(cfg, unitSelection{}).Env; got != "KENWARD_BOT_TOKEN_HOUSEHOLD" {
		t.Errorf("the host process would check %q", got)
	}

	// And the probe actually uses it: david's pod authorises with david's token.
	h := newHarness(t, isolatedYAML, fullEnvironment())
	var seen string
	h.e.probes.telegram = func(_ context.Context, token string) telegramResult {
		seen = token
		return telegramResult{Username: "david_kenward_bot"}
	}
	if err := nodeHealthProbes(h.e, cfg, unitSelection{member: "david"}).Telegram(h.e.context()); err != nil {
		t.Fatal(err)
	}
	if seen != fakeDavidToken {
		t.Error("david's pod authorised with a token that is not david's")
	}
}

// TestSchedulerFailureNeverPreventsStarting.
//
// This build has no release keys compiled in, so the scheduler cannot be built. A
// household whose update configuration is wrong must still get a working assistant:
// the failure is a warning and nothing more.
func TestSchedulerFailureNeverPreventsStarting(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(releaseTrustedKeys) != "" {
		t.Skip("this build has release keys compiled in")
	}
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
		return stubSupervisor{}, nil
	}

	if code := h.run("run"); code != exitOK {
		t.Fatalf("exit = %d, want 0: a scheduler that cannot be built must not stop the assistant\n%s", code, h.both())
	}
	log := h.stderr()
	if !strings.Contains(log, "auto-update is off for this run") {
		t.Errorf("the warning was not logged:\n%s", log)
	}
	if !strings.Contains(log, "the assistant is unaffected") {
		t.Errorf("the warning does not say the assistant still works:\n%s", log)
	}
	h.assertNoSecrets(t)
}

// TestNilSchedulerIsSafe: internal/updater makes Run and Resume no-ops on a nil
// receiver precisely so a caller can log the construction error and go on serving
// without guarding every call site. This asserts cmd/kenward relies on that rather
// than nil-checking inconsistently.
func TestNilSchedulerIsSafe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	logger := slog.New(slog.NewTextHandler(h.e.stderr, nil))

	if code, done := resumeUpdate(h.e, nil, logger); done || code != exitOK {
		t.Errorf("resumeUpdate on a nil scheduler returned (%d, %v)", code, done)
	}
	if code := serve(h.e, stubSupervisor{}, nil, newRestartSignal(), logger); code != exitOK {
		t.Errorf("serve with a nil scheduler returned %d", code)
	}
}

// TestConsentNeverApprovesWithoutAYes.
//
// A major version bump, or a release flagged as changing security-relevant defaults,
// needs a human to agree. When there is nobody at the terminal nothing is applied: a
// release that may move routing or privacy defaults must not slip through because
// nothing was listening.
//
// The unanswered/declined split is not cosmetic. keel re-asks an unanswered request
// on the next cycle and permanently remembers a decline, so reading silence as "no"
// would bury a release — possibly a security fix — until somebody noticed.
func TestConsentNeverApprovesWithoutAYes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stdin string
		want  keelupdate.Decision
	}{
		{"empty input", "", keelupdate.DecisionUnanswered},
		{"anything else", "maybe\n", keelupdate.DecisionUnanswered},

		{"no", "n\n", keelupdate.DecisionDeclined},
		{"no spelled out", "NO\n", keelupdate.DecisionDeclined},

		{"yes", "y\n", keelupdate.DecisionApproved},
		{"yes spelled out", "YES\n", keelupdate.DecisionApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, simpleYAML, fullEnvironment())
			h.e.stdin = strings.NewReader(tc.stdin)
			got, err := consentPrompt(h.e)(h.e.context(), keelupdate.ConsentRequest{
				From:  keelVersion(1, 0, 0),
				To:    keelVersion(2, 0, 0),
				Notes: "notes",
			})
			if err != nil {
				t.Fatalf("consent returned an error: %v", err)
			}
			if got != tc.want {
				t.Errorf("consent = %v, want %v", got, tc.want)
			}
			if !strings.Contains(h.stdout(), "needs your agreement") {
				t.Errorf("the prompt does not explain itself:\n%s", h.stdout())
			}
		})
	}
}

// TestConsentNamesWhichReasonItIsAsking.
//
// "There is a major version" and "this release changes security-relevant behaviour"
// call for different amounts of thought from the person answering. keel carries the
// flag on the request precisely so a consumer can say which one it is, and collapsing
// them into one sentence would hide the one that matters — a release that could move
// routing or tier defaults, the settings that decide whether a private conversation
// may reach a provider.
func TestConsentNamesWhichReasonItIsAsking(t *testing.T) {
	t.Parallel()

	t.Run("security sensitive", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		h.e.stdin = strings.NewReader("n\n")
		if _, err := consentPrompt(h.e)(h.e.context(), keelupdate.ConsentRequest{
			From: keelVersion(1, 2, 0), To: keelVersion(1, 2, 1), SecuritySensitive: true,
		}); err != nil {
			t.Fatal(err)
		}
		out := h.stdout()
		for _, want := range []string{"security-relevant behaviour", "routing policy", "reach a provider"} {
			if !strings.Contains(out, want) {
				t.Errorf("the prompt does not say %q:\n%s", want, out)
			}
		}
	})

	t.Run("major version", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		h.e.stdin = strings.NewReader("n\n")
		if _, err := consentPrompt(h.e)(h.e.context(), keelupdate.ConsentRequest{
			From: keelVersion(1, 0, 0), To: keelVersion(2, 0, 0),
		}); err != nil {
			t.Fatal(err)
		}
		out := h.stdout()
		if !strings.Contains(out, "major version") {
			t.Errorf("the prompt does not say it is a major version:\n%s", out)
		}
		if strings.Contains(out, "flagged as changing security-relevant") {
			t.Errorf("a plain major bump was described as security-sensitive:\n%s", out)
		}
	})
}
