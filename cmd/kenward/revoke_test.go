package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// TestIsolatedRevokeReachesThePodThatHoldsTheBinding is the journey the operator
// actually makes, end to end, and it is `kenward invite`'s crossing in reverse.
//
// `kenward revoke` unbinds in this node's own state file. In isolated mode the process
// holding the binding is not this one: it is the member's pod, which redeemed the claim
// against its own volume, on a filesystem this machine cannot see and must not write.
// Nothing bridged them. The operator ran `kenward revoke jordan`, read that messages
// from that account were ignored from now on, and jordan's pod carried on serving them
// — which is worse than `invite`'s failure, because `invite` failed where somebody could
// see it and this succeeded while doing nothing.
//
// The bridge is a per-member record the deployment carries into the pod, and this walks
// all three legs: record on the host, carry the file, apply it in the pod.
func TestIsolatedRevokeReachesThePodThatHoldsTheBinding(t *testing.T) {
	t.Parallel()
	// jordan claimed inside their own pod, which is where an isolated claim happens:
	// the host's state file never learned of it and the configuration declares
	// nothing.
	yaml := strings.Replace(isolatedYAML, "    telegram_id: 87654321\n", "", 1)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("revoke", "jordan"); code != exitOK {
		t.Fatalf("revoke exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	// The one sentence it must not print. The pod is still serving them.
	if strings.Contains(out, "ignored from now on") {
		t.Errorf("revoke claims the account is already ignored; jordan's pod is still serving it:\n%s", out)
	}
	for _, want := range []string{"NOT unbound yet", "Restart kenward"} {
		if !strings.Contains(out, want) {
			t.Errorf("revoke does not say %q:\n%s", want, out)
		}
	}
	h.assertNoSecrets(t)

	// Leg two: the record the deployment carries. It is jordan's, it is where both
	// deployment paths look for it, and it is named for the configured id.
	rec := filepath.Join(h.dir, "data", revocationDirName, "jordan.json")
	if !strings.Contains(out, rec) {
		t.Errorf("revoke does not name the file it wrote:\n%s", out)
	}
	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("`kenward revoke` recorded nothing for jordan's pod, so the revocation cannot reach it: %v", err)
	}
	if strings.Contains(string(raw), "david") {
		t.Errorf("jordan's revocation record mentions david:\n%s", raw)
	}

	// Leg three: the pod. A different data directory on a different filesystem —
	// which is the whole point — holding the binding jordan's claim wrote there.
	claimedAt := h.e.now().Add(-time.Hour)
	podCfg := podWithClaim(t, yaml, "jordan", 87654321, claimedAt)
	if !mustMember(t, podCfg, "jordan").Enrolled() {
		t.Fatal("the fixture does not bind jordan, so this test is checking nothing")
	}
	if err := applyRevocation(h.e, podCfg, unitSelection{member: "jordan"}, rec); err != nil {
		t.Fatalf("the pod could not apply the revocation: %v", err)
	}
	if m := mustMember(t, podCfg, "jordan"); m.Enrolled() {
		t.Errorf("jordan's pod still serves telegram_id %d after applying the revocation", m.TelegramID)
	}
	// And durably, in the one file the pod owns: a pod that cleared this in memory
	// only would serve them again on the next restart.
	st, err := config.LoadState(podCfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if bd, ok := st.Binding("jordan"); ok {
		t.Errorf("%s still binds jordan to %d", podCfg.StatePath(), bd.TelegramID)
	}
}

// TestPodKeepsAClaimMadeAfterTheRevocation.
//
// The record is not a standing order. A member who is revoked, invited again and claims
// again holds a binding newer than it, and their pod is recreated on every rolling
// update — so a record that unbound on sight would make re-enrolment impossible for
// exactly the people who have been through this once.
func TestPodKeepsAClaimMadeAfterTheRevocation(t *testing.T) {
	t.Parallel()
	yaml := strings.Replace(isolatedYAML, "    telegram_id: 87654321\n", "", 1)
	h := newHarness(t, yaml, fullEnvironment())
	if code := h.run("revoke", "jordan"); code != exitOK {
		t.Fatalf("revoke exit = %d, want 0\n%s", code, h.both())
	}
	rec := filepath.Join(h.dir, "data", revocationDirName, "jordan.json")

	podCfg := podWithClaim(t, yaml, "jordan", 99999999, h.e.now().Add(time.Hour))
	if err := applyRevocation(h.e, podCfg, unitSelection{member: "jordan"}, rec); err != nil {
		t.Fatalf("applying the revocation: %v", err)
	}
	if !mustMember(t, podCfg, "jordan").Enrolled() {
		t.Error("a claim made after the revocation was undone by it; jordan can never re-enrol")
	}
}

// TestPodRefusesARevocationThatNamesAnotherMember.
//
// The record is provisioned into exactly one pod, and the compose deployment mounts it
// by hand. A path pointing at the wrong file would otherwise unbind whoever this pod
// serves, which is the same class of mistake as a pod started for a member it does not
// serve, and gets the same refusal.
func TestPodRefusesARevocationThatNamesAnotherMember(t *testing.T) {
	t.Parallel()
	yaml := strings.Replace(isolatedYAML, "    telegram_id: 87654321\n", "", 1)
	h := newHarness(t, yaml, fullEnvironment())
	if code := h.run("revoke", "david"); code == exitOK {
		t.Fatalf("revoke david should refuse: the configuration declares david's telegram_id\n%s", h.both())
	}
	// david's record, written directly, mounted into jordan's pod.
	dir := t.TempDir()
	rec, err := writeRevocation(dir, "david", h.e.now())
	if err != nil {
		t.Fatal(err)
	}
	podCfg := podWithClaim(t, yaml, "jordan", 87654321, h.e.now().Add(-time.Hour))
	err = applyRevocation(h.e, podCfg, unitSelection{member: "jordan"}, rec)
	if err == nil {
		t.Fatal("jordan's pod applied a revocation naming david")
	}
	if !strings.Contains(err.Error(), "david") || !strings.Contains(err.Error(), "jordan") {
		t.Errorf("the refusal does not name both members: %v", err)
	}
	if !mustMember(t, podCfg, "jordan").Enrolled() {
		t.Error("jordan was unbound by a record naming david")
	}
}

// TestRevokeRefusesWhileTheConfigurationDeclaresTheTelegramID.
//
// The same silent success by a different route, and in both modes. A telegram_id written
// into kenward.yaml by hand is not in the enrolment record, so clearing that record
// changes nothing an operator can see and nothing the next start reads: the file still
// names the account and it is served again. kenward does not rewrite the configuration —
// that file is the operator's, comments and all — so this refuses, before anything has
// been cleared, and names the one line to delete.
func TestRevokeRefusesWhileTheConfigurationDeclaresTheTelegramID(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct{ name, yaml string }{
		{"simple", simpleYAML},
		{"isolated", isolatedYAML},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, mode.yaml, fullEnvironment())
			if code := h.run("revoke", "david"); code != exitUsage {
				t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
			}
			out := h.stderr()
			for _, want := range []string{h.config, "telegram_id: 12345678", "members[david]"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not name %q:\n%s", want, out)
				}
			}
			if h.stdout() != "" {
				t.Errorf("a refused revocation still reported one on stdout:\n%s", h.stdout())
			}
			// Nothing cleared and nothing recorded: running it again after the edit
			// starts from an unchanged household.
			if _, err := os.Stat(filepath.Join(h.dir, "data", revocationDirName)); err == nil {
				t.Error("a refused revocation still wrote a record for the pod")
			}
		})
	}
}

// TestSimpleRevokeSaysARunningNodeMustBeRestarted.
//
// A running node built its member set when it started and does not re-read the state
// file, so a revocation performed beside it takes effect at the next start in this mode
// too. Saying so is the difference between a revocation that happens and one that is
// believed to have happened.
func TestSimpleRevokeSaysARunningNodeMustBeRestarted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, claimedYAML, fullEnvironment())
	h.claimedInState(t, "david", 12345678)
	if code := h.run("revoke", "david"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	if !strings.Contains(h.stdout(), "restart it") {
		t.Errorf("revoke does not say a running node must be restarted:\n%s", h.stdout())
	}
	if strings.Contains(h.stdout(), revocationDirName) {
		t.Errorf("simple mode wrote a pod revocation record; there is no pod to carry it to:\n%s", h.stdout())
	}
	st, err := config.LoadState(filepath.Join(h.dir, "data", config.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Binding("david"); ok {
		t.Error("simple mode's revocation did not clear the binding it can reach")
	}
}

// TestTheTwoDeploymentPathsAgreeOnWhereARevocationIsRead.
//
// The host writes the record, the supervisor provisions it and the pod reads the path
// its command line names. Three places, and the compose file writes the same three by
// hand. The pod-side paths are constants in internal/supervisor; this is what holds the
// command line to them.
func TestTheTwoDeploymentPathsAgreeOnWhereARevocationIsRead(t *testing.T) {
	t.Parallel()
	argv := strings.Join(supervisor.PodCommand("--member=jordan"), " ")
	if want := "--revoked=" + supervisor.PodRevokedPath; !strings.Contains(argv, want) {
		t.Errorf("a member's pod is not told where its revocation record is: %s", argv)
	}
	// The group's pod holds no member's binding and has nothing to clear.
	if group := strings.Join(supervisor.PodCommand(supervisor.PodGroupFlag), " "); strings.Contains(group, "--revoked") {
		t.Errorf("the household group's pod is given a revocation record: %s", group)
	}
	// And the host writes one file per member, named for the configured id, which is
	// what the supervisor looks up and what the compose file mounts.
	if got, want := revocationPath("/data/revocations", "jordy"), filepath.Join("/data/revocations", "jordy.json"); got != want {
		t.Errorf("revocationPath = %q, want %q", got, want)
	}
}

// podWithClaim is a pod's world: its own configuration file, its own data directory,
// and a binding in the state file on it — which is where an isolated claim puts one,
// and the file the host can neither see nor write.
func podWithClaim(t *testing.T, yaml string, id domain.MemberID, telegramID int64, at time.Time) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	st := config.NewState()
	st.Bind(id, telegramID, at)
	if err := st.Save(filepath.Join(data, config.StateFileName)); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, data, testSecrets(fullEnvironment()))
	if err != nil {
		t.Fatalf("loading the pod's configuration: %v", err)
	}
	return cfg
}
