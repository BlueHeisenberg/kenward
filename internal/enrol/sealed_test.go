package enrol

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// flat collapses a statement to one line so a claim can be compared across a hard
// wrap. internal/privacy's statements are wrapped prose and the catalogue's are not,
// so the same sentence differs by a newline and nothing else.
func flat(s string) string { return strings.ToLower(oneLine(s)) }

// TestSimpleOnboardingClaimsOnlyWhatPrivacySays is the check the violation needed and
// did not have: what a simple-mode member is told at enrolment, measured against what
// internal/privacy says simple mode delivers.
//
// internal/privacy is the source of truth for that, deliberately and with golden tests
// on it. What it never covered is internal/lang, which is where the sentence a member
// actually reads is written — so for as long as the sentence has existed, the first
// message every member got after redeeming a claim code said "Nobody else in the
// household can read it" while privacy.Statement(ModeSimple), six lines below its own
// "the household group can never read it", said "the person operating this computer
// can read every member's private memory".
//
// The link is made by quotation rather than by paraphrase. The English onboarding
// carries privacy's own clause word for word, and this asserts the clause is still in
// both places: soften internal/privacy and this fails, embellish internal/lang and
// this fails. A paraphrase would have let the two drift by a word at a time, which is
// exactly how the sentence being fixed here got written in the first place.
func TestSimpleOnboardingClaimsOnlyWhatPrivacySays(t *testing.T) {
	t.Parallel()

	en := lang.For(lang.English)
	simple := flat(privacy.Statement(privacy.ModeSimple))

	// The guarantee the onboarding makes, in privacy's words.
	const licensed = "stored in your own space, and the household group can never read it"
	if !strings.Contains(simple, licensed) {
		t.Fatalf("privacy.Statement(ModeSimple) no longer contains %q — the onboarding quotes it, so one of the two has moved", licensed)
	}
	if !strings.Contains(flat(en.EnrolPrivateBody), licensed) {
		t.Errorf("the onboarding stopped quoting privacy's simple-mode guarantee.\nwant it to contain: %q\ngot: %q", licensed, en.EnrolPrivateBody)
	}

	// And what it may not make. This is ownbot_test.go's ban, applied to the messages
	// a member receives rather than to the paragraph an operator reads.
	msgs := Explanation(1, en, false, privacy.ModeSimple, false)
	if len(msgs) == 0 {
		t.Fatal("Explanation sent nothing")
	}
	for _, m := range msgs {
		low := strings.ToLower(m.Text)
		for _, claim := range []string{
			"nobody else",
			"no one else",
			"neither can the person",
			"sealed",
		} {
			if strings.Contains(low, claim) {
				t.Errorf("simple-mode onboarding claims %q, which privacy.Statement(ModeSimple) does not support: %q", claim, m.Text)
			}
		}
	}
}

// TestSealedClaimIsIsolatedOnly is the structural half of the ban, and it is the half
// that holds in all ten languages without anyone reading ten languages.
//
// A per-language list of forbidden phrases would be a test of the translations: it
// passes on any wording its author did not anticipate, in scripts they may not read.
// What is language-independent is the wiring — the seal lives in one field and that
// field is rendered on one path — so that is what this pins, for every table.
func TestSealedClaimIsIsolatedOnly(t *testing.T) {
	t.Parallel()

	for _, tag := range lang.Tags() {
		c := lang.For(tag)
		for _, tc := range []struct {
			mode privacy.Mode
			want bool
		}{
			{privacy.ModeSimple, false},
			{privacy.ModeIsolated, true},
			// The zero value. A caller that never set a mode gets the sentence
			// that is true either way, because understating isolated mode is a
			// disappointment and overstating simple mode is a lie.
			{privacy.ModeUnknown, false},
		} {
			var all string
			for _, m := range Explanation(1, c, false, tc.mode, false) {
				all += m.Text + "\n"
			}
			if got := strings.Contains(all, c.EnrolPrivateSealed); got != tc.want {
				t.Errorf("%s: Explanation(%v, false) contains the sealed paragraph = %v, want %v", tag, tc.mode, got, tc.want)
			}
			// The both-modes body goes out whatever the mode is. If it ever did
			// not, a member would be left with only the strong claim and none of
			// what it is a claim about.
			if !strings.Contains(all, c.EnrolPrivateBody) {
				t.Errorf("%s: Explanation(%v, false) dropped the body every mode owes the member", tag, tc.mode)
			}
		}
	}
}

// TestIsolatedOnboardingDoesNotUnderstate. Scoping the copy so one sentence is true in
// both modes, and stopping there, would leave a household that arranged its own
// sealing told nothing about it — which is the other way to be dishonest about a
// privacy claim, and the reason Explanation takes a mode at all rather than the copy
// simply being made weaker.
//
// The strong claim and its limit are both internal/privacy's, verbatim. Quoting the
// limit alongside the claim is not decoration: privacy.go bounds every strong claim in
// the same breath it makes it, because a member who works out on their own that root
// on the box can reach a running key stops believing the sentence before it.
func TestIsolatedOnboardingDoesNotUnderstate(t *testing.T) {
	t.Parallel()

	isolated := flat(privacy.Statement(privacy.ModeIsolated))
	var all string
	for _, m := range Explanation(1, lang.For(lang.English), false, privacy.ModeIsolated, false) {
		all += m.Text + "\n"
	}
	flatAll := flat(all)

	for _, quoted := range []string{
		"nobody else in the household can read your private memory, and neither can the person who runs this machine",
		"someone with root access to this machine, while your assistant is running, could reach your key",
	} {
		if !strings.Contains(isolated, quoted) {
			t.Fatalf("privacy.Statement(ModeIsolated) no longer contains %q — the onboarding quotes it", quoted)
		}
		if !strings.Contains(flatAll, quoted) {
			t.Errorf("isolated onboarding no longer says %q", quoted)
		}
	}
}
