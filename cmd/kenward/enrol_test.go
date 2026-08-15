package main

import (
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/enrol"
)

func mustDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestInvitePrintsTheCodeAndNothingElse.
//
// docs/CLI.md: the code, and the three facts the person handing it over needs. No
// QR, no link, no deep link that would leak the code into a chat log.
func TestInvitePrintsTheCodeAndNothingElse(t *testing.T) {
	t.Parallel()
	// david is declared but has claimed already; sam is declared and has not.
	yaml := strings.Replace(simpleYAML,
		"  - id: jordan\n    name: Jordan\n    telegram_id: 87654321\n",
		"  - id: jordan\n    name: Jordan\n", 1)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()

	for _, want := range []string{
		"Claim code for Jordan:",
		"It works once",
		"expires in 24 hours",
		"the bot will not reply to them at all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("invite does not say %q:\n%s", want, out)
		}
	}
	// Nothing that would carry the code somewhere it can be read later.
	for _, unwanted := range []string{"http://", "https://", "t.me/", "QR", "qr"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("invite printed %q, which would leak the code into a chat log:\n%s", unwanted, out)
		}
	}
	h.assertNoSecrets(t)
}

// TestInviteRefusesAnUndeclaredMember.
//
// The Binder can create a member the configuration does not declare, but the state
// file cannot persist one — it holds bindings and nothing else — so a member conjured
// by a claim would vanish on the next restart along with the binding pointing at
// them. The operator would blame enrolment rather than persistence, so this refuses
// where the fix is one edit away, and says which four fields to add.
func TestInviteRefusesAnUndeclaredMember(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())

	if code := h.run("invite", "--name", "Sam"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	out := h.stderr()
	if !strings.Contains(out, h.config) {
		t.Errorf("the error does not name the file to edit:\n%s", out)
	}
	for _, field := range []string{"id:", "name:", "private_space:", "tiers:"} {
		if !strings.Contains(out, field) {
			t.Errorf("the error does not name the %q field:\n%s", field, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "provisioning") {
		t.Errorf("the error talks about provisioning rather than what to do:\n%s", out)
	}
	if h.stdout() != "" {
		t.Errorf("a refused invite still wrote to stdout:\n%s", h.stdout())
	}
}

// TestInviteFindsAMemberByTheNameTheWizardWasGiven.
//
// setup derives a member's id by slugifying their name, accents folded. Somebody who
// told the wizard "José" must be able to type "José" here.
func TestInviteFindsADeclaredMemberSeveralWays(t *testing.T) {
	t.Parallel()
	yaml := strings.Replace(simpleYAML,
		"  - id: jordan\n    name: Jordan\n    telegram_id: 87654321\n",
		"  - id: jose\n    name: José\n", 1)
	yaml = strings.Replace(yaml, "jordan-private", "jose-private", 1)

	for _, name := range []string{"jose", "José", "josé"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, yaml, fullEnvironment())
			if code := h.run("invite", "--name", name); code != exitOK {
				t.Fatalf("invite --name %q exited %d\n%s", name, code, h.both())
			}
			if !strings.Contains(h.stdout(), "Claim code for José") {
				t.Errorf("the code was not minted for the declared member:\n%s", h.stdout())
			}
		})
	}
}

// TestInviteRefusesAnAlreadyEnrolledMember: a second code for somebody who has
// already claimed is a code that will not bind.
func TestInviteRefusesAnAlreadyEnrolledMember(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := h.run("invite", "--name", "David"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	if !strings.Contains(h.stderr(), "already claimed") {
		t.Errorf("the error does not say why:\n%s", h.stderr())
	}
}

// TestRevokeStatesTheKeyRotationCaveat.
//
// Unbinding stops that Telegram account being served and does nothing to a key the
// member already holds. kenward has no authority to re-key a lore space, and a
// revocation that reads as complete when it is not is a false security claim.
func TestRevokeStatesTheKeyRotationCaveat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())

	if code := h.run("revoke", "david"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "david-private") {
		t.Errorf("the revocation does not name the space whose key must be rotated:\n%s", out)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "rotate") || !strings.Contains(lower, "key") {
		t.Errorf("the revocation does not state the key-rotation caveat:\n%s", out)
	}
	// The canonical wording lives in internal/enrol, not here.
	if !strings.Contains(out, enrol.Revocation{
		Member: mustMember(t, mustLoad(t, simpleYAML), "david"),
		Space:  "david-private",
	}.Warning()) {
		t.Errorf("the revocation does not print enrol.Revocation.Warning verbatim:\n%s", out)
	}
	h.assertNoSecrets(t)
}

// TestRevokeUnknownMemberIsAUsageError.
func TestRevokeUnknownMemberIsAUsageError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := h.run("revoke", "nobody"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
}

// TestFlagsAfterAPositionalArgument.
//
// The flag package stops at the first non-flag argument, so `kenward revoke david
// --config …` would read the flag as a second member id. Operators write flags after
// the subject all the time, and refusing a reasonable line while naming the wrong
// problem is a bad way to meet somebody.
func TestFlagsAfterAPositionalArgument(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	if code := dispatch(h.e, []string{"revoke", "david", "--config", h.config}); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	if !strings.Contains(h.stdout(), "unbound") {
		t.Errorf("the revocation did not happen:\n%s", h.both())
	}
}

// TestHumanDuration renders a TTL the way somebody says it out loud.
func TestHumanDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"24h", "24 hours"},
		{"1h", "1 hour"},
		{"48h", "2 days"},
		{"30m", "30 minutes"},
		{"1m", "1 minute"},
	} {
		d := mustDuration(t, tc.in)
		if got := humanDuration(d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
