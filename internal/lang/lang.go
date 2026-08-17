// Package lang is every string a household member reads, in every language kenward
// is written in.
//
// # What is in here and what is not
//
// Only text that reaches a person through Telegram. Everything the model reads —
// the system prompt, the tool descriptions, the JSON schema descriptions — stays
// English and lives in internal/assistant, because the model is told the member's
// language by the persona and answers in it. Translating a prompt would change what
// the model is asked to do, not what a member is told, and docs/PROMPT.md is checked
// against those strings verbatim.
//
// The one function that straddles the line is remind.Reminder.When, which serves the
// member, the model's system prompt and the operator's CLI from one place. It stays
// English; Catalogue.When is the member's copy of it, and reminder_test.go asserts
// the prompt path never sees a translation.
//
// # Shape
//
// A struct with named fields, not a map. A missing map key is an empty string in
// front of a member; a missing field here is a name that does not exist, so every
// table breaks at the compiler the moment one is added or renamed, and
// TestEveryLanguageIsComplete catches a field left at its zero value. Entries that
// interpolate are func values, so each language writes its own Sprintf and the
// placeholder-order problem disappears — German puts the verb last, Arabic puts an
// isolate around the digits, and neither has to argue with a shared format string.
//
// The same reasoning kills the destination slot. English says "Saved X to your
// private memory" and reuses "your private memory" in nine sentences; German inflects
// it with the preposition, French contracts the article, Dutch changes the
// preposition itself (opslaan in / verwijderen uit / wegschrijven naar), and a shared
// noun phrase produces "verwijderd in je persoonlijke geheugen" — which claims the
// opposite of what happened. So the destination is not a parameter at all: Saved,
// Removed, UndoFailed and the rest take a bool and write both sentences out.
//
// Markup is applied inside the tables, never around them. transport.Bold and friends
// escape what they are given, so a member who titles a note "<b>" gets a note titled
// "<b>"; a translator who types "<b>" gets it shown as text, which is correct.
//
// Glyphs are structural rather than prose and stay outside: the caller prepends them.
package lang

import (
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// The language tags this package holds a full catalogue for.
//
// The list is the honest list, not an aspiration: a tag appears here only when every
// field below is written out for it. A missing translation is not a shorter table,
// it is a build that does not pass TestEveryLanguageIsComplete.
const (
	English    = "en"
	Spanish    = "es"
	Catalan    = "ca"
	Portuguese = "pt"
	French     = "fr"
	Italian    = "it"
	Dutch      = "nl"
	German     = "de"
	Chinese    = "zh"
	Arabic     = "ar"
)

// Catalogue is every user-facing string one language needs.
//
// Field order follows the sections a member meets them in: node errors, refusals,
// the retrieval line, reminders, capture, buttons, question outcomes, enrolment.
type Catalogue struct {
	// Tag is the language this table is written in.
	Tag string
	// Name is what the language calls itself.
	Name string
	// EnglishName is what an English-speaking operator calls it. It exists because
	// the setup wizard has to list the languages kenward's own messages are written
	// in, in a wizard that runs in English, and a list typed into that prose would
	// drift from this one the first time an eleventh table lands.
	EnglishName string

	// --- ERR: the node reporting that a turn produced nothing ---------------

	Locked        string
	ContentFilter string
	Queued        string
	Dropped       string
	NoAnswer      string
	ResetNotice   string

	// ToolMisfire is NoAnswer for the one empty turn whose cause the node knows: the
	// model tried to act — it called a tool — and named one that does not exist, so
	// the call was dropped and there was no reply behind it. A live turn called
	// "reminder" when the tool is "remind", and the member, who had asked for a
	// reminder, was told "I didn't get a usable answer to that".
	//
	// It is a separate string rather than a reworded NoAnswer because the two say
	// different true things. NoAnswer reports that nothing came back and reads,
	// fairly, as being about the question. This reports that kenward tried to do
	// something and got it wrong, which is about kenward — and it has to say that
	// nothing happened, because the member's request was an action and the honest
	// worry after silence is whether half of it landed.
	ToolMisfire string

	// NothingSaved goes under a reply that would leave the member believing something
	// was kept on a turn where the node kept nothing — see BareAcknowledgements and
	// SaveClaims. Under it and never instead of it: the assistant's answer always
	// reaches the member, and this is the node's own accounting added beneath it.
	//
	// It says the one thing the member cannot see and would otherwise get wrong,
	// and it invites them to ask again. It deliberately does not apologise for the
	// model or explain what a tool call is: it is only ever sent on a turn where the
	// member asked for something to be kept or the reply told them it had been, and
	// what they need is to know that it was not.
	NothingSaved string

	// BareAcknowledgements is every way this language says "done" and nothing else.
	//
	// It is the one entry in this table the member never reads. It is matched against
	// what the *model* wrote, which is written in the member's language, and this is
	// the only package that knows ten of those — the alternative is a second language
	// table in internal/assistant, which holds the model-facing English and should go
	// on holding only that.
	//
	// What belongs here: a reply that claims an action completed and carries no
	// information — "Done.", "Got it.", "Noted." A turn that made no tool call, was
	// asked for a write, and answered with one of these has told the member something
	// happened when nothing did, which is D-059 arriving through the reply instead of
	// through a claimed save.
	//
	// What must never be put here: an answer. "Yes", "No", "Correct" and their
	// equivalents answer questions, and a match here is what decides that a reply
	// needs the member's own request behind it before it earns a notice — so a word
	// that can carry information would be a real answer sent past the gate on the
	// wrong branch. Every entry is a word that cannot answer anything.
	//
	// A matched reply is no longer dropped. It was, and the argument for it — an
	// acknowledgement has nothing in it to lose — held only while the acknowledgement
	// was answering a save request. "Got it." after "thanks" is a legitimate reply and
	// was being deleted by a string match, so the caller appends and never substitutes.
	//
	// Entries are written naturally and normalized on both sides at match time, so
	// apostrophes, diacritics and punctuation need no thought here.
	BareAcknowledgements []string

	// SaveClaims is every way this language says "I have kept that" — the words, not
	// the shape of the sentence around them.
	//
	// The second field the member never reads, matched against what the model wrote,
	// and here for the same reason as the first. It exists because the first was not
	// enough. Once bare acknowledgements were corrected, a live run of seventeen
	// "remember this for me: …" turns produced three replies that named the fact and
	// claimed it kept — "Saved — plumber number is 555 0182.", "Noted — the garden
	// tap is shut off at the valve under the sink." — with nothing behind any of
	// them. Those are worse than "Done.": they tell the member exactly what was
	// stored, so the false belief is specific and checkable and will not be checked.
	//
	// What belongs here: vocabulary that can only be about a write that has already
	// happened. "Saved", "noted", "stored", "got it". These are false the moment they
	// are written on a turn that stored nothing, whatever the member asked for, so
	// this is the one list consulted unconditionally.
	//
	// What belongs in SavePromises instead: the future tense. "I'll remember" is a
	// promise and not a claim, and the difference showed up live — see that field.
	//
	// What must never be put here: a word that merely says an action finished.
	// "Done" and "Got it" say nothing about memory — "Done — the boiler service code
	// is 4471." is an answer to a question and must reach the member untouched — and
	// a table that held them would annotate every completed errand with a notice
	// about storage. That distinction is the whole difference between this field and
	// the one above, which is why they overlap in some languages ("saved", "noted")
	// and not in others ("done", "ok").
	//
	// Entries match as substrings of the reply, so a claim keeps matching when it is
	// conjugated, suffixed, or buried mid-sentence — which is where it lives in
	// Chinese and Arabic, neither of which puts a space in a useful place.
	//
	// ponytail: substring match, so "you saved £40" reads as a claim. The cost is one
	// extra true sentence appended to a reply nobody loses; the cost of missing a real
	// claim is a member who believes something is stored. Narrow the entries, never
	// the match, if the noise ever shows up in a live run — which it did, and
	// SavePromises is what came out of it.
	SaveClaims []string

	// SavePromises is every way this language says "I will keep that" — the future
	// tense of SaveClaims, and the negated future with it: "I'll remember", "I won't
	// forget", "I'll note that down".
	//
	// These were in SaveClaims and were moved out, and a live run is what moved them.
	// A promise counts as a lie in the context that field was written for — a member
	// says "remember this", the model says "I won't forget that", nothing is stored,
	// and the member believes it is — and it was three of the twenty samples the eval
	// first caught the defect on. But outside that context it is not a claim about
	// anything: sixteen turns of ordinary household chat produced *"Yep — drop me the
	// day next time and I'll keep it."*, which promises a future write, states plainly
	// that nothing was written now, and is the honest thing to say. Annotating it with
	// "I didn't record anything just then. Say it again if you want me to remember it."
	// contradicts an offer the member had just been made.
	//
	// So this list is gated on SaveRequests exactly as BareAcknowledgements is, and for
	// the same reason: a promise is false when it answers a request to keep something
	// and is merely a promise otherwise. SaveClaims stays unconditional, because a
	// completed claim is false whatever prompted it.
	//
	// Nothing is lost by the gate. A promise is dangerous precisely on the turn where
	// the member asked, and that is the turn AsksForASave matches.
	SavePromises []string

	// SaveRequests is every way this language asks for something to be kept —
	// "remember this", "write that down", "don't forget".
	//
	// The third field the member never reads, and the only one matched against what
	// the *member* wrote rather than what the model wrote. It is here because it is
	// the same ten-language problem as the two above and there is no second place to
	// put it.
	//
	// It exists to narrow the two guards whose replies are false only in context —
	// BareAcknowledgements and SavePromises — and must never be widened into a
	// detector of its own. Deciding "the member asked for a write" from free text is
	// exactly the primary detector internal/assistant refuses to build: a phrasing this
	// table missed would be a member left believing something was stored, silently. As
	// a filter the risk runs the other way — a miss leaves a bare "Done." or an unkept
	// promise uncorrected, and SaveClaims still catches every reply saying a save has
	// happened — which is what makes an admittedly incomplete list of phrases
	// acceptable here and unacceptable as the thing the guard rests on.
	//
	// What belongs here: the verbs and the imperatives a member uses when they hand
	// something over to be kept. Breadth is cheap. An entry that fires on a message
	// that was not a save request costs, at most, the notice appearing on a turn
	// where the model also wrote nothing but "Done." — which was the whole of the old
	// behaviour and is survivable; the cost of a missing entry is the guard not
	// firing where it should.
	//
	// What must never be put here: a word ordinary conversation is full of. "Thanks",
	// "ok" and "that's great" are the messages this filter exists to let through, and
	// a table that matched them would put the notice back in front of a member who
	// asked for nothing. That is the defect this field was added to fix.
	//
	// Substring-matched over the member's message like SaveClaims, so conjugations,
	// clitics and enclosing sentences fall out.
	SaveRequests []string

	// --- REF: refusals ------------------------------------------------------

	ModelBusy     string
	Misconfigured string
	TurnFailed    string
	ReasoningOnly string

	// RefusalEmptyChain is the refusal for a scope whose tier chain is empty.
	RefusalEmptyChain string
	// RefusalAssembled is the refusal for a chain that was walked and exhausted.
	// tried is a complete sentence and the space or punctuation that joins it to
	// what follows belongs to this template, not to a shared format string:
	// Chinese values end in 。, which is the sentence boundary, so a hardcoded
	// "%s " emits a stray space.
	RefusalAssembled func(whose, chain, tried, tierWord string) string
	// WhoseDirect and WhoseGroup name whose chain was walked. In several languages
	// they carry the preposition, already contracted — Catalan de+els becomes dels
	// and Portuguese de+os becomes dos — so the template must not add one.
	WhoseDirect string
	WhoseGroup  string
	// TierWord names the chain by size. Arabic has a dual; Chinese distinguishes
	// 这个 from 这些 on the demonstrative rather than the noun.
	TierWord func(n int) string
	// Chain renders the permitted tiers as a list. Names are raw: the table marks
	// them as code and isolates them if the script needs it.
	Chain func(names []string) string
	// Tried says what was attempted, for zero, one, two or many machines. Names
	// are raw, as in Chain.
	Tried func(names []string) string

	// --- RET: the retrieval accounting line ---------------------------------

	// Searched joins the parts into the line the member reads. The preposition
	// lives in the parts rather than the prefix: Portuguese em+a contracts to na,
	// so a shared prefix cannot be grammatical.
	Searched    func(parts []string) string
	PartPrivate func(count string) string
	PartShared  func(count string) string
	// Count renders one space's contribution. It is a plural function and not a
	// string: Arabic has six CLDR categories and five distinct forms, Chinese has
	// one. n is never delegated to a CLDR selector — CLDR French puts zero in the
	// one category, which would print "1 entrée" for no results.
	Count func(unreadable bool, n int) string

	// --- REM: reminders. The brackets are product surface. ------------------

	RemindFull    string
	RemindPast    string
	RemindFailed  string
	UnremindNone  string
	UnremindFails string
	// ReminderSet takes the schedule, the member's own text and the code they type
	// back to cancel it. The code keeps Latin digits in every language.
	ReminderSet       func(when, text, id string) string
	ReminderCancelled func(text string) string
	// When is the member's reading of a reminder's schedule, with the weekday and
	// month tables that Go's time package does not have.
	When func(r remind.Reminder, loc *time.Location) string

	// --- CAP: capture -------------------------------------------------------

	SaveFailed string
	AskFailed  func(title string) string
	// The destination-bearing announcements. private selects the member's own
	// memory over the household's; there is deliberately no destination string to
	// slot, because a bare noun phrase after a preposition is a grammar bug in
	// most of this table.
	Saved        func(private bool, title string) string
	SavedNoUndo  func(private bool, title string) string
	Removed      func(private bool, title string) string
	UndoFailed   func(private bool, title string) string
	StoreRefused func(private bool, title string) string

	WrongSpace        func(title string) string
	PublishNoShared   string
	PublishUnreadable string
	PublishAskFailed  func(title string) string
	PublishRefused    func(title string) string
	PublishWrongSpace func(title string) string
	Published         func(title string) string

	// OnlyOneProposal is the node saying it dropped a second proposal from one
	// turn. It rides on the reply, italic, like the retrieval line: it is the node
	// accounting for what it did, not the assistant talking.
	//
	// It must not be written as an apology or an offer, because the member is being
	// told about something that did not happen and the sentence has to survive being
	// read quickly. What it has to carry is both halves — one at a time, and the rest
	// was not saved — since half of it invites exactly the belief this line exists to
	// prevent.
	OnlyOneProposal string

	ProposalOpener   string
	ProposalNoDest   string
	ProposalWithDest func(private bool) string
	UndoExpiredNote  string
	WrittenOpener    func(private bool) string
	WrittenHint      string
	PromotionOpener  string
	PromotionCloser  string

	// AlsoKnownAs labels the member's own words for what an entry is about, on the
	// line capture appends to a stored body so that the entry can be retrieved in
	// the language it was said in.
	//
	// It is the one entry in this table that is not markup and must not become
	// markup: the line is written into lore and read back by the model as well as
	// shown to the member, so a <b> in it would be stored, indexed and eventually
	// read aloud. It is also the one entry that is joined by the table rather than
	// by the caller, because a list separator is a language's own — Chinese uses 、
	// and Arabic ، — and because Arabic's isolates are deliberately absent here:
	// they are invisible control characters and this string is stored.
	AlsoKnownAs func(words []string) string

	// EnglishGloss labels a one-line reading, in this language, of an entry whose
	// title and body are English — which every entry's are, deliberately, so that a
	// shared space stays usable by a household that does not all read one language.
	//
	// It exists because the capture question is a consent question. A Spanish member
	// was shown Spanish chrome around an English title and an English body and asked
	// whether to save it, which is asking somebody to approve wording they may not be
	// able to read. The whole capture design rests on the member seeing what will be
	// written, so the gloss is what makes the question answerable.
	//
	// It is the opposite of AlsoKnownAs in two ways, and both matter. It is never
	// stored — the entry is the English above it, and this line is presentation only,
	// which is why it may be model-written text that nobody has checked. And it must
	// say, in the same breath, that the text above is the English one: a reading in
	// the member's language that did not would leave them believing the store now
	// holds a Spanish entry, which is exactly the belief the design cannot afford.
	//
	// It is not markup and does not escape its argument. The caller wraps the whole
	// line, once, the way it wraps WrittenHint.
	EnglishGloss func(summary string) string

	// --- BTN: button labels. The ids are constants; only labels translate. ---

	BtnUndo             string
	BtnPublishHousehold string
	BtnCancel           string
	BtnSavePersonal     string
	BtnDontSave         string
	BtnPersonal         string
	BtnHousehold        string
	BtnSaveHousehold    string

	// --- SYS: the outcome line appended to a spent question -----------------

	// Dash introduces an outcome line. Chinese uses 破折号, Arabic needs a leading
	// RLM so the dash does not detach from the phrase it introduces when the
	// question above it was written in Latin script.
	Dash      string
	Declined  string
	Withdrawn string

	// --- ENR: the memory model, explained to the person it applies to -------

	EnrolPrivateHeading    string
	EnrolPrivateBody       string
	EnrolGroupHeading      string
	EnrolGroupBody         string
	EnrolMemoryHeading     string
	EnrolMemoryBodyDefault string
	EnrolMemoryBodyAsk     string

	// Notice wraps a line the node appends to the model's own answer inside one
	// Telegram message.
	//
	// It exists for one failure that only shows up in a right-to-left language.
	// Paragraph direction is decided by the first strong character of the whole
	// message, so a Latin-initial answer sets it left-to-right and the Arabic
	// fragments of an appended notice then lay out left to right relative to each
	// other — the sentence reads backwards. An RLI wrapper pins the notice's base
	// direction whatever precedes it. Every left-to-right language returns s.
	Notice func(s string) string
}

// OutcomeNotes is what this language calls a question nobody answered, in the shape
// the transport takes it. The transport cannot import this package — the tables call
// its markup helpers — so the words travel on the Question instead.
func (c Catalogue) OutcomeNotes() transport.OutcomeNotes {
	return transport.OutcomeNotes{Dash: c.Dash, Declined: c.Declined, Withdrawn: c.Withdrawn}
}

// IsBareAcknowledgement reports whether the whole of a reply is one of this
// language's contentless acknowledgements — see BareAcknowledgements.
//
// The whole of it, not the start of it: "Done — the code is 4471" carries the answer
// and is not this. Only a reply with nothing in it but the acknowledgement matches,
// which is what makes the reply's meaning depend entirely on the message it answers —
// and therefore what lets the caller ask AsksForASave before saying anything about it.
//
// Both sides are reduced to their letters and digits before comparing, so trailing
// punctuation, an emoji, a French apostrophe and an Arabic diacritic all fall out
// without a rule of their own. The length bound is what keeps this off the hot path
// for an ordinary reply: an acknowledgement is short in every language here.
func (c Catalogue) IsBareAcknowledgement(reply string) bool {
	if len(reply) > 80 {
		return false
	}
	got := letters(reply)
	if got == "" {
		return false
	}
	for _, ack := range c.BareAcknowledgements {
		if got == letters(ack) {
			return true
		}
	}
	return false
}

// ClaimsASave reports whether a reply tells the member something has been kept —
// see SaveClaims.
//
// Any part of it, unlike IsBareAcknowledgement, which needs the whole. That is the
// difference the two guards are for: an acknowledgement matches whole because a reply
// with nothing else in it means only what the message before it meant, and a claimed
// save matches anywhere because the claim arrives welded to the fact it is lying
// about, which is content the member keeps.
//
// There is no attempt to read tense, and it is deliberate. "Yes, I saved it earlier"
// is true about an earlier turn and matches here, and every rule that could tell the
// two apart — a list of words meaning "before", a check that the claim is about this
// turn's content — buys that one redundant sentence back at the price of missing "I
// already noted that" said about a note that never existed. One is noise and the
// other is the defect. The caller appends rather than replaces and its notice speaks
// only for this turn, which is what makes the noise survivable.
func (c Catalogue) ClaimsASave(reply string) bool {
	return containsAny(reply, c.SaveClaims)
}

// PromisesASave reports whether a reply promises to keep something in future — see
// SavePromises. Substring-matched like ClaimsASave, and meaningful only alongside
// AsksForASave: on its own a promise is not a false statement about this turn.
func (c Catalogue) PromisesASave(reply string) bool {
	return containsAny(reply, c.SavePromises)
}

// AsksForASave reports whether a member's message asks for something to be kept —
// see SaveRequests.
//
// Matched over the member's own message, and only ever used to narrow the two guards
// whose replies are false only in context — the bare acknowledgement and the promise.
// It is not, and must not become, the thing that decides whether a turn was supposed to
// store something: ClaimsASave is what catches a reply that says a save happened, and
// it runs whatever this returns.
func (c Catalogue) AsksForASave(text string) bool {
	return containsAny(text, c.SaveRequests)
}

// containsAny reports whether s contains any of the phrases, both sides reduced to
// their letters and digits first. Three of the four matchers here are this — the
// tables differ, the matching does not — and the fourth compares the whole string
// instead, which is the only real distinction among them.
func containsAny(s string, phrases []string) bool {
	got := letters(s)
	if got == "" {
		return false
	}
	for _, p := range phrases {
		if strings.Contains(got, letters(p)) {
			return true
		}
	}
	return false
}

// letters reduces a string to its lowercased letters and digits, single-spaced.
func letters(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " "))
}

// tables is every language, keyed by tag, with English filled in for any field a
// table left empty. The fill is a safety net and not a design: the completeness test
// asserts the raw tables need it nowhere, so nothing here should ever fire. It is
// per field rather than per table on purpose — falling back to a whole English table
// mid-conversation would be worse than one English sentence in a Catalan one.
var tables = func() map[string]Catalogue {
	m := make(map[string]Catalogue, len(raw))
	for tag, c := range raw {
		m[tag] = filled(c)
	}
	return m
}()

// raw is the tables as they are written, before the fill. It exists so the
// completeness test can assert that the fill has nothing to do: a test run against
// the filled tables would pass on a table with a hole in it, which is the whole
// failure it is there to catch.
var raw = map[string]Catalogue{
	English:    english,
	Spanish:    spanish,
	Catalan:    catalan,
	Portuguese: portuguese,
	French:     french,
	Italian:    italian,
	Dutch:      dutch,
	German:     german,
	Chinese:    chinese,
	Arabic:     arabic,
}

// filled returns c with every zero-valued field replaced by English's.
func filled(c Catalogue) Catalogue {
	dst := reflect.ValueOf(&c).Elem()
	src := reflect.ValueOf(english)
	for i := range dst.NumField() {
		if dst.Field(i).IsZero() {
			dst.Field(i).Set(src.Field(i))
		}
	}
	return c
}

// missing names the fields c leaves at their zero value. It is what the completeness
// test reports and it is exported to no one: the fill above means a caller can never
// observe an incomplete table, so the only thing that can ask is a test in this
// package.
func missing(c Catalogue) []string {
	v := reflect.ValueOf(c)
	t := v.Type()
	var out []string
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			out = append(out, t.Field(i).Name)
		}
	}
	return out
}

// For returns the catalogue for a language named the way a person names one.
// Anything this package does not hold gets English.
func For(language string) Catalogue { return tables[TagFor(language)] }

// IsEnglish reports whether language names English, which saying nothing does.
//
// It is not TagFor(language) == English and must not become it. TagFor falls back
// to English for anything this package has never heard of, which is right for
// choosing a table to read strings out of and wrong for every question of the form
// "is this conversation in English?" — a household writing in a language kenward
// holds no catalogue for is emphatically not one, and the caller that asks
// (internal/capture, deciding whether an alias could bridge anything) would draw
// exactly the wrong conclusion.
func IsEnglish(language string) bool {
	if strings.TrimSpace(language) == "" {
		return true
	}
	return aliases[normalize(language)] == English
}

// Spoken reports whether kenward's own strings exist in this language.
func Spoken(language string) bool {
	_, ok := aliases[normalize(language)]
	return ok
}

// EnglishNames is every language a catalogue exists for, named in English and in
// the order Tags gives them. It is what an operator-facing list is built from.
func EnglishNames() []string {
	tags := Tags()
	out := make([]string, len(tags))
	for i, tag := range tags {
		out[i] = tables[tag].EnglishName
	}
	return out
}

// Tags is every language a catalogue exists for, English first.
func Tags() []string {
	return []string{English, Spanish, Catalan, Portuguese, French, Italian, Dutch, German, Chinese, Arabic}
}

// aliases maps a language named the way a person names one onto a tag.
//
// config.PersonaConfig.Language is free text and has to be: it is passed to the
// model, not looked up in a table, and a household is entitled to ask for a register
// of a language kenward has never heard of. This package's copy is the opposite — a
// closed list of languages somebody has actually written and read — so the two are
// resolved against each other here and nowhere else.
var aliases = map[string]string{
	"en": English, "eng": English, "english": English, "inglés": English, "ingles": English,

	"es": Spanish, "spa": Spanish, "spanish": Spanish, "español": Spanish,
	"espanol": Spanish, "castellano": Spanish, "castilian": Spanish,

	"ca": Catalan, "cat": Catalan, "catalan": Catalan, "català": Catalan,
	"valencià": Catalan, "valenciano": Catalan, "valencian": Catalan,

	"pt": Portuguese, "por": Portuguese, "portuguese": Portuguese,
	"português": Portuguese, "portugues": Portuguese,
	"brazilian portuguese": Portuguese, "european portuguese": Portuguese,

	"fr": French, "fra": French, "fre": French, "french": French,
	"français": French, "francais": French,

	"it": Italian, "ita": Italian, "italian": Italian, "italiano": Italian,

	"nl": Dutch, "nld": Dutch, "dut": Dutch, "dutch": Dutch,
	"nederlands": Dutch, "flemish": Dutch, "vlaams": Dutch,

	"de": German, "deu": German, "ger": German, "german": German,
	"deutsch": German, "allemand": German,

	"zh": Chinese, "zho": Chinese, "chi": Chinese, "chinese": Chinese,
	"mandarin": Chinese, "中文": Chinese, "简体中文": Chinese, "汉语": Chinese,
	"simplified chinese": Chinese,

	"ar": Arabic, "ara": Arabic, "arabic": Arabic, "العربية": Arabic, "عربي": Arabic,
	"msa": Arabic, "modern standard arabic": Arabic,
}

// TagFor maps a language name onto a tag this package holds copy for, falling back
// to English for anything it does not.
func TagFor(language string) string {
	if tag, ok := aliases[normalize(language)]; ok {
		return tag
	}
	return English
}

// normalize reduces a written language name to something the alias table can hold.
// It tries the whole string first and then the part before a separator, so
// "pt-BR", "zh_Hans" and "Spanish (Latin America)" all land somewhere sensible.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := aliases[s]; ok {
		return s
	}
	if i := strings.IndexAny(s, "-_ ("); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// --- helpers the tables share ----------------------------------------------

// clock is HH:MM, which every language in this table renders the same way and every
// one of them keeps in Latin digits.
func clock(r remind.Reminder) string { return fmt.Sprintf("%02d:%02d", r.Hour, r.Minute) }

// codeJoin marks each name as code and joins them with sep. It is the shape most of
// the Latin-script tables want for a chain.
func codeJoin(names []string, sep string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = transport.Code(n)
	}
	return strings.Join(out, sep)
}

// naturalJoin is list grammar for a language whose conjunction is one invariable
// word with a space on each side: "a", "a and b", "a, b and c". Spanish, Arabic and
// Chinese each need their own and do not call this.
func naturalJoin(items []string, comma, and string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], comma) + and + items[len(items)-1]
	}
}

// codeAll marks every name as code, for a table that joins them itself.
func codeAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = transport.Code(n)
	}
	return out
}

// identity is Notice for every language that reads left to right.
func identity(s string) string { return s }

// latinList is the plain comma list every Latin-script table in here uses for
// AlsoKnownAs. It is shared rather than repeated because there is no grammar in it —
// no conjunction, no agreement, no contraction — which is precisely what makes it
// unlike Chain and Tried, where each language joins its own way and Chinese and
// Arabic do not use a comma at all.
func latinList(words []string) string { return strings.Join(words, ", ") }
