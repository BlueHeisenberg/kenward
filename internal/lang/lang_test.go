package lang

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
)

// sampleReminder is one reminder in each of the three shapes When has to render.
func sampleReminder(every remind.Every) remind.Reminder {
	return remind.Reminder{
		ID:      "4821",
		Text:    "Buy milk",
		Every:   every,
		Hour:    8,
		Minute:  30,
		Weekday: time.Wednesday,
		Next:    time.Date(2026, time.August, 15, 8, 30, 0, 0, time.UTC),
	}
}

// rendered is every string one table can put in front of a member, with every
// branch of every function taken. Tests that need "the whole catalogue" use it
// rather than listing fields, so a field added later is covered without an edit.
func rendered(c Catalogue) []string {
	out := []string{
		c.Locked, c.ContentFilter, c.Queued, c.Dropped, c.NoAnswer, c.ToolMisfire, c.ResetNotice,
		c.NothingSaved,
		c.ModelBusy, c.Misconfigured, c.TurnFailed, c.ReasoningOnly, c.RefusalEmptyChain,
		c.WhoseDirect, c.WhoseGroup,
		c.RemindFull, c.RemindPast, c.RemindFailed, c.UnremindNone, c.UnremindFails,
		c.SaveFailed, c.PublishNoShared, c.PublishUnreadable,
		c.ProposalOpener, c.ProposalNoDest, c.UndoExpiredNote, c.WrittenHint,
		c.PromotionOpener, c.PromotionCloser,
		c.BtnUndo, c.BtnPublishHousehold, c.BtnCancel, c.BtnSavePersonal,
		c.BtnDontSave, c.BtnPersonal, c.BtnHousehold, c.BtnSaveHousehold,
		c.Dash, c.Declined, c.Withdrawn,
		c.EnrolPrivateHeading, c.EnrolPrivateBody, c.EnrolPrivateSealed, c.EnrolGroupHeading, c.EnrolGroupBody,
		c.EnrolMemoryHeading, c.EnrolMemoryBodyDefault, c.EnrolMemoryBodyAsk,
		c.RefusalAssembled("W", "C", "T", "X"),
		c.Chain([]string{"local", "cloud"}),
		c.Searched([]string{"one", "two"}),
		c.PartPrivate("(n)"), c.PartShared("(n)"),
		c.ReminderSet("W", "text", "4821"), c.ReminderCancelled("text"),
		c.WrongSpace("Title"), c.AskFailed("Title"), c.PublishAskFailed("Title"),
		c.PublishRefused("Title"), c.PublishWrongSpace("Title"), c.Published("Title"),
		c.Notice("x"),
		c.EnglishGloss("what the entry says"),
	}
	for _, n := range []int{0, 1, 2, 3, 11, 103, 111} {
		out = append(out, c.TierWord(n), c.Tried(names(n)), c.Count(false, n))
	}
	out = append(out, c.Count(true, 0))
	for _, private := range []bool{true, false} {
		out = append(out,
			c.Saved(private, "Title"), c.SavedNoUndo(private, "Title"),
			c.Removed(private, "Title"), c.UndoFailed(private, "Title"),
			c.StoreRefused(private, "Title"), c.ProposalWithDest(private),
			c.WrittenOpener(private), c.NotSaved(private))
	}
	for _, e := range []remind.Every{remind.EveryOnce, remind.EveryDaily, remind.EveryWeekly} {
		out = append(out, c.When(sampleReminder(e), time.UTC))
	}
	return out
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "node"
	}
	return out
}

// TestEveryLanguageIsComplete is the point of a struct rather than a map: a missing
// map key is an empty string in front of a member, and a missing field here is a
// name that does not compile in any table. What a struct cannot catch on its own is
// a field left at its zero value, which is what this is for.
//
// It walks raw rather than tables. The filled tables would pass on a hole, because
// filling it is what they do.
func TestEveryLanguageIsComplete(t *testing.T) {
	if len(raw) != len(Tags()) {
		t.Fatalf("raw holds %d tables and Tags names %d", len(raw), len(Tags()))
	}
	for _, tag := range Tags() {
		c, ok := raw[tag]
		if !ok {
			t.Fatalf("Tags names %q and there is no table for it", tag)
		}
		if c.Tag != tag {
			t.Errorf("table %q says its tag is %q", tag, c.Tag)
		}
		if got := missing(c); len(got) != 0 {
			t.Errorf("table %q is missing %v", tag, got)
		}
	}
}

// TestNothingRendersEmpty catches the second half of the same failure: a field that
// is set but whose function returns nothing for some branch.
func TestNothingRendersEmpty(t *testing.T) {
	for _, tag := range Tags() {
		for i, s := range rendered(For(tag)) {
			if strings.TrimSpace(s) == "" {
				t.Errorf("%s: rendered string %d is empty", tag, i)
			}
		}
	}
}

// TestNotSavedIsALabel. The line under a struck entry is a label, not a paragraph.
//
// It said two sentences and thirty words once — "I didn't record this after all — I've
// taken it back out of your private memory. It won't come back in an answer, not here
// and not on any other device in the household." — for an event the member caused one
// second ago and can see struck through directly above it. Everything past "not saved
// to your private memory" was read on the first undo and skipped on every one after.
//
// Sixty characters is not a typographic law; it is a ceiling low enough that the
// paragraph cannot grow back without somebody deciding to raise it here.
func TestNotSavedIsALabel(t *testing.T) {
	for _, tag := range Tags() {
		for _, private := range []bool{true, false} {
			got := For(tag).NotSaved(private)
			if n := len([]rune(got)); n > 60 {
				t.Errorf("%s NotSaved(%v) is %d characters: %q", tag, private, n, got)
			}
			if strings.Count(got, ".")+strings.Count(got, "。") > 1 {
				t.Errorf("%s NotSaved(%v) is more than one sentence: %q", tag, private, got)
			}
		}
	}
}

// TestOnboardingTeachesWhatUndoBuys is where the promise above went.
//
// A member who undoes something sensitive wants to know whether it is gone from
// everyone's device, and lore's answer is specific: a delete writes a signed tombstone
// that propagates, so the entry stops coming back from an answer everywhere and the row
// is still on a disk. That is worth saying once, on the onboarding card that introduces
// the Undo button, and not on every undo. ARCHITECTURE.md's capture section requires the
// product to say which of the two it is somewhere a member reads; this is the somewhere,
// with Catalogue.Removed — the notice a failed edit falls back to — as the other.
//
// Asserted in English only, deliberately. The ten tables are not translations of each
// other and a substring test on nine of them is a test of my Arabic, not of the product.
// TestNothingRendersEmpty covers that they exist; the wording is reviewed by eye.
func TestOnboardingTeachesWhatUndoBuys(t *testing.T) {
	c := For(English)
	for _, want := range []string{"Undo", "won't come back in an answer", "not on any other device in the household"} {
		if !strings.Contains(c.EnrolMemoryBodyDefault, want) {
			t.Errorf("the onboarding memory card does not carry %q:\n%s", want, c.EnrolMemoryBodyDefault)
		}
	}
	// The same bound the message used to be held to. A tombstone is not a shred.
	for _, never := range []string{"erased", "destroyed", "wiped", "deleted forever"} {
		if strings.Contains(c.EnrolMemoryBodyDefault, never) {
			t.Errorf("the onboarding memory card promises %q; lore writes a tombstone", never)
		}
	}
	// The ask-mode card is not given the sentence: in that policy nothing is written
	// before the member picks a memory, so there is no Undo button on it to explain.
	if strings.Contains(c.EnrolMemoryBodyAsk, "won't come back in an answer") {
		t.Errorf("the ask-mode card explains an Undo button it never shows:\n%s", c.EnrolMemoryBodyAsk)
	}
}

// TestFrenchSpacing is a lint, and it exists because the thing it checks is
// invisible in a diff. French puts a space before ? : and ; and it must be U+00A0
// NO-BREAK SPACE — not an ordinary space, which lets the line break before the mark,
// and not U+202F NARROW NO-BREAK SPACE, which has patchy font coverage and renders
// as a missing-glyph box on some Android Telegram builds.
func TestFrenchSpacing(t *testing.T) {
	bad := regexp.MustCompile(`[^\x{00A0}][?:;]`)
	// A clock reading is not French punctuation and takes no space; nor is markup,
	// which is the transport's and carries ASCII of its own. Both are removed so
	// the rule below is the rule as written.
	clock := regexp.MustCompile(`\d{1,2}:\d{2}`)
	tags := strings.NewReplacer("<b>", "", "</b>", "", "<i>", "", "</i>", "",
		"<code>", "", "</code>", "")
	for _, s := range rendered(For(French)) {
		plain := clock.ReplaceAllString(tags.Replace(s), "")
		if m := bad.FindString(plain); m != "" {
			t.Errorf("French entry %q has %q — the mark must be preceded by U+00A0", plain, m)
		}
	}
}

// TestFrenchZeroIsNotOne. CLDR French puts n = 0 in the one category, so a generic
// CLDR selector prints "1 entrée" when a search found nothing. The rule here is
// hardcoded 0 / 1 / many for exactly that reason, and this is the test that fails if
// somebody replaces it with a library.
func TestFrenchZeroIsNotOne(t *testing.T) {
	c := For(French)
	if zero, one := c.Count(false, 0), c.Count(false, 1); zero == one {
		t.Errorf("French says %q for both no entries and one", zero)
	}
	if got := c.Count(false, 0); strings.Contains(got, "1") {
		t.Errorf("French renders no results as %q", got)
	}
}

// TestArabicPluralCategories. Arabic has six CLDR categories and five distinct
// shapes, and the n%100 rule is not decoration: 103 is few and 111 is many, so an
// n <= 10 shortcut is wrong above a hundred. few and many are the classic mistake
// and are genuinely different words.
func TestArabicPluralCategories(t *testing.T) {
	c := For(Arabic)
	few, many := c.Count(false, 5), c.Count(false, 15)
	if strings.Contains(few, "عنصرًا") {
		t.Errorf("5 rendered with the many form: %q", few)
	}
	if strings.Contains(many, "عناصر") {
		t.Errorf("15 rendered with the few form: %q", many)
	}
	if got, want := shape(c.Count(false, 103)), shape(few); got != want {
		t.Errorf("103 is few in Arabic; got the shape of %q", c.Count(false, 103))
	}
	if got, want := shape(c.Count(false, 111)), shape(many); got != want {
		t.Errorf("111 is many in Arabic; got the shape of %q", c.Count(false, 111))
	}
	// one and two carry the count in the morphology and interpolate no numeral at
	// all. A completeness test that demanded a digit in every count string would be
	// wrong here rather than the catalogue being wrong.
	for _, n := range []int{1, 2} {
		if got := c.Count(false, n); strings.ContainsAny(got, "0123456789") {
			t.Errorf("Arabic count for %d shows a numeral: %q", n, got)
		}
	}
	// The dual is the one place Arabic case is orthographically visible.
	if one, two, more := c.TierWord(1), c.TierWord(2), c.TierWord(3); one == two || two == more {
		t.Errorf("Arabic tier words do not distinguish singular, dual and plural: %q %q %q", one, two, more)
	}
	if two, more := c.Tried(names(2)), c.Tried(names(3)); two == more {
		t.Errorf("Arabic does not distinguish a dual from a plural in Tried: %q", two)
	}
}

// shape strips the digits so two renderings can be compared on their words alone.
func shape(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return -1
		}
		return r
	}, s)
}

// TestArabicIsolatesEveryNumericRun. UBA rule W2 gives a European Number the type of
// the last strong character before it, so digits following Arabic prose become
// Arabic Numbers and a renderer may draw them with national digit forms. The member
// then reads ٤٨٢١ where the server stored 4821 and cannot type the code back to
// cancel the reminder. The bytes are correct and the rendering is wrong, so nothing
// but this catches it.
func TestArabicIsolatesEveryNumericRun(t *testing.T) {
	c := For(Arabic)
	for _, s := range rendered(c) {
		for i, r := range s {
			if r < '0' || r > '9' {
				continue
			}
			// Walk back over the rest of this numeric run and whatever separates
			// digits inside one reading, then require the isolate.
			head := strings.TrimRight(s[:i], "0123456789:./")
			if !strings.HasSuffix(head, arLRI) {
				t.Errorf("Arabic string %q has a numeric run with no LRI before it", s)
				break
			}
		}
	}
}

// TestArabicNoticePinsBaseDirection. A notice is appended to the model's own answer
// inside one message, and paragraph direction comes from the first strong character
// of the whole message — so a Latin-initial answer flips the base and the Arabic
// fragments lay out backwards relative to each other.
func TestArabicNoticePinsBaseDirection(t *testing.T) {
	got := For(Arabic).Notice("x")
	if !strings.HasPrefix(got, arRLI) || !strings.HasSuffix(got, arPDI) {
		t.Errorf("Arabic Notice returned %q; it must wrap the line in RLI … PDI", got)
	}
	for _, tag := range Tags() {
		if tag == Arabic {
			continue
		}
		if got := For(tag).Notice("x"); got != "x" {
			t.Errorf("%s wraps a notice as %q; only an RTL language needs to", tag, got)
		}
	}
	// FSI is U+2068 and LRI is U+2066. Using LRI where FSI belongs forces an
	// Arabic-titled entry to render left to right, which for this audience is the
	// more common case.
	if arFSI != "⁨" || arLRI != "⁦" {
		t.Fatalf("the isolates are mislabelled: FSI=%q LRI=%q", arFSI, arLRI)
	}
	if got := For(Arabic).Saved(true, "Weekly Standup"); !strings.Contains(got, arFSI) {
		t.Errorf("a title of unknown direction was not first-strong isolated: %q", got)
	}
}

// TestListGrammarIsPerLanguage. naturalJoin was ", " and " and " hardcoded in the
// refusal builder. Each of these is a language where that is wrong in a different
// way.
func TestListGrammarIsPerLanguage(t *testing.T) {
	// Spanish y becomes e before a word beginning with the sound /i/, and machine
	// names are chosen by the household, so "pi e igloo" is reachable.
	if got := For(Spanish).Tried([]string{"pi", "igloo"}); !strings.Contains(got, " e ") {
		t.Errorf("Spanish joined before an i- word with y: %q", got)
	}
	if got := For(Spanish).Tried([]string{"pi", "hierro"}); !strings.Contains(got, " y ") {
		t.Errorf("Spanish alternated before hie-, where the word begins with a consonant sound: %q", got)
	}
	if got := For(Spanish).Tried([]string{"pi", "nuc"}); !strings.Contains(got, " y ") {
		t.Errorf("Spanish did not join with y: %q", got)
	}
	// Italian ed only before a word beginning with e, per current Crusca guidance.
	if got := For(Italian).Tried([]string{"alpha", "echo"}); !strings.Contains(got, " ed ") {
		t.Errorf("Italian did not use the d eufonica before echo: %q", got)
	}
	if got := For(Italian).Tried([]string{"beta", "alpha"}); strings.Contains(got, " ed ") {
		t.Errorf("Italian used ed before a vowel that is not e: %q", got)
	}
	// Arabic و is a proclitic: a space before it and none after.
	if got := For(Arabic).Tried([]string{"a", "b"}); !strings.Contains(got, " و") || strings.Contains(got, "و ") {
		t.Errorf("Arabic did not attach the wāw as a proclitic: %q", got)
	}
	// Chinese enumerates with 、 and has no coordinating word at all.
	zh := For(Chinese).Tried([]string{"a", "b", "c"})
	if strings.Count(zh, "、") != 2 {
		t.Errorf("Chinese did not join three items with two 顿号: %q", zh)
	}
	if strings.Contains(zh, "，") {
		t.Errorf("Chinese used a clause comma for a list: %q", zh)
	}
}

// TestNoDestinationIsSlotted. Any translated noun phrase that follows a preposition
// is a latent grammar bug, and the destination was nine of them. The catalogue writes
// each containing sentence out instead, so private and shared must differ by more
// than a substitution — this asserts they differ at all, which is what a table that
// reintroduced the slot would fail.
func TestNoDestinationIsSlotted(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		pairs := map[string][2]string{
			"Saved":            {c.Saved(true, "T"), c.Saved(false, "T")},
			"SavedNoUndo":      {c.SavedNoUndo(true, "T"), c.SavedNoUndo(false, "T")},
			"Removed":          {c.Removed(true, "T"), c.Removed(false, "T")},
			"UndoFailed":       {c.UndoFailed(true, "T"), c.UndoFailed(false, "T")},
			"StoreRefused":     {c.StoreRefused(true, "T"), c.StoreRefused(false, "T")},
			"ProposalWithDest": {c.ProposalWithDest(true), c.ProposalWithDest(false)},
			"WrittenOpener":    {c.WrittenOpener(true), c.WrittenOpener(false)},
		}
		for name, p := range pairs {
			if p[0] == p[1] {
				t.Errorf("%s: %s says the same thing about both memories: %q", tag, name, p[0])
			}
		}
	}
}

// TestInterpolationsSurvive. Each of these entries takes a value from outside and has
// to put it back; a translator dropping a placeholder is a message that silently
// stops naming the thing it is about.
func TestInterpolationsSurvive(t *testing.T) {
	const title = "ZZTITLEZZ"
	for _, tag := range Tags() {
		c := For(tag)
		with := map[string]string{
			"AskFailed":         c.AskFailed(title),
			"WrongSpace":        c.WrongSpace(title),
			"PublishAskFailed":  c.PublishAskFailed(title),
			"PublishRefused":    c.PublishRefused(title),
			"PublishWrongSpace": c.PublishWrongSpace(title),
			"Published":         c.Published(title),
			"Saved":             c.Saved(true, title),
			"SavedNoUndo":       c.SavedNoUndo(false, title),
			"Removed":           c.Removed(true, title),
			"UndoFailed":        c.UndoFailed(false, title),
			"StoreRefused":      c.StoreRefused(true, title),
		}
		for name, got := range with {
			if !strings.Contains(got, title) {
				t.Errorf("%s: %s dropped the title: %q", tag, name, got)
			}
		}
		if got := c.EnglishGloss(title); !strings.Contains(got, title) {
			t.Errorf("%s: EnglishGloss dropped the reading it exists to carry: %q", tag, got)
		}
		if got := c.ReminderSet("WHEN", "TEXT", "CODE"); !strings.Contains(got, "WHEN") ||
			!strings.Contains(got, "TEXT") || !strings.Contains(got, "CODE") {
			t.Errorf("%s: ReminderSet dropped a value: %q", tag, got)
		}
		if got := c.RefusalAssembled("WHOSE", "CHAIN", "TRIED", "TIER"); !strings.Contains(got, "WHOSE") ||
			!strings.Contains(got, "CHAIN") || !strings.Contains(got, "TRIED") || !strings.Contains(got, "TIER") {
			t.Errorf("%s: RefusalAssembled dropped a value: %q", tag, got)
		}
	}
}

// TestMemberTextIsEscaped. A reminder's text is the member's own words and rides on a
// message sent with a parse mode, so a reminder titled "<b>" must arrive as "<b>"
// rather than as markup somebody else chose.
func TestMemberTextIsEscaped(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		for name, got := range map[string]string{
			"ReminderSet":       c.ReminderSet("w", "<b>", "1"),
			"ReminderCancelled": c.ReminderCancelled("<b>"),
		} {
			if strings.Contains(got, "<b>") {
				t.Errorf("%s: %s passed markup through: %q", tag, name, got)
			}
			if !strings.Contains(got, "&lt;b&gt;") {
				t.Errorf("%s: %s lost the member's own text: %q", tag, name, got)
			}
		}
	}
}

// TestWeekdayAndMonthTablesAreDistinct. Go's time package has no localisation at all,
// so every language carries its own tables, and a table with a copy-paste in it reads
// as the wrong day of the week — which is the one thing a reminder must not do.
func TestWeekdayAndMonthTablesAreDistinct(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		seen := map[string]bool{}
		for d := time.Sunday; d <= time.Saturday; d++ {
			r := sampleReminder(remind.EveryWeekly)
			r.Weekday = d
			got := c.When(r, time.UTC)
			if seen[got] {
				t.Errorf("%s: two weekdays render the same: %q", tag, got)
			}
			seen[got] = true
		}
		months := map[string]bool{}
		for m := time.January; m <= time.December; m++ {
			r := sampleReminder(remind.EveryOnce)
			r.Next = time.Date(2026, m, 15, 8, 30, 0, 0, time.UTC)
			got := c.When(r, time.UTC)
			if months[got] {
				t.Errorf("%s: two months render the same: %q", tag, got)
			}
			months[got] = true
		}
	}
}

// TestCatalanDateElision. Catalan writes "15 d'agost", not "15 de agost", and
// "l'1 de gener", not "el 1 de gener". Both are formatter rules rather than table
// entries, so both need a test.
func TestCatalanDateElision(t *testing.T) {
	c := For(Catalan)
	r := sampleReminder(remind.EveryOnce)
	r.Next = time.Date(2026, time.August, 15, 8, 30, 0, 0, time.UTC)
	if got := c.When(r, time.UTC); !strings.Contains(got, "d'agost") {
		t.Errorf("Catalan did not elide de before a vowel: %q", got)
	}
	r.Next = time.Date(2026, time.January, 1, 8, 30, 0, 0, time.UTC)
	if got := c.When(r, time.UTC); !strings.Contains(got, "l'1 de gener") {
		t.Errorf("Catalan did not elide the article before day 1: %q", got)
	}
}

// TestReminderCodeKeepsLatinDigits. The member reads the code off the screen and
// types it back to cancel the reminder, in every language.
func TestReminderCodeKeepsLatinDigits(t *testing.T) {
	for _, tag := range Tags() {
		if got := For(tag).ReminderSet("w", "t", "4821"); !strings.Contains(got, "4821") {
			t.Errorf("%s: the reminder code is not in the notice verbatim: %q", tag, got)
		}
	}
}

// TestNoMarkupIsTypedIntoTheCatalogue. The markup helpers escape what they are given
// and are the only thing that emits tags. A translator typing < is safe under Esc;
// one typing <b> should get it shown as text, and one who typed it into a static
// string here would get a message no helper ever escaped.
func TestNoMarkupIsTypedIntoTheCatalogue(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		for _, s := range []string{
			c.Locked, c.ContentFilter, c.Queued, c.Dropped, c.NoAnswer, c.ToolMisfire, c.ResetNotice,
			c.NothingSaved,
			c.ModelBusy, c.Misconfigured, c.TurnFailed, c.ReasoningOnly, c.RefusalEmptyChain,
			c.RemindFull, c.RemindPast, c.RemindFailed, c.UnremindNone, c.UnremindFails,
			c.SaveFailed, c.PublishNoShared, c.PublishUnreadable, c.ProposalOpener,
			c.ProposalNoDest, c.UndoExpiredNote, c.WrittenHint, c.PromotionOpener,
			c.PromotionCloser, c.Declined, c.Withdrawn,
			c.EnrolPrivateBody, c.EnrolGroupBody, c.EnrolMemoryBodyDefault, c.EnrolMemoryBodyAsk,
		} {
			if strings.ContainsAny(s, "<>&") {
				t.Errorf("%s: %q carries markup; the helpers own that", tag, s)
			}
		}
	}
}

// TestBareAcknowledgementsMatchWhatTheyAreFor. Every table's own entries match, with
// the punctuation, casing and emoji a model actually types around them — that is what
// the normalization on both sides is for, and it is the half that has to work in
// Arabic, where a diacritic sits between two letters, and in French, where an
// apostrophe does.
func TestBareAcknowledgementsMatchWhatTheyAreFor(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		if len(c.BareAcknowledgements) == 0 {
			t.Errorf("%s: no acknowledgements, so a bare \"Done.\" in this language reaches the member unchallenged", tag)
		}
		for _, ack := range c.BareAcknowledgements {
			for _, dressed := range []string{ack, ack + ".", ack + "!", strings.ToUpper(ack) + " 👍", "  " + ack + "  "} {
				if !c.IsBareAcknowledgement(dressed) {
					t.Errorf("%s: %q is in the table and %q does not match it", tag, ack, dressed)
				}
			}
			if c.IsBareAcknowledgement(ack + " — the boiler code is 4471") {
				t.Errorf("%s: %q matched a reply carrying an answer; only the whole reply may match, or replacing it would lose what the member asked for", tag, ack)
			}
		}
	}
}

// TestSaveClaimsMatchWhatTheyAreFor. Every table's own entries match where they
// actually appear — welded to the fact the claim is about, which is the shape the live
// run produced and the shape IsBareAcknowledgement cannot see, since none of these
// replies is bare.
func TestSaveClaimsMatchWhatTheyAreFor(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		if len(c.SaveClaims) == 0 {
			t.Errorf("%s: no save claims, so \"Saved — the plumber's number is 555 0182\" in this language reaches the member unchallenged", tag)
		}
		for _, claim := range c.SaveClaims {
			for _, dressed := range []string{
				claim, claim + ".", claim + " — 555 0182.",
				strings.ToUpper(claim) + "! 555 0182", "…, " + claim + " 555 0182",
			} {
				if !c.ClaimsASave(dressed) {
					t.Errorf("%s: %q is in the table and %q does not match it", tag, claim, dressed)
				}
			}
			if c.IsBareAcknowledgement(claim + " — 555 0182.") {
				t.Errorf("%s: %q with a fact welded to it read as a bare acknowledgement, and that reply would be dropped with the fact in it", tag, claim)
			}
		}
	}
}

// TestSavePromisesMatchWhatTheyAreFor, and the rule that keeps the gate from being
// bypassed: no promise may also be a save claim.
//
// SaveClaims is consulted unconditionally and SavePromises only behind AsksForASave, so
// an entry in both is an entry with no gate — the phrase would fire in ordinary
// conversation through the unconditional list and the split would have bought nothing.
// That is not hypothetical: every entry here was in SaveClaims until a live run found
// one of them under an honest offer.
func TestSavePromisesMatchWhatTheyAreFor(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		if len(c.SavePromises) == 0 {
			t.Errorf("%s: no save promises, so \"I won't forget that\" answering an explicit request reaches the member unchallenged in this language", tag)
		}
		for _, p := range c.SavePromises {
			for _, dressed := range []string{
				p, p + ".", p + " — 555 0182.", strings.ToUpper(p) + "!", "…, " + p + ".",
			} {
				if !c.PromisesASave(dressed) {
					t.Errorf("%s: %q is in the table and %q does not match it", tag, p, dressed)
				}
			}
			if c.ClaimsASave(p) {
				t.Errorf("%s: %q is in SavePromises and also matches SaveClaims, which is consulted unconditionally — the promise would fire in ordinary conversation anyway and the gate would be doing nothing", tag, p)
			}
		}
	}
	if For("").PromisesASave("") {
		t.Error("an empty reply promised a save; nothing was said, so nothing was promised")
	}
}

// TestTheLivePromiseInOrdinaryChatIsNotAClaim. The exact sentence a live run of
// sixteen ordinary turns produced, in answer to "ok, no problem" and nothing else.
// It offers a future write and says plainly that nothing has been written, so it is
// the honest reply — and it was being annotated with a notice that contradicted it.
func TestTheLivePromiseInOrdinaryChatIsNotAClaim(t *testing.T) {
	const reply = "Yep — drop me the day next time and I'll keep it."
	c := For("")
	if c.ClaimsASave(reply) {
		t.Errorf("%q read as a claim that something had been kept; it says the opposite, and it is consulted with no gate in front of it", reply)
	}
	if !c.PromisesASave(reply) {
		t.Errorf("%q did not read as a promise, so the same sentence answering \"remember this\" would go out uncorrected", reply)
	}
	if c.AsksForASave("ok, no problem") {
		t.Error(`"ok, no problem" read as a request to keep something`)
	}
}

// TestSaveRequestsMatchWhatTheyAreFor. Every table's own entries match inside the
// sentence a member wraps them in, which is the only shape they ever arrive in.
func TestSaveRequestsMatchWhatTheyAreFor(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		if len(c.SaveRequests) == 0 {
			t.Errorf("%s: no save requests, so the bare-acknowledgement guard never fires in this language and a bare \"Done.\" to an explicit request reaches the member unchallenged", tag)
		}
		for _, req := range c.SaveRequests {
			for _, dressed := range []string{
				req, req + " 555 0182", strings.ToUpper(req) + "! 555 0182",
				"…, " + req + " 555 0182",
			} {
				if !c.AsksForASave(dressed) {
					t.Errorf("%s: %q is in the table and %q does not match it", tag, req, dressed)
				}
			}
		}
	}
}

// TestOrdinaryConversationIsNotASaveRequest is the defect this table was added to fix,
// asserted at the table rather than at the guard.
//
// A member said "thanks", the model said "Got it.", and they were told to say it again
// if they wanted it remembered. Nothing here may read as a request to keep something:
// these are the messages a household sends between the ones that matter, and the whole
// job of SaveRequests is to let them through the bare-acknowledgement arm.
func TestOrdinaryConversationIsNotASaveRequest(t *testing.T) {
	for tag, chat := range map[string][]string{
		English:    {"thanks", "ok", "thank you!", "great, cheers", "that's perfect", "sounds good", "morning", "how are you?", "no worries", "yep"},
		Spanish:    {"gracias", "vale", "muchas gracias", "perfecto", "buenos días", "¿qué tal?", "genial", "de nada", "sí", "hasta luego"},
		Catalan:    {"gràcies", "d'acord", "moltes gràcies", "perfecte", "bon dia", "com va?", "genial", "de res", "sí", "fins ara"},
		Portuguese: {"obrigado", "está bem", "muito obrigada", "perfeito", "bom dia", "tudo bem?", "ótimo", "de nada", "sim", "até logo"},
		French:     {"merci", "d'accord", "merci beaucoup", "parfait", "bonjour", "ça va ?", "super", "de rien", "oui", "à plus"},
		Italian:    {"grazie", "va bene", "grazie mille", "perfetto", "buongiorno", "come stai?", "ottimo", "di niente", "sì", "a dopo"},
		Dutch:      {"bedankt", "oké", "dank je wel", "perfect", "goedemorgen", "hoe gaat het?", "top", "graag gedaan", "ja", "tot straks"},
		German:     {"danke", "okay", "danke dir", "perfekt", "guten morgen", "wie geht's?", "super", "gern geschehen", "ja", "bis später"},
		Chinese:    {"谢谢", "好的", "多谢", "太好了", "早上好", "你好吗？", "没问题", "不客气", "是的", "回头见"},
		Arabic:     {"شكرا", "تمام", "شكرا جزيلا", "ممتاز", "صباح الخير", "كيف حالك؟", "رائع", "عفوا", "نعم", "إلى اللقاء"},
	} {
		c := For(tag)
		for _, s := range chat {
			if c.AsksForASave(s) {
				t.Errorf("%s: %q read as a request to keep something; a member who says that and gets a bare acknowledgement back would be told to say it again if they want it remembered, and they asked for nothing", tag, s)
			}
		}
	}
	if For("").AsksForASave("") {
		t.Error("an empty message asked for a save; nothing was said, so nothing was asked")
	}
}

// TestTheLiveSaveRequestsAreRecognised: the exact member messages the guard's own
// tests put to a model, one per language, matched by the table that would run for
// them. A phrasing that stops matching here is a bare "Done." going out uncorrected.
func TestTheLiveSaveRequestsAreRecognised(t *testing.T) {
	for tag, ask := range map[string]string{
		English:    "remember this for me: the plumber's number is 555 0182",
		Spanish:    "apunta esto: el número del fontanero es el 555 0182",
		Catalan:    "apunta això: el número del lampista és el 555 0182",
		Portuguese: "guarda isto: o número do canalizador é 555 0182",
		French:     "retiens ça : le numéro du plombier est le 555 0182",
		Italian:    "ricorda questo: il numero dell'idraulico è 555 0182",
		Dutch:      "onthoud dit: het nummer van de loodgieter is 555 0182",
		German:     "merk dir das: die Nummer des Klempners ist 555 0182",
		Chinese:    "帮我记住：水管工的电话是 555 0182",
		Arabic:     "احفظ هذا: رقم السباك هو 555 0182",
	} {
		if !For(tag).AsksForASave(ask) {
			t.Errorf("%s: %q did not read as a request to keep something, so a reply of \"Done.\" to it would go out uncorrected", tag, ask)
		}
	}
}

// TestNoSaveClaimIsMerelyAnErrandFinished is the line between the two tables, and it
// is drawn between completion and possession.
//
// "Done" says an action finished and says nothing about memory, so a reply carrying it
// is routinely an answer — "Done — the boiler service code is 4471." — and the product
// must send that untouched, which TestAnAcknowledgementCarryingAnAnswerIsLeftAlone
// asserts. "Got it" is the other side of the line and is in SaveClaims: it says the
// node is holding the thing the member just handed it, which is a claim about storage,
// and it is where the residue was measured live.
//
// A consequence worth naming: "Done — <the fact>" on a turn that stored nothing is
// still uncaught, and cannot be caught without breaking the reply that carries an
// answer. It is the known hole, and the prompt is the only thing that can reach it.
func TestNoSaveClaimIsMerelyAnErrandFinished(t *testing.T) {
	for _, s := range []string{
		"Done — the boiler service code is 4471.",
		"OK, the bins go out on Tuesday.",
		"Yes.",
		"We went in the spring of 2019, four nights, and you got sunburnt.",
	} {
		if For("").ClaimsASave(s) {
			t.Errorf("English: %q read as a claim to have saved something; it is an answer, and the product would annotate it with a notice about memory", s)
		}
	}
	if For("").ClaimsASave("") {
		t.Error("an empty reply claimed a save; nothing was said, so nothing was claimed")
	}
}

// TestNoAcknowledgementCanCarryAnAnswer is the rule that makes replacing a matched
// reply safe rather than destructive.
//
// The caller drops the reply outright, so a word that can answer a question must never
// be in the table: a member who asks "is bin day Thursday?" and is answered "Yes." must
// read that answer, not a notice about memory. Acknowledgements cannot answer anything,
// which is the whole basis of the guard.
func TestNoAcknowledgementCanCarryAnAnswer(t *testing.T) {
	answers := []string{
		"yes", "no", "correct", "sí", "no", "sim", "não", "oui", "non", "sí, és",
		"ja", "nee", "nein", "是", "不是", "对", "نعم", "لا",
	}
	for _, tag := range Tags() {
		c := For(tag)
		for _, a := range answers {
			if c.IsBareAcknowledgement(a) {
				t.Errorf("%s: %q is treated as a bare acknowledgement, and a matched reply is dropped — a member asking a yes-or-no question would lose the answer", tag, a)
			}
		}
	}
	if For("").IsBareAcknowledgement("") {
		t.Error("an empty reply matched; nothing was said, so there is nothing to replace")
	}
}

// TestTagForResolvesTheWaysAPersonNamesALanguage. PersonaConfig.Language is free text
// and has to be — it is passed to the model rather than looked up — so this is the one
// place the two halves of the setting are reconciled.
func TestTagForResolvesTheWaysAPersonNamesALanguage(t *testing.T) {
	for in, want := range map[string]string{
		"":                     English,
		"English":              English,
		"español":              Spanish,
		" Castellano ":         Spanish,
		"Català":               Catalan,
		"Brazilian Portuguese": Portuguese,
		"pt-BR":                Portuguese,
		"Français":             French,
		"italiano":             Italian,
		"Nederlands":           Dutch,
		"Deutsch":              German,
		"中文":                   Chinese,
		"zh-Hans":              Chinese,
		"العربية":              Arabic,
		"Klingon":              English,
	} {
		if got := TagFor(in); got != want {
			t.Errorf("TagFor(%q) = %q, want %q", in, got, want)
		}
	}
	if Spoken("Klingon") {
		t.Error("Spoken claims a language there is no table for")
	}
	if !Spoken("Català") {
		t.Error("Spoken denies a language there is a table for")
	}
}

// TestButtonNamesInProseMatchTheButtons. The onboarding tells a member which button to
// tap, and it used to name Save — a label that does not exist, because the buttons are
// "Save to personal" and "Save to household". Naming a button the member will never
// see is a small lie in the one message that explains the product.
func TestButtonNamesInProseMatchTheButtons(t *testing.T) {
	for _, tag := range Tags() {
		c := For(tag)
		if !strings.Contains(c.EnrolMemoryBodyDefault, c.BtnSaveHousehold) {
			t.Errorf("%s: the default onboarding body does not name the %q button:\n%s",
				tag, c.BtnSaveHousehold, c.EnrolMemoryBodyDefault)
		}
		if !strings.Contains(c.EnrolMemoryBodyAsk, c.BtnDontSave) {
			t.Errorf("%s: the ask onboarding body does not name the %q button:\n%s",
				tag, c.BtnDontSave, c.EnrolMemoryBodyAsk)
		}
	}
}

// TestUndoAndCancelAreDifferentWords. French and Italian both spell Undo and Cancel
// Annuler and Annulla, and both buttons exist in this product. A member looking at
// two buttons with the same word on them has no way to choose.
func TestUndoAndCancelAreDifferentWords(t *testing.T) {
	for _, tag := range Tags() {
		if c := For(tag); c.BtnUndo == c.BtnCancel {
			t.Errorf("%s: Undo and Cancel are both %q", tag, c.BtnUndo)
		}
	}
}
