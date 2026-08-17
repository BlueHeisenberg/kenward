package assistant

import (
	"slices"
	"strings"
	"testing"
)

// The handle of the bot the live household is addressed through. It is four words to
// lore's tokeniser and six is all the slots there are, which is the whole bug.
const liveBotHandle = "@kenward_hearth_e2e_bot"

// TestSearchTermsKeepsWhatWasAsked is the measured defect, in the four forms it was
// measured in: the same question with and without the handle, in the household's
// language and in English.
//
// Against the code before the fix, all four rows came back with the subject of the
// question missing or the bot's own name searched for:
//
//	"@kenward_hearth_e2e_bot ¿cuál es el código del portón del garaje?"
//	  -> [kenward hearth e2e bot cuál es]
//	"¿cuál es el código del portón del garaje?"
//	  -> [cuál es el código del portón]
//	"@kenward_hearth_e2e_bot what is the garage gate code?"
//	  -> [kenward hearth e2e bot garage gate]
//	"what is the garage gate code?"
//	  -> [garage gate code]
//
// The first row is what a Spanish household actually saw: an @mentioned question about
// the garage gate code, searched for as "kenward hearth e2e bot cuál es", answered with
// "I have not got that written down" over a shared memory that had it.
func TestSearchTermsKeepsWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		text    string
		mention string
		want    []string
	}{
		{"@kenward_hearth_e2e_bot ¿cuál es el código del portón del garaje?", liveBotHandle, []string{"código", "portón", "garaje"}},
		{"¿cuál es el código del portón del garaje?", "", []string{"código", "portón", "garaje"}},
		{"@kenward_hearth_e2e_bot what is the garage gate code?", liveBotHandle, []string{"garage", "gate", "code"}},
		{"what is the garage gate code?", "", []string{"garage", "gate", "code"}},
	} {
		got := searchTerms(tc.text, tc.mention)
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("searchTerms(%q)\n = %q\n want it to contain %q", tc.text, got, w)
			}
		}
		for _, w := range got {
			if strings.Contains(liveBotHandle, w) {
				t.Errorf("searchTerms(%q) = %q — %q is a word of the bot's own handle, not of the question", tc.text, got, w)
			}
		}
		if len(got) > maxSearchTerms {
			t.Errorf("searchTerms(%q) = %q — %d terms, cap is %d", tc.text, got, len(got), maxSearchTerms)
		}
	}
}

// The same question in both languages must be searched the same way whether the
// member named kenward or replied to it. Two ways of addressing one node that
// disagree about what it remembers is what the household saw.
func TestSearchTermsIgnoresHowTheBotWasAddressed(t *testing.T) {
	for _, tc := range []struct{ mentioned, plain string }{
		{"@kenward_hearth_e2e_bot ¿cuál es el código del portón del garaje?", "¿cuál es el código del portón del garaje?"},
		{"@kenward_hearth_e2e_bot what is the garage gate code?", "what is the garage gate code?"},
		{"hey @kenward_hearth_e2e_bot, ¿dónde están las llaves?", "hey, ¿dónde están las llaves?"},
	} {
		with := searchTerms(tc.mentioned, liveBotHandle)
		without := searchTerms(tc.plain, "")
		if !slices.Equal(with, without) {
			t.Errorf("addressing changed the search:\n mentioned %q -> %q\n plain     %q -> %q",
				tc.mentioned, with, tc.plain, without)
		}
	}
}

// A handle is removed wherever it sits and however it is capitalised — the transport
// hands over the substring the member typed, so the case is theirs, not the bot's.
func TestSearchTermsRemovesTheHandleWhereverItIs(t *testing.T) {
	for _, tc := range []struct{ text, mention string }{
		{"@kenward_hearth_e2e_bot ¿cuál es el código del portón?", liveBotHandle},
		{"¿cuál es el código del portón, @kenward_hearth_e2e_bot?", liveBotHandle},
		{"@Kenward_Hearth_E2E_Bot ¿cuál es el código del portón?", "@Kenward_Hearth_E2E_Bot"},
		{"@kenward_hearth_e2e_bot¿cuál es el código del portón?", liveBotHandle},
	} {
		got := searchTerms(tc.text, tc.mention)
		if !slices.Contains(got, "código") || !slices.Contains(got, "portón") {
			t.Errorf("searchTerms(%q, %q) = %q, want the question's own words", tc.text, tc.mention, got)
		}
		for _, w := range got {
			if strings.Contains(strings.ToLower(liveBotHandle), w) {
				t.Errorf("searchTerms(%q, %q) = %q — %q came from the handle", tc.text, tc.mention, got, w)
			}
		}
	}
}

// An empty mention must not be treated as a substring to remove: strings.ReplaceAll
// with "" inserts the replacement between every rune, which would turn a message into
// single letters and search for none of them.
func TestSearchTermsWithNoMentionLeavesTheMessageAlone(t *testing.T) {
	got := searchTerms("what is the garage gate code?", "")
	if want := []string{"garage", "gate", "code"}; !slices.Equal(got, want) {
		t.Errorf("searchTerms with no mention = %q, want %q", got, want)
	}
}

// Over the cap, the longest words win, and that is what carries a language whose
// function words are not in the English stopword list. Before the length preference
// this question searched [puedes decir por favor cuál es] — six terms, not one of
// them the noun, and "código" never searched at all.
func TestSearchTermsPrefersTheLongestWordsOverTheCap(t *testing.T) {
	got := searchTerms("¿me puedes decir por favor cuál es el código?", "")
	if !slices.Contains(got, "código") {
		t.Errorf("searchTerms = %q, want the subject of the question in it", got)
	}
	if len(got) != maxSearchTerms {
		t.Errorf("searchTerms = %q, want %d terms", got, maxSearchTerms)
	}
}

// A question that fits keeps the order it was asked in: the length preference is a
// tie-break for the overflowing case, not a reordering of every message.
func TestSearchTermsUnderTheCapKeepsTheOrderSpoken(t *testing.T) {
	got := searchTerms("where is the spare key for the shed?", "")
	if want := []string{"spare", "key", "shed"}; !slices.Equal(got, want) {
		t.Errorf("searchTerms = %q, want %q in the order they were said", got, want)
	}
}
