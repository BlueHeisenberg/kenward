package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/transport"
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
	//
	// Scanned around the code rather than through it: the code is random Crockford
	// Base32, so it contains "QR" — or any other banned pair — by chance often enough
	// to fail this test roughly one run in four. A code is not a QR image, and a test
	// about QR images must not depend on which letters the generator picked. The code
	// is printed alone on its own indented line, so the rest of the output is exactly
	// the prose this assertion is about.
	code, prose := splitClaimCode(t, out)
	for _, unwanted := range []string{"http://", "https://", "t.me/", "QR", "qr"} {
		if strings.Contains(prose, unwanted) {
			t.Errorf("invite printed %q, which would leak the code into a chat log:\n%s", unwanted, out)
		}
	}
	// And the code itself appears once, in that one place. A deep link would carry a
	// second copy, which is the leak the substrings above are a proxy for.
	if n := strings.Count(out, code); n != 1 {
		t.Errorf("the claim code appears %d times, want 1 — a second copy is a second place it can be read:\n%s", n, out)
	}
	h.assertNoSecrets(t)
}

// splitClaimCode returns the claim code `invite` printed and the rest of the output.
// The code is the one non-empty indented line; everything else is prose.
func splitClaimCode(t *testing.T, out string) (code, prose string) {
	t.Helper()
	var rest []string
	for _, line := range strings.Split(out, "\n") {
		if code == "" && strings.HasPrefix(line, "    ") && strings.TrimSpace(line) != "" {
			code = strings.TrimSpace(line)
			continue
		}
		rest = append(rest, line)
	}
	if code == "" {
		t.Fatalf("invite printed no claim code on a line of its own:\n%s", out)
	}
	return code, strings.Join(rest, "\n")
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
	yaml = strings.Replace(yaml, "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19", "b1c8d42f-3a56-4e09-8f77-2d9e6a10b345", 1)

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
	// david claimed, which is what puts a binding in the state file. A telegram_id
	// written into the configuration by hand is a different thing and revoke refuses
	// it — see TestRevokeRefusesWhileTheConfigurationDeclaresTheTelegramID.
	h := newHarness(t, claimedYAML, fullEnvironment())
	h.claimedInState(t, "david", 12345678)

	if code := h.run("revoke", "david"); code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, h.both())
	}
	out := h.stdout()
	if !strings.Contains(out, "7d5047bb-d939-4539-b3db-8b6221a2e245") {
		t.Errorf("the revocation does not name the space whose key must be rotated:\n%s", out)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "rotate") || !strings.Contains(lower, "key") {
		t.Errorf("the revocation does not state the key-rotation caveat:\n%s", out)
	}
	// The canonical wording lives in internal/enrol, not here.
	if !strings.Contains(out, enrol.Revocation{
		Member: mustMember(t, mustLoad(t, simpleYAML), "david"),
		Space:  "7d5047bb-d939-4539-b3db-8b6221a2e245",
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
	h := newHarness(t, claimedYAML, fullEnvironment())
	h.claimedInState(t, "david", 12345678)
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

// TestIsolatedInviteReachesThePodThatRedeemsIt is the journey the operator actually
// makes, end to end, and it is the check on the last gap D-023 had.
//
// `kenward invite` mints into this node's own store. In isolated mode the process that
// has to redeem the code is not this one: it is the member's pod, reading its own
// invite store on its own volume, on a filesystem this machine's store is not part of.
// Nothing bridged them. The operator ran `kenward invite --name Jordan`, handed the
// code over, and jordan's pod — which by then exists and is waiting, claim-only —
// refused it and said nothing, because silence is what enrolment owes a sender it does
// not recognise. Every step reported success and the member was never enrolled.
//
// The bridge is a per-member seed file the deployment carries into the pod, and this
// walks all three legs: mint on the host, carry the file, redeem in the pod.
func TestIsolatedInviteReachesThePodThatRedeemsIt(t *testing.T) {
	t.Parallel()
	// jordan is declared and has not claimed — the state D-023 is about.
	yaml := strings.Replace(isolatedYAML,
		"    telegram_id: 87654321\n", "", 1)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("invite exit = %d, want 0\n%s", code, h.both())
	}
	plaintext, _ := splitClaimCode(t, h.stdout())

	// Leg two: the file the deployment carries. It is jordan's, it is where both
	// deployment paths look for it, and it holds no plaintext.
	dataDir := filepath.Join(h.dir, "data")
	seed := filepath.Join(dataDir, inviteSeedDirName, "jordan.json")
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("`kenward invite` wrote no seed for jordan's pod, so the code cannot reach it: %v", err)
	}
	if strings.Contains(string(raw), strings.ReplaceAll(plaintext, "-", "")) {
		t.Fatalf("the seed carries the code in the clear:\n%s", raw)
	}
	// Digests only, and one member's. The host's own store holds every member's.
	if strings.Contains(string(raw), "David") {
		t.Errorf("jordan's seed carries david's invites:\n%s", raw)
	}

	// Leg three: the pod. A different data directory on a different filesystem —
	// which is the whole point — with the seed provisioned in as the deployment does
	// it, and `run --member=jordan --invites=…` importing it on the way up.
	podDir := t.TempDir()
	podCfg := &config.Config{DataDir: podDir}
	if err := importInvites(h.e, podCfg, seed, nil); err != nil {
		t.Fatalf("the pod could not import its invites: %v", err)
	}

	pod := inviteStore(podCfg)
	binder := &recordingBinder{}
	claimer, err := enrol.New(pod, binder, enrol.WithClock(h.e.now))
	if err != nil {
		t.Fatal(err)
	}
	msg := transport.Inbound{ChatID: 99, UserID: 99, Text: plaintext, At: h.e.now()}
	res, err := claimer.Handle(context.Background(), msg)
	if err != nil || !res.Enrolled {
		t.Fatalf("jordan's pod refuses the code the operator handed jordan: enrolled=%v err=%v", res.Enrolled, err)
	}
	if binder.id != "jordan" {
		t.Errorf("the code bound %q, not jordan", binder.id)
	}

	// And it is single-use across the crossing, not just within one store. The seed
	// on the host never learns the code was spent — consumption happens in the pod —
	// so a second import must not restore it. A pod is recreated on every rolling
	// update, and if it did, one code would enrol somebody twice.
	if _, err := copyInvites(context.Background(), pod, enrol.NewFileStore(seed), nil); err != nil {
		t.Fatalf("re-importing the seed: %v", err)
	}
	if _, err := claimer.Handle(context.Background(), msg); !errors.Is(err, enrol.ErrCodeConsumed) {
		t.Fatalf("after re-importing the seed the spent code is redeemable again (err = %v); a pod is recreated on every rolling update", err)
	}
}

// recordingBinder is the member set enrolment binds into, reduced to what this test
// asks of it: which member the code named.
type recordingBinder struct{ id domain.MemberID }

func (b *recordingBinder) Bind(_ context.Context, id domain.MemberID, name string, telegramID int64, _ time.Time) (domain.Member, error) {
	b.id = id
	return domain.Member{ID: id, Name: name, TelegramID: telegramID}, nil
}

func (b *recordingBinder) Unbind(context.Context, domain.MemberID) (domain.Member, error) {
	return domain.Member{}, enrol.ErrUnknownMember
}

// TestSimpleInviteWritesNoSeed. The seed exists for the crossing between a host and a
// pod, and simple mode has no crossing: one process mints and redeems against one
// file. Writing a second copy of every household digest there would be a second file
// to lose for nothing.
func TestSimpleInviteWritesNoSeed(t *testing.T) {
	t.Parallel()
	yaml := strings.Replace(simpleYAML,
		"  - id: jordan\n    name: Jordan\n    telegram_id: 87654321\n",
		"  - id: jordan\n    name: Jordan\n", 1)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("invite exit = %d, want 0\n%s", code, h.both())
	}
	if _, err := os.Stat(filepath.Join(h.dir, "data", inviteSeedDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("simple mode wrote a pod seed directory (%v); there is no pod to carry it to", err)
	}
	if strings.Contains(h.stdout(), "isolated") {
		t.Errorf("simple mode printed isolated mode's delivery note:\n%s", h.stdout())
	}
}

// TestInviteMintsAgainstTheConfiguredMemberID.
//
// `kenward invite` finds a member by id, by name, or by the slug of their name, and
// used to mint the code against the *name* — so a household whose `id: jordy` carries
// `name: Jordan` got a code recorded for a member nobody declares. The Binder that
// redeems it will not create one, and refuses:
//
//	enrol: bind "jordan": config: no provisioning; a member the configuration
//	  does not declare cannot be created: member "jordan"
//
// which the person holding the code experiences as the silence enrolment owes a
// stranger. The id the configuration gave them travels with the mint instead.
func TestInviteMintsAgainstTheConfiguredMemberID(t *testing.T) {
	t.Parallel()
	yaml := strings.NewReplacer(
		"  - id: jordan\n", "  - id: jordy\n",
		"    telegram_id: 87654321\n", "",
	).Replace(isolatedYAML)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("invite exit = %d, want 0\n%s", code, h.both())
	}
	plaintext, _ := splitClaimCode(t, h.stdout())

	// Redeem it the way jordy's pod does: their own seed imported into a store of
	// their own, and the real Binder over the same configuration — the one that
	// refuses to invent a member.
	seed := filepath.Join(h.dir, "data", inviteSeedDirName, "jordy.json")
	podCfg := mustLoad(t, yaml)
	if err := importInvites(h.e, podCfg, seed, nil); err != nil {
		t.Fatalf("the pod could not import its invites: %v", err)
	}
	binder, err := newBinder(podCfg)
	if err != nil {
		t.Fatal(err)
	}
	claimer, err := enrol.New(inviteStore(podCfg), binder, enrol.WithClock(h.e.now))
	if err != nil {
		t.Fatal(err)
	}
	res, err := claimer.Handle(context.Background(), transport.Inbound{
		ChatID: 99, UserID: 99, Text: plaintext, At: h.e.now(),
	})
	if err != nil {
		t.Fatalf("the code minted for the declared member does not bind: %v", err)
	}
	if res.Member.ID != "jordy" {
		t.Errorf("the code bound %q, not the configured id jordy", res.Member.ID)
	}
}

// TestIsolatedInviteSeedSurvivesAnIdThatIsNotTheName is the same household seen from
// the file the deployment carries: the seed is named for the configured id and holds
// the code minted for it. It was written empty while the mint recorded a second id
// derived from the name, and reported success doing it.
func TestIsolatedInviteSeedSurvivesAnIdThatIsNotTheName(t *testing.T) {
	t.Parallel()
	yaml := strings.NewReplacer(
		"  - id: jordan\n", "  - id: jordy\n",
		"    telegram_id: 87654321\n", "",
		"KENWARD_BOT_TOKEN_JORDAN", "KENWARD_BOT_TOKEN_JORDAN",
	).Replace(isolatedYAML)
	h := newHarness(t, yaml, fullEnvironment())

	if code := h.run("invite", "--name", "Jordan"); code != exitOK {
		t.Fatalf("invite exit = %d, want 0\n%s", code, h.both())
	}
	// Named for the configured id, because that is what the deployment looks up.
	seed := filepath.Join(h.dir, "data", inviteSeedDirName, "jordy.json")
	codes, err := enrol.NewFileStore(seed).All(context.Background())
	if err != nil {
		t.Fatalf("reading %s: %v", seed, err)
	}
	if len(codes) != 1 {
		t.Fatalf("jordy's seed holds %d codes, want the one just minted: %+v", len(codes), codes)
	}
}
