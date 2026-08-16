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
