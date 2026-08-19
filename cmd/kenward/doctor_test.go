package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
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
		args   []string
	}{
		{"simple", simpleYAML, "doctor_simple.txt", nil},
		{"isolated", isolatedYAML, "doctor_isolated.txt", nil},
		// The pod's own report, which is what the container HEALTHCHECK runs and the
		// only place the shared-memory section appears: a household-wide process has
		// no lore store of its own to ask about.
		{"isolated-pod", isolatedYAML, "doctor_isolated_pod.txt", []string{"--member", "david"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			if code := h.run(append([]string{"doctor"}, tc.args...)...); code != exitOK {
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

// TestDoctorReportsWhichConversationsTheHouseholdHas covers household.agents, whose
// only visible consequence is which chats exist.
//
// The third conversation is the reason this is on the report at all. "kenward is
// reachable in a private chat" is a fact about where a member's words go, and an
// operator who cannot see it here has no way to find it out short of messaging the
// bot — so it is asserted rather than left to the goldens, which cover one agent.
func TestDoctorReportsWhichConversationsTheHouseholdHas(t *testing.T) {
	t.Parallel()
	perMember := strings.Replace(isolatedYAML,
		"  tiers: [local]\ntelegram:", "  tiers: [local]\n  agents: per_member\ntelegram:", 1)
	if perMember == isolatedYAML {
		t.Fatal("the fixture no longer has the shape this test edits")
	}

	for _, tc := range []struct {
		name string
		yaml string
		want []string
		deny []string
	}{
		{
			name: "one agent",
			yaml: isolatedYAML,
			want: []string{"one assistant for this household"},
			deny: []string{"reachable in a private chat"},
		},
		{
			name: "one agent each",
			yaml: perMember,
			want: []string{
				"one agent each",
				"kenward is also reachable in a private chat, on the household bot",
				"never a member's private memory",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			if code := h.run("doctor"); code != exitOK {
				t.Fatalf("exit = %d, want 0\n%s", code, h.stderr())
			}
			out := strings.Join(strings.Fields(h.stdout()), " ")
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("doctor output does not mention %q:\n%s", want, h.stdout())
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(out, deny) {
					t.Errorf("doctor output mentions %q for a household that has one assistant:\n%s", deny, h.stdout())
				}
			}
		})
	}
}

// TestDoctorListsWhatIsScheduled. The Reminders section is the only part of this
// report about what the node will do when nobody is talking to it, so an operator has
// to be able to see what is set without going near a member's chat — and must not see
// the reminder text itself, which is that member's private business and is read over
// somebody's shoulder here.
func TestDoctorListsWhatIsScheduled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())

	// Write a member's store where the running node would have written it.
	cfg := &config.Config{DataDir: filepath.Join(h.dir, "data")}
	store, err := remind.Open(cfg.RemindersPath("david", false), remind.Options{Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	r, err := remind.New("ring the plumber about the leak", remind.EveryDaily, 7, 30, time.Sunday, "", 99, now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(r); err != nil {
		t.Fatal(err)
	}

	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want 0 — a scheduled reminder is never a health failure\n%s", code, h.stderr())
	}
	out := h.stdout()
	if !strings.Contains(out, "David has 1 reminder scheduled") {
		t.Errorf("doctor does not report what is scheduled:\n%s", out)
	}
	if !strings.Contains(out, "every day at 07:30") {
		t.Errorf("doctor does not say when it fires:\n%s", out)
	}
	if strings.Contains(out, "plumber") {
		t.Errorf("doctor printed the reminder's text; it is the member's private business:\n%s", out)
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
	if !strings.Contains(h.stdout(), "lore did not answer") {
		t.Errorf("output does not say lore did not respond:\n%s", h.stdout())
	}
	// Everything else still ran: doctor reports all results rather than stopping.
	if !strings.Contains(h.stdout(), "Endpoints") || !strings.Contains(h.stdout(), "Privacy") {
		t.Errorf("doctor stopped at the first failure; it must run every check:\n%s", h.stdout())
	}
}

// TestDoctorReportsBrokenSharedMemory is the defect this section exists for.
//
// A pod can pass every other check with a shared space that reaches nobody: its own
// lore store holds the space, lore answers about it, and nothing carries an
// entry to or from the household's other pods because no sync daemon is running. That
// used to render as a cheerful tick and a standing hint to run `lore serve`. It must
// now say plainly that shared memory is not moving — and must not fail the report,
// because the pod is serving and private memory, which is what the mode is for, is
// unaffected.
func TestDoctorReportsBrokenSharedMemory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		probes func(probes) probes
		want   []string
	}{
		{"no daemon", syncDaemonDown, []string{
			"shared memory is not syncing",
			"private memory is unaffected",
		}},
		{"daemon alone", syncFoundNobody, []string{
			"shared memory is syncing with nobody",
			"separate copy",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, isolatedYAML, fullEnvironment())
			h.e.probes = tc.probes(healthyProbes())

			if code := h.run("doctor", "--member", "david"); code != exitOK {
				t.Fatalf("exit = %d, want 0: a pod that is serving is not unhealthy for a degraded feature\n%s", code, h.both())
			}
			for _, want := range tc.want {
				if !strings.Contains(h.stdout(), want) {
					t.Errorf("output does not say %q:\n%s", want, h.stdout())
				}
			}
		})
	}
}

// TestDoctorPodSpaceMissingNamesTheRealCause, for each of the two kinds of space a pod
// holds.
//
// The observed symptom of the original defect was `✗ space "…" is not a space this
// lore store holds`, which sent the operator to check the id column of a value that
// was perfectly correct. In a pod the cause is that this store never received the space,
// and the report has to say how it can.
//
// The two remedies are different, and only one of them is an operator's. The shared
// space is one space held by several stores, so it is invited and joined by a person
// who decided to share. A member's PRIVATE space crosses nothing and is created by that
// member's own pod at the configured id — so the remedy for it is that there is no
// remedy: `run` makes it, and offering a command here would send somebody to create a
// second space beside the one that is coming.
func TestDoctorPodSpaceMissingNamesTheRealCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		space string
		want  []string
	}{
		{"the household's shared space", "dac31e70-72e4-4b10-9cef-a6276c4a87b8",
			[]string{"lore space invite", "lore join"}},
		{"david's private space", "7d5047bb-d939-4539-b3db-8b6221a2e245",
			[]string{"this unit creates this space itself", "kenward run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, isolatedYAML, fullEnvironment())
			base := healthyProbes()
			inner := base.lore
			base.lore = func(ctx context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
				res := inner(ctx, cfg, scope)
				for i, s := range res.Spaces {
					if string(s.Space) == tc.space {
						res.Spaces[i].Err = unknownSpaceErr(s.Space)
					}
				}
				return res
			}
			h.e.probes = base

			// Exit 0, and that is load-bearing rather than lenient. One space of two
			// is a degraded pod and not a broken one, and this command is the image's
			// HEALTHCHECK: restarting a pod that is waiting for an invitation cannot
			// bring the invitation any closer.
			if code := h.run("doctor", "--member", "david"); code != exitOK {
				t.Fatalf("exit = %d, want 0: a pod missing one space of two must stay reachable\n%s", code, h.both())
			}
			flat := strings.Join(strings.Fields(h.stdout()), " ")
			for _, want := range tc.want {
				if !strings.Contains(flat, want) {
					t.Errorf("output does not name the remedy %q:\n%s", want, h.stdout())
				}
			}
		})
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
// id belongs is refused at every call.
//
// It reported such a space as a fact ("does not exist yet — it is created when the
// member claims their invite") and exited 0. Nothing in kenward creates a lore space,
// so there was nothing to wait for: that was a green light for a household that could
// not read its own memory. It is a fault in kenward.yaml, only an edit fixes it, and
// the exit code says so.
//
// The remedy has to be true about writes as well, which is the second thing fixed here.
// It used to say a name "fails only on reads — writes keep appearing to work", which
// described the superseded subprocess client and told an operator that whatever had
// already been captured was safe. Measured against a real lore store:
//
//	write to a REAL space id           -> ok
//	write to an UNKNOWN well-formed id -> lore: space "…": space not found  (exit 1)
//	write to a DISPLAY NAME (CLI)      -> ok
//
// The third line is the CLI only: cmd/lore's resolveSpace falls back to SpaceByName.
// memory.Client.Put hands the configured string to lore.Store.PutEntry, which does
// GetSpace on it and nothing else — so through the path kenward actually uses, a name
// and a mistyped id fail identically and nothing was ever stored under either.
func TestDoctorUnknownSpaceIsAConfigurationFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.e.probes.lore = func(_ context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
		var res loreResult
		for _, s := range configuredSpaces(cfg, scope) {
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
	// Exit 0, and the change from 2 is the second half of a bug rather than a
	// relaxation. `run` serves a household with one mistyped space id, and this
	// command is the image's HEALTHCHECK: exiting non-zero on a condition the node
	// starts through marks the container unhealthy forever over something no restart
	// can fix. The two now share judgeMemory, so unhealthy means "this node would
	// refuse to start on this" and nothing else. The finding is still on the report,
	// in full, which is what the assertions below are for.
	if code := h.run("doctor"); code != exitOK {
		t.Fatalf("exit = %d, want %d: one mistyped id is a finding, not a restart\n%s", code, exitOK, h.both())
	}
	out := h.stdout()
	// Compared with the line breaks collapsed: doctor wraps its detail lines, and a
	// space id is long enough to move where the wrap falls.
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"is not a space this lore store holds",
		"spaces are named by id here, not by display name",
		"run `lore spaces` and configure the id column",
		"nothing has been stored under this value: reads and writes both fail",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not exist yet") {
		t.Errorf("output still tells the operator to wait for a space kenward never creates:\n%s", out)
	}
	// The old remedy, which is false against lore's Go API and reassuring in the
	// wrong direction: it tells an operator the household's captured memories
	// survived a fault under which nothing was written at all.
	for _, gone := range []string{
		"fails only on reads",
		"writes keep appearing to work",
	} {
		if strings.Contains(flat, gone) {
			t.Errorf("the remedy still says %q; memory.Client.Put passes the configured "+
				"string to lore.Store.PutEntry, which resolves it by id and refuses a "+
				"name, so the write fails too:\n%s", gone, out)
		}
	}
}

// TestStartupAndHealthAgreeAboutMissingSpaces is the reconciliation itself, asserted as
// one property rather than as two behaviours that happen to line up today.
//
// `kenward doctor` is the image's HEALTHCHECK. An exit code from it is not a severity
// rating, it is an instruction to a container runtime to restart the node — so it is
// only meaningful if unhealthy means "this node would refuse to start on this". It did
// not: `doctor` exited 2 on any missing space while `run` served through the same
// condition, which is a node that works and a container marked unhealthy forever
// (deploy/compose.simple.yml's documented flow produced exactly that). Whichever way a
// future edit moves either side, this fails unless it moves both.
func TestStartupAndHealthAgreeAboutMissingSpaces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		yaml    string
		unit    []string
		missing func(cfg *config.Config, scope config.UnitScope) map[string]bool
		// wantServes is whether a node in this state should be running at all. The
		// health check's answer must be the same one.
		wantServes bool
	}{
		{
			name: "simple mode, every space missing: this is not the household's store",
			yaml: simpleYAML,
			missing: func(cfg *config.Config, scope config.UnitScope) map[string]bool {
				return allSpaces(cfg, scope)
			},
			wantServes: false,
		},
		{
			name: "simple mode, one space missing: one member's typo",
			yaml: simpleYAML,
			missing: func(*config.Config, config.UnitScope) map[string]bool {
				return map[string]bool{"5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19": true}
			},
			wantServes: true,
		},
		{
			// This one used to serve, and stopping it serving is the point of the
			// pod creating its own spaces. A pod makes its private space at the
			// configured id on the way up, so a pod that holds NONE of its spaces
			// is not a pod that has yet to be provisioned — it is a pod whose
			// creation did not happen, or a store that is not this member's.
			name: "an isolated pod holding none of its spaces, which it can no longer be waiting for",
			yaml: isolatedYAML,
			unit: []string{"--member", "david"},
			missing: func(cfg *config.Config, scope config.UnitScope) map[string]bool {
				return allSpaces(cfg, scope)
			},
			wantServes: false,
		},
		{
			// And this is the state the exception above existed for, still intact:
			// the invite handshake happens inside a running pod, and this pod runs.
			name: "an isolated pod that has its private space but not the household's",
			yaml: isolatedYAML,
			unit: []string{"--member", "david"},
			missing: func(cfg *config.Config, _ config.UnitScope) map[string]bool {
				return map[string]bool{cfg.Household.SharedSpace: true}
			},
			wantServes: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := func(_ context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
				gone := tc.missing(cfg, scope)
				var res loreResult
				for _, s := range configuredSpaces(cfg, scope) {
					r := spaceResult{Space: s}
					if gone[string(s)] {
						r.Err = unknownSpaceErr(s)
					}
					res.Spaces = append(res.Spaces, r)
				}
				return res
			}

			vars := fullEnvironment()
			vars["LORE_HOME"] = filepath.Join(t.TempDir(), "lore")

			hRun := newHarness(t, tc.yaml, vars)
			hRun.e.probes = healthyProbes()
			hRun.e.probes.lore = probe
			served := false
			hRun.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				served = true
				return stubSupervisor{}, nil
			}
			runCode := hRun.run(append([]string{"run"}, tc.unit...)...)

			hDoc := newHarness(t, tc.yaml, vars)
			hDoc.e.probes = healthyProbes()
			hDoc.e.probes.lore = probe
			docCode := hDoc.run(append([]string{"doctor"}, tc.unit...)...)

			if served != tc.wantServes {
				t.Errorf("run served = %v, want %v\n%s", served, tc.wantServes, hRun.both())
			}
			if healthy := docCode == exitOK; healthy != tc.wantServes {
				t.Errorf("doctor exit = %d (healthy = %v), want healthy = %v\n%s",
					docCode, healthy, tc.wantServes, hDoc.both())
			}
			if (runCode == exitOK) != (docCode == exitOK) {
				t.Errorf("run exited %d and doctor exited %d for one store; a health check "+
					"that disagrees with startup is a restart loop or a silent amnesia",
					runCode, docCode)
			}
		})
	}
}

// allSpaces is every space the unit is configured for, as a lookup for the probe above.
func allSpaces(cfg *config.Config, scope config.UnitScope) map[string]bool {
	out := map[string]bool{}
	for _, s := range configuredSpaces(cfg, scope) {
		out[string(s)] = true
	}
	return out
}

// TestDoctorReportsTheConversationResetSchedule.
//
// The symptom of this setting — "it forgot what we were talking about" — is
// indistinguishable from a broken assistant, so the effective value has to be readable
// somewhere. This is that somewhere, and it is on the same screen as everything else
// somebody checks when a household says kenward is behaving oddly.
//
// Both cases are ok lines rather than warnings. Unlike an idle key timeout there is
// nothing here to recover from: the member is told, and their next message works.
func TestDoctorReportsTheConversationResetSchedule(t *testing.T) {
	t.Parallel()

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, simpleYAML, fullEnvironment())
		if code := h.run("doctor"); code != exitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, h.both())
		}
		if !strings.Contains(h.stdout(), "history.reset_every is off") {
			t.Errorf("doctor does not say the conversation reset is off:\n%s", h.stdout())
		}
	})

	t.Run("on", func(t *testing.T) {
		t.Parallel()
		on := simpleYAML + "history:\n  reset_every: 6h\n"
		h := newHarness(t, on, fullEnvironment())
		if code := h.run("doctor"); code != exitOK {
			t.Fatalf("exit = %d, want 0: a scheduled reset is normal operation, not a fault\n%s", code, h.both())
		}
		out := h.stdout()
		for _, want := range []string{
			"drop their recent turns every 6h0m0s",
			"anchored to local midnight",
			// The distinction is the reason the line is worth printing at all.
			"nothing in lore is touched",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("doctor does not report the conversation reset (%q missing):\n%s", want, out)
			}
		}
	})
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
