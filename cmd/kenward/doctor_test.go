package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden files in testdata")

// TestDoctorGolden pins doctor's whole output for both modes.
//
// docs/CLI.md calls this the single most important output the product produces,
// because it is where a claim becomes checkable. A golden file is the only way to
// notice the day somebody softens a line by accident.
func TestDoctorGolden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		yaml   string
		golden string
	}{
		{"simple", simpleYAML, "doctor_simple.txt"},
		{"isolated", isolatedYAML, "doctor_isolated.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			if code := h.run("doctor"); code != exitOK {
				t.Fatalf("exit = %d, want 0\n%s", code, h.stderr())
			}
			h.assertNoSecrets(t)
			// The configuration path is a temp directory; the golden file cannot
			// carry it.
			got := strings.ReplaceAll(normalize(h.stdout()), h.config, "<CONFIG>")
			compareGolden(t, tc.golden, got)
		})
	}
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (regenerate with -update-golden): %v", err)
	}
	if got != normalize(string(want)) {
		t.Errorf("doctor output differs from %s.\n--- got ---\n%s\n--- want ---\n%s", path, got, normalize(string(want)))
	}
}

// TestDoctorEverythingAsleepExitsZero is the check the container's HEALTHCHECK
// depends on.
//
// A household's inference machines are legitimately switched off. Treating that as
// unhealthy would restart a working household in a loop, so an unreachable endpoint
// is reported as a fact and changes nothing about the exit code.
func TestDoctorEverythingAsleepExitsZero(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes = everythingAsleep(healthyProbes())

	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0: a powered-off endpoint is a fact, not a failure\n%s", code, h.both())
	}
	out := h.stdout()
	for _, want := range []string{"monster", "openrouter", "no answer", "not failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	h.assertNoSecrets(t)
}

// TestDoctorLoreUnreachableFails: an unreachable lore is one of the three things
// docs/CLI.md says doctor exits non-zero for.
func TestDoctorLoreUnreachableFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes = loreDown(healthyProbes())

	if code := h.run("doctor"); code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(h.stdout(), "lore mcp did not respond") {
		t.Errorf("output does not say lore did not respond:\n%s", h.stdout())
	}
	// Everything else still ran: doctor reports all results rather than stopping.
	if !strings.Contains(h.stdout(), "Endpoints") || !strings.Contains(h.stdout(), "Privacy") {
		t.Errorf("doctor stopped at the first failure; it must run every check:\n%s", h.stdout())
	}
}

// TestDoctorTelegramRefusalFails: a Telegram authorisation failure is the third.
//
// The second case is the one a health check meets. A refused token and sleeping
// endpoints in the same run must still exit 1: the sleeping machines stay facts, and a
// node that can neither receive nor send is not healthy just because the reason it is
// broken shares a screen with something that is fine. In isolated mode too, where the
// refused token belongs to one member's own bot.
func TestDoctorTelegramRefusalFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		yaml   string
		probes probes
	}{
		{"simple", simpleYAML, telegramRefuses(healthyProbes())},
		{"simple, endpoints asleep too", simpleYAML, telegramRefuses(everythingAsleep(healthyProbes()))},
		{"isolated", isolatedYAML, telegramRefuses(healthyProbes())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			h.e.probes = tc.probes

			if code := h.run("doctor"); code != exitFailure {
				t.Fatalf("exit = %d, want %d: a token Telegram refuses is a node that cannot work\n%s", code, exitFailure, h.both())
			}
			if !strings.Contains(h.stdout(), "Telegram did not authorise") {
				t.Errorf("output does not report the refusal:\n%s", h.stdout())
			}
			h.assertNoSecrets(t)
		})
	}
}

// TestDoctorConfigFaultExitsTwo: a configuration fault is a usage error, and it
// dominates whatever else the probes found.
func TestDoctorConfigFaultExitsTwo(t *testing.T) {
	t.Parallel()
	// "gpu" is not a tag on any endpoint.
	broken := strings.Replace(simpleYAML, "    tiers: [local]\n", "    tiers: [gpu]\n", 1)
	h := newHarness(t, broken, fullEnvironment())

	if code := h.run("doctor"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(h.stdout(), "cannot be served") {
		t.Errorf("output does not report the validation problem:\n%s", h.stdout())
	}
}

// TestDoctorPrivacySectionComesFromTheirPackage.
//
// The privacy statement exists once, in internal/privacy, and internal/setup tells
// every operator that `kenward doctor` prints "this same statement, in the same
// words". This asserts the exact string, unmodified — no indentation, no rewrap, no
// summary — because a paraphrase is how one copy of a promise drifts into promising
// more than the mode delivers.
func TestDoctorPrivacySectionComesFromTheirPackage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		mode privacy.Mode
	}{
		{"simple", simpleYAML, privacy.ModeSimple},
		{"isolated", isolatedYAML, privacy.ModeIsolated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			h.run("doctor")
			out := normalize(h.stdout())

			if !strings.Contains(out, normalize(privacy.Statement(tc.mode))) {
				t.Errorf("doctor did not print privacy.Statement(%v) verbatim:\n%s", tc.mode, out)
			}
			other := privacy.ModeIsolated
			if tc.mode == privacy.ModeIsolated {
				other = privacy.ModeSimple
			}
			if strings.Contains(out, normalize(privacy.Statement(other))) {
				t.Errorf("doctor printed the other mode's statement")
			}
		})
	}
}

// TestDoctorTierNotesComeFromPrivacy checks the per-conversation lines are the
// privacy package's, and that "local" is decided from where the endpoints actually
// are rather than from what a tier is called.
func TestDoctorTierNotesComeFromPrivacy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.run("doctor")
	out := normalize(h.stdout())

	cfg, err := loadConfig(h.config, filepath.Join(h.dir, "data"), testSecrets(fullEnvironment()))
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	david := mustMember(t, cfg, "david")
	jordan := mustMember(t, cfg, "jordan")

	// david's chain is [local], and monster.tail is a machine in the house.
	if want := privacy.MemberNote(david, true); !strings.Contains(out, want) {
		t.Errorf("missing %q\n%s", want, out)
	}
	// jordan's chain reaches openrouter.ai, which is not.
	if want := privacy.MemberNote(jordan, false); !strings.Contains(out, want) {
		t.Errorf("missing %q\n%s", want, out)
	}
	if want := privacy.TierNote("Casa", cfg.Household.Tiers, false); !strings.Contains(out, want) {
		t.Errorf("missing %q\n%s", want, out)
	}
}

// TestDoctorJSON checks --json carries the same verdict as the text form.
func TestDoctorJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes = everythingAsleep(healthyProbes())
	if code := h.run("doctor", "--json"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.stderr())
	}
	var rep doctorReport
	if err := json.Unmarshal(h.out.Bytes(), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, h.stdout())
	}
	if rep.Exit != exitOK {
		t.Errorf("exit_code = %d, want 0", rep.Exit)
	}
	if rep.Mode != "simple" {
		t.Errorf("mode = %q", rep.Mode)
	}
	if len(rep.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(rep.Endpoints))
	}
	for _, ep := range rep.Endpoints {
		if ep.Reached {
			t.Errorf("endpoint %s reported as reached", ep.Name)
		}
	}
	if rep.Statement != privacy.Statement(privacy.ModeSimple) {
		t.Errorf("privacy_statement is not internal/privacy's")
	}
	h.assertNoSecrets(t)
}

// TestDoctorUnknownSpaceIsAConfigurationFault.
//
// This check exists because doctor was vouching for something the runtime cannot do.
// internal/memory keys spaces on lore space ids, so a display name configured where an
// id belongs fails every read — and lore's own arguments are lenient enough that writes
// keep working, so nothing surfaces until the first retrieval comes back empty.
//
// It reported such a space as a fact ("does not exist yet — it is created when the
// member claims their invite") and exited 0. Nothing in kenward creates a lore space,
// so there was nothing to wait for: that was a green light for a household that could
// not read its own memory. It is a fault in kenward.yaml, only an edit fixes it, and
// the exit code says so.
func TestDoctorUnknownSpaceIsAConfigurationFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes.lore = func(_ context.Context, cfg *config.Config) loreResult {
		var res loreResult
		for _, s := range configuredSpaces(cfg) {
			r := spaceResult{Space: s}
			if s == "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19" {
				// The error internal/memory returns for exactly this, wrapped the
				// way the client wraps it — doctor surfaces its text rather than
				// writing a second explanation.
				r.Err = fmt.Errorf("memory: lore holds no space %s (spaces are named by id here, not by display name): %w",
					s, memory.ErrUnknownSpace)
			}
			res.Spaces = append(res.Spaces, r)
		}
		return res
	}
	if code := h.run("doctor"); code != exitUsage {
		t.Fatalf("exit = %d, want %d: a space lore does not hold is a fault only an edit fixes\n%s", code, exitUsage, h.both())
	}
	out := h.stdout()
	// Compared with the line breaks collapsed: doctor wraps its detail lines, and a
	// space id is long enough to move where the wrap falls.
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"is not a space this lore store holds",
		"spaces are named by id here, not by display name",
		"run `lore spaces` and configure the id column",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not exist yet") {
		t.Errorf("output still tells the operator to wait for a space kenward never creates:\n%s", out)
	}
}

// TestDoctorSaysWhichWayIdleExpiryIsSet.
//
// The privacy statement names session.idle_timeout and is deliberately true whichever
// way it is set, because internal/privacy cannot see a household's configuration.
// doctor can, and this is the screen somebody reads to find out which case is theirs.
// A household that has switched expiry on has armed something with no in-band way back
// (D-019): the assistant stops answering after that much quiet and needs somebody at
// the machine. That is worth saying before it happens, and it is a warning rather than
// a failure, because it is the household's own choice.
func TestDoctorSaysWhichWayIdleExpiryIsSet(t *testing.T) {
	t.Parallel()

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		if code := h.run("doctor"); code != exitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, h.both())
		}
		if !strings.Contains(h.stdout(), "do not expire on idle") {
			t.Errorf("doctor does not say idle expiry is off:\n%s", h.stdout())
		}
	})

	t.Run("on", func(t *testing.T) {
		t.Parallel()
		on := simpleYAML + "session:\n  idle_timeout: 30m\n"
		h := newHarness(t, on, fullEnvironment())
		if code := h.run("doctor"); code != exitOK {
			t.Fatalf("exit = %d, want 0: a configured timeout is a warning, not a failure\n%s", code, h.both())
		}
		out := h.stdout()
		for _, want := range []string{
			"expire after 30m0s of quiet",
			"stops answering",
			"no way back from a chat",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("doctor does not warn about idle expiry (%q missing):\n%s", want, out)
			}
		}
	})
}
