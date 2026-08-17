package enrol

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Language tags this package can hold a whole conversation in.
//
// The list is short on purpose and it is the honest list, not an aspiration. Every
// string a member reads between "you're in" and the end of the tutorial is written
// out by hand below, twice, and a tag only appears here when both copies exist. A
// third language is a third table and nothing else — no loader, no bundle format,
// no fallback chain that silently degrades to English halfway through a sentence.
//
// It is deliberately not machine translation. This copy makes promises about where
// a member's words are stored and who can read them, and the package documentation
// for Onboarding already says wrong copy about privacy is worse than none. A
// sentence produced by a model at runtime is a sentence nobody has read; running the
// memory model through one would put the product's central claim on a path with no
// review in it.
const (
	// LangEnglish is the default and the language the wizard runs in.
	LangEnglish = "en"
	// LangSpanish is the second language the tutorial is written in.
	LangSpanish = "es"
)

// Choice ids. Stable and machine-readable; they are what comes back in an Answer and
// they never change with the language on screen.
const (
	choiceLangEnglish = "lang.en"
	choiceLangSpanish = "lang.es"
	choiceLangOther   = "lang.other"
	choiceToneFlat    = "tone.flat"
	choiceToneWarm    = "tone.warm"
	choiceTonePlayful = "tone.playful"
	choiceSkip        = "skip"
	choiceBack        = "back"
)

// skipWord and backWord are what a member types instead of tapping, on the two
// questions that take typed text and therefore have no buttons.
const (
	skipWord = "/skip"
	backWord = "/back"
)

// text is every string one language's tutorial needs.
//
// A struct rather than a map because a missing key in a map is a runtime empty
// string in front of a member, and a missing field here does not compile.
type text struct {
	// tag is the language this table is written in.
	tag string
	// name is what the language calls itself, for the button.
	name string

	// greeting promises how many questions are coming, which is not a constant: under
	// one agent per member there are four, and under one shared assistant there is
	// only the language question. The count is passed in rather than written into the
	// sentence so that the promise and the step list cannot drift apart — see
	// questionCount. It arrives already spelled in this language, because a table
	// cannot reach into itself from inside its own literal.
	//
	// The rest of the sentence is written to hold for one question as well as four.
	// That is not fussiness: the two counts the step list can produce are 1 and 4, and
	// a greeting that says "One quick questions" is the first thing a member ever
	// reads.
	greeting func(member, questions string) string
	// questionsPhrase is the quantified noun this language puts in that slot — the
	// whole phrase and not the numeral, because "one question" and "four questions"
	// do not differ only in the number in either language this tutorial is written
	// in. Only the sizes the step list can produce need an entry; questionCountPhrase
	// falls back to digits, which is wrong-looking rather than blank.
	questionsPhrase map[int]string

	languageQ       string
	languageOther   string
	otherPrompt     string
	otherNoted      func(named string) string
	skip            string
	back            string
	sameAsHousehold string
	// retired replaces the outcome line on a question nobody answered. The
	// transport's default says the question was declined or withdrawn; for a setup
	// question the true and more useful fact is which value it was left at.
	retired string
	// typedNotAnAnswer is sent to a member who writes something while a button
	// question is on screen. What they typed is dropped — correctly, since it is not
	// an answer to anything and buffering it would make it the answer to the next
	// question — and before this they got nothing at all, on a question that looks
	// answerable by typing.
	//
	// It is here rather than in the reader that drops the message because the reader
	// does not know what language this tutorial is in: the member may have chosen
	// Spanish at question one, and the household's language is a different setting.
	// See Tutorial.Nudge.
	typedNotAnAnswer string

	nameQ       string
	nameTooLong string
	nameKept    string
	nameSet     func(agent string) string

	registerQ       string
	registerFlat    string
	registerWarm    string
	registerPlayful string

	characterQ       string
	characterTooLong string
	// characterNoted acknowledges what the member wrote, as nameSet does for the
	// name. It says nothing back to them: the sentence is theirs and quoting it
	// would be a second copy of it on screen for no gain.
	characterNoted string

	abandoned string
}

// The memory model itself — the three messages Explanation sends — is deliberately
// not in this struct. It lives in internal/lang, in every language kenward speaks
// rather than the two this tutorial's questions are written in.
//
// The split is not tidiness. The questions are a convenience and can honestly say
// they are offered in two languages; the memory model is what the product is obliged
// to say about where a member's words go and who can read them, and a member who
// named Catalan should get that part in Catalan even though the four questions
// before it were in English.

// questionCountPhrase is how this language quantifies the questions in prose, falling
// back to digits for a count it has no phrase for. A digit in the middle of a sentence
// is ugly; a blank where a number should be is a broken promise, and that is the one
// this guards against.
func (t text) questionCountPhrase(n int) string {
	if w, ok := t.questionsPhrase[n]; ok {
		return w
	}
	return strconv.Itoa(n)
}

// tables is every language the tutorial is written in, keyed by tag.
var tables = map[string]text{
	LangEnglish: english,
	LangSpanish: spanish,
}

// textFor returns the table for a tag, falling back to English for anything this
// package does not hold. The fallback is never silent where a member can see it:
// askLanguage only offers tags that are in the table, and a member who names some
// other language is told in so many words that the rest of this runs in English.
func textFor(tag string) text {
	if t, ok := tables[tag]; ok {
		return t
	}
	return english
}

// Spoken reports whether the tutorial can be delivered end to end in this language.
// It is what a caller should consult before promising a member anything in it.
func Spoken(tag string) bool {
	_, ok := tables[tag]
	return ok
}

// TagFor maps a language named the way a person names one onto a tag this package
// holds copy for, falling back to English for anything it does not.
//
// It exists because the two halves of this setting are honestly different.
// config.PersonaConfig.Language is free text and has to be: it is passed to the model,
// not looked up in a table, and a household is entitled to ask for a register of a
// language kenward has never heard of. This package's copy is the opposite — a short,
// closed list of languages somebody has actually written and read — so somewhere the
// one has to be resolved against the other, and doing it here keeps the free-text
// promise intact everywhere else.
//
// ponytail: a switch, because there are two languages. A third makes it a field on
// text — the aliases each table answers to — rather than a longer switch here.
func TagFor(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "es", "spanish", "español", "espanol", "castellano":
		return LangSpanish
	default:
		return LangEnglish
	}
}

var english = text{
	tag:  LangEnglish,
	name: "English",

	questionsPhrase: map[int]string{1: "One quick question", 4: "Four quick questions"},
	greeting: func(member, questions string) string {
		return fmt.Sprintf("Hello %s. You're in.\n\n"+
			"%s to set me up for you, then I'll explain how I work. "+
			"Skip what you like and you get my defaults, and you can change all of it later.",
			transport.Esc(member), questions)
	},

	languageQ:     transport.Bold("Language") + "\n\nWhat language should I speak with you?",
	languageOther: "Another language",
	otherPrompt:   "Which language? Send me its name.",
	otherNoted: func(named string) string {
		return fmt.Sprintf("Noted: %s.\n\n"+
			"I have to be straight with you: these setup questions are only written in "+
			"English and Spanish so far, so the rest of them will be in English. What I "+
			"explain afterwards is in your language, and your choice is saved — you can "+
			"change it whenever you like.", transport.Esc(named))
	},
	skip:            "Skip",
	back:            "Back",
	sameAsHousehold: "Same as the household",
	retired:         "no answer — left on the default",
	typedNotAnAnswer: "This one is the buttons above — I can't read a typed answer to it, " +
		"so what you just sent hasn't gone anywhere.",

	nameQ: transport.Bold("My name") + "\n\nWhat would you like to call me? Send a name, " +
		"or " + transport.Code(skipWord) + " to leave me as kenward. " +
		transport.Code(backWord) + " goes back a question.",
	nameTooLong: "That's longer than I can wear. Forty characters or fewer, on one line.",
	nameKept:    "Staying as kenward, then.",
	nameSet: func(agent string) string {
		return fmt.Sprintf("%s it is.", transport.Esc(agent))
	},

	registerQ:       transport.Bold("How I talk") + "\n\nHow should I sound when I answer you?",
	registerFlat:    "Plain — short, no small talk",
	registerWarm:    "Warm — friendly, still brief",
	registerPlayful: "Playful — a bit of humour",

	characterQ: transport.Bold("Anything else") + "\n\nAnything else about how you'd like me to be? " +
		"A sentence is plenty — \"a bit dry, into cycling\". Send " + transport.Code(skipWord) +
		" if you'd rather not, or " + transport.Code(backWord) + " to go back.",
	characterTooLong: "A bit much for me to hold. Three hundred characters or fewer, please.",
	characterNoted:   "Noted. I'll keep that in mind.",

	abandoned: "No rush — I've left the rest on my defaults. You can change any of it later. " +
		"Here's how I work.",
}

var spanish = text{
	tag:  LangSpanish,
	name: "Español",

	questionsPhrase: map[int]string{1: "Una pregunta rápida", 4: "Cuatro preguntas rápidas"},
	greeting: func(member, questions string) string {
		return fmt.Sprintf("Hola %s. Ya estás dentro.\n\n"+
			"%s para ajustarme a ti y luego te explico cómo funciono. "+
			"Puedes omitir lo que quieras y te quedas con mis valores por defecto; todo esto "+
			"se puede cambiar más adelante.",
			transport.Esc(member), questions)
	},

	languageQ:     transport.Bold("Idioma") + "\n\n¿En qué idioma quieres que hable contigo?",
	languageOther: "Otro idioma",
	otherPrompt:   "¿Cuál? Escríbeme su nombre.",
	otherNoted: func(named string) string {
		return fmt.Sprintf("Anotado: %s.\n\n"+
			"Te lo digo claro: estas preguntas de configuración solo están escritas en "+
			"inglés y español por ahora, así que el resto irán en inglés. Lo que te explico "+
			"después sí va en tu idioma, y tu elección queda guardada: puedes cambiarla "+
			"cuando quieras.", transport.Esc(named))
	},
	skip:            "Saltar",
	back:            "Atrás",
	sameAsHousehold: "Igual que la casa",
	retired:         "sin respuesta — se queda como estaba",
	typedNotAnAnswer: "Esta se contesta con los botones de arriba: no puedo leer una " +
		"respuesta escrita, así que lo que acabas de enviar no ha ido a ninguna parte.",

	nameQ: transport.Bold("Mi nombre") + "\n\n¿Cómo quieres llamarme? Escríbeme un nombre, " +
		"o " + transport.Code(skipWord) + " para dejarme como kenward. " +
		transport.Code(backWord) + " vuelve a la pregunta anterior.",
	nameTooLong: "Ese nombre me queda largo. Cuarenta caracteres o menos, en una sola línea.",
	nameKept:    "Me quedo como kenward, entonces.",
	nameSet: func(agent string) string {
		return fmt.Sprintf("%s, pues.", transport.Esc(agent))
	},

	registerQ:       transport.Bold("Cómo hablo") + "\n\n¿Cómo quieres que suene cuando te conteste?",
	registerFlat:    "Directo — breve, sin rodeos",
	registerWarm:    "Cercano — amable, igual de breve",
	registerPlayful: "Con humor — un poco de guasa",

	characterQ: transport.Bold("Algo más") + "\n\n¿Algo más sobre cómo quieres que sea? " +
		"Con una frase basta: «un poco seco, le va el ciclismo». Escribe " +
		transport.Code(skipWord) + " si prefieres dejarlo, o " + transport.Code(backWord) +
		" para volver atrás.",
	characterTooLong: "Eso es más de lo que puedo sostener. Trescientos caracteres o menos, por favor.",
	characterNoted:   "Anotado. Lo tendré en cuenta.",

	abandoned: "Sin prisa: he dejado el resto con mis valores por defecto y puedes cambiarlo " +
		"cuando quieras. Te cuento cómo funciono.",
}
