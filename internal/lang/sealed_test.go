package lang

import (
	"strings"
	"testing"
)

// sealedClaims are the phrases that tell a member their private memory is sealed
// against every other *person* — not against the group chat, which is a different
// claim and a true one in both modes.
//
// The distinction is the whole content of this list. "the household group can never
// read it" is a statement about a chat and it holds wherever kenward runs. "nobody
// else in the household can read it" is a statement about people, and in simple mode
// it is false: every member's key is in one process, so whoever operates the machine
// can read every member's private memory. internal/privacy says so in as many words,
// and internal/privacy is right.
//
// So this bans person-scoped denials and leaves group-scoped ones alone.
var sealedClaims = []string{
	"nobody else",
	"no one else",
	"no-one else",
	"nobody but",
	"nobody in the household",
	"no one in the household",
	"only you",
	"sealed",
}

// TestNoSealedMemoryClaimInMemberCopy is the ban that internal/privacy's
// ownbot_test.go makes inside internal/privacy, applied where the sentence a member
// actually reads is written.
//
// That ban never reached here. internal/privacy is careful, golden-tested and
// operator-facing; internal/lang is the ten tables that ship to members over
// Telegram, and it carried "Nobody else in the household can read it" as the first
// thing every member read after redeeming a claim code, in all ten languages, for as
// long as the sentence has existed. A rule enforced next door to the code that
// breaks it is not enforced.
//
// The check has two halves, and they are different in kind on purpose.
//
// English is checked by content, over rendered() — every string a table can put in
// front of a member, with every branch of every function taken — so a sealed claim
// that appears in some field nobody thought of is caught anyway, and a field added
// later is covered without an edit here.
//
// The other nine are checked by structure, not by content. A per-language list of
// forbidden substrings would be testing the translations rather than the product: it
// would pass on any phrasing the list's author did not think of in a language they
// may not read, and it would fail on an honest translation that chose a synonym. What
// holds across all ten instead is where the claim is allowed to live — one field,
// rendered on one path — which is checked here and in internal/enrol.
//
// What this cannot catch: a translator writing the seal into EnrolPrivateBody in a
// language nobody on the project reads. Nothing mechanical can catch that. What is
// mechanical is that the English source cannot say it, and that the field which may
// say it never reaches a simple-mode member.
func TestNoSealedMemoryClaimInMemberCopy(t *testing.T) {
	en := For(English)
	for _, s := range rendered(en) {
		if s == en.EnrolPrivateSealed {
			// The one field whose whole purpose is to make this claim, in the one
			// mode where it is true. enrol.TestSealedClaimIsIsolatedOnly is what
			// keeps it there.
			continue
		}
		low := strings.ToLower(s)
		for _, claim := range sealedClaims {
			if strings.Contains(low, claim) {
				t.Errorf("English member copy claims sealed memory (%q): %q\n"+
					"simple mode does not seal against the operator — see internal/privacy.Statement(ModeSimple)", claim, s)
			}
		}
	}

	// The structural half, and the only half that can hold in ten languages: every
	// table has exactly one place the claim may live, it is not empty, and it has not
	// been folded into the body that both modes send. A table that inlined the seal
	// into EnrolPrivateBody would put it in front of a simple-mode member, and the
	// English check above cannot see a Dutch sentence.
	for _, tag := range Tags() {
		c := For(tag)
		if strings.TrimSpace(c.EnrolPrivateSealed) == "" {
			t.Errorf("%s: EnrolPrivateSealed is empty — an isolated household would be told nothing about the one thing it bought", tag)
		}
		if strings.Contains(c.EnrolPrivateBody, c.EnrolPrivateSealed) {
			t.Errorf("%s: the sealed paragraph is inside EnrolPrivateBody, which both modes send:\n%q", tag, c.EnrolPrivateBody)
		}
	}
}
