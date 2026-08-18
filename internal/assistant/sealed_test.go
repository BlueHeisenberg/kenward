package assistant

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// claimsSimpleModeCannotSupport is every way the scope disclosures have asserted, or
// could plausibly assert, that some *person* cannot read something.
//
// privacy.Statement(ModeSimple) licenses exactly one such assertion — the household
// group can never read a member's private space — and refuses every wider one in as
// many words: "the person operating this computer can read every member's private
// memory". Anything stronger is a claim about people, and in simple mode it is false.
//
// A phrase list is a blunt instrument, deliberately. It is enrol/sealed_test.go's list,
// applied to the text a model is sent rather than to the text a member is sent, because
// the two surfaces make the same claim about the same fact and a model that is told
// nobody can see a conversation will say so to the member who asks.
var claimsSimpleModeCannotSupport = []string{
	"nobody else",
	"no one else",
	"nobody but",
	"only they and you",
	"visible to anyone else",
	"anyone else in the household",
	"neither can the person",
	"sealed",
}

// TestDisclosuresClaimOnlyWhatSimpleModeSupports measures what the prompt tells the
// model about who can read a conversation against what internal/privacy says simple
// mode delivers.
//
// The prompt is not member-facing copy, which is why the guard added for
// internal/lang does not reach it, and it is still a place this product can lie: the
// model repeats it. "Nobody else can see it" in a household where the operator holds
// the bot token and every member's key in one process is the sentence a member is
// reassured by shortly before finding out otherwise.
//
// This is scanned over the rendered prompt rather than over the three constants, so a
// claim reintroduced anywhere in the assembly — a capture block, a persona note, a
// disclosure not yet written — fails here too.
//
// It asserts simple mode only. Isolated mode delivers more and is allowed to say so
// elsewhere; the prompt does not say it, and understating there costs a model some
// wording it never needed, where overstating here costs a member the one guarantee
// they were given.
func TestDisclosuresClaimOnlyWhatSimpleModeSupports(t *testing.T) {
	t.Parallel()

	for shape, system := range everyScopeShape(t) {
		low := strings.ToLower(flattened(system))
		for _, claim := range claimsSimpleModeCannotSupport {
			if strings.Contains(low, claim) {
				t.Errorf("the %s prompt claims %q, which privacy.Statement(ModeSimple) does not support:\n%s",
					shape, claim, system)
			}
		}
	}
}

// TestDirectDisclosureQuotesPrivacy is the positive half, and it is a quotation rather
// than a paraphrase for the reason enrol/sealed_test.go is: two paraphrases of one
// guarantee drift a word at a time, which is how the sentence this test was written
// for came to exist. Soften internal/privacy and this fails; embellish the disclosure
// and the test above fails.
func TestDirectDisclosureQuotesPrivacy(t *testing.T) {
	t.Parallel()

	// The one thing a private space is guaranteed against in either mode.
	const licensed = "the household group can never read"

	if !strings.Contains(flattened(privacy.Statement(privacy.ModeSimple)), licensed) {
		t.Fatalf("privacy.Statement(ModeSimple) no longer contains %q — the scope disclosure quotes it, so one of the two has moved", licensed)
	}
	if !strings.Contains(flattened(directDisclosureText), licensed) {
		t.Errorf("directDisclosureText stopped quoting privacy's simple-mode guarantee.\nwant it to contain: %q\ngot: %q", licensed, directDisclosureText)
	}
}
