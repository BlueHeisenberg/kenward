package lang

import (
	"fmt"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Dutch. Informal je and jij throughout, never u. Dutch has no participle gender
// agreement, so {title} carries no grammatical risk here; the risk in Dutch is word
// order, and every entry below has been checked against the three traps — V2
// inversion after a fronted element (Voor het gedeelde geheugen … vraag ik het
// eerst), verb-final order in subordinate clauses (voordat je het opnieuw opslaat),
// and separable prefixes landing at the end of their clause (sla ik het niet op,
// stuur ik het nergens anders heen). A literal rendering of the English breaks all
// three, and breaks them most often in the dash clauses, which is where the promises
// live.
//
// household = huishouden · household memory = het huishoudgeheugen · tier = niveau ·
// node = knooppunt · entry = item.

var nlWeekdays = [7]string{"zondag", "maandag", "dinsdag", "woensdag", "donderdag", "vrijdag", "zaterdag"}

var nlMonths = [13]string{"", "januari", "februari", "maart", "april", "mei", "juni",
	"juli", "augustus", "september", "oktober", "november", "december"}

var dutch = Catalogue{
	Tag:         Dutch,
	Name:        "Nederlands",
	EnglishName: "Dutch",

	Locked:        "Je assistent is vergrendeld. Hij moet ontgrendeld worden op de machine waarop hij draait.",
	ContentFilter: "Het model heeft geweigerd op je bericht te antwoorden.",
	Queued:        "Ik ben nog bezig met je vorige bericht — dit staat in de wachtrij en ik pak het daarna op.",
	Dropped:       "Ik heb een achterstand en moest dat bericht laten vallen. Stuur het over een moment opnieuw.",
	NoAnswer:      "Ik heb daar geen bruikbaar antwoord op gekregen. Probeer het opnieuw te vragen.",
	ToolMisfire:   "Ik probeerde er iets mee te doen en deed het verkeerd, dus er is niets gebeurd. Vraag het me nog eens.",
	NothingSaved:  "Ik heb net niets opgeslagen. Zeg het nog eens als je wilt dat ik het onthoud.",
	ResetNotice:   "We beginnen opnieuw — ik heb het eerdere deel van dit gesprek gewist. Er is niets veranderd in je geheugen; dit is de geplande reset.",

	BareAcknowledgements: []string{
		"gedaan", "klaar", "genoteerd", "opgeslagen", "begrepen", "duidelijk",
		"oké", "ok", "okay", "in orde", "komt goed", "prima", "geen probleem",
	},

	SaveClaims: []string{
		"opgeslagen", "genoteerd", "vastgelegd", "opgeschreven", "bewaard",
		"ik heb genoteerd", "ik heb opgeslagen", "ik heb opgeschreven",
		"ik noteer", "ik schrijf het op", "ik onthoud",
		"ik heb het",
		"toegevoegd aan je", "toegevoegd aan het geheugen", "staat nu in je",
	},

	SavePromises: []string{
		"ik zal onthouden", "ik zal het onthouden", "ik vergeet het niet",
		"ik vergeet dat niet", "ik zal het noteren", "ik zal het opslaan",
		"ik bewaar het",
	},

	SaveRequests: []string{
		"onthoud", "onthouden", "noteer", "noteren", "schrijf dit op",
		"schrijf dat op", "schrijf het op", "schrijf op", "bewaar",
		"sla dit op", "sla dat op", "sla op", "vergeet niet", "niet vergeten",
		"denk eraan", "leg vast", "vastleggen", "voor de volgende keer",
	},

	ModelBusy:         "Het model is op dit moment bezet. Probeer het over een moment opnieuw.",
	Misconfigured:     "Er klopt iets niet aan de instellingen van dit huishouden — zeg het tegen degene die het beheert.",
	TurnFailed:        "Er ging iets mis bij het bereiken van het model, en je bericht is niet beantwoord. Probeer het over een moment opnieuw.",
	ReasoningOnly:     "Het model heeft de hele tijd zitten nadenken en is niet tot een antwoord gekomen. Er is niets stuk — vraag het opnieuw, of in kleinere stukjes.",
	RefusalEmptyChain: "Er is geen machine ingesteld om dit gesprek te beantwoorden. Vraag degene die dit knooppunt beheert om er een in te stellen.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Geen enkele machine in %s (%s) is nu bereikbaar. %s Dit gesprek is beperkt tot %s, dus ik stuur het nergens anders heen. Maak er een wakker en vraag het opnieuw.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "jouw toegestane niveaus",
	WhoseGroup:  "de toegestane niveaus van het huishouden",
	TierWord: func(n int) string {
		if n == 1 {
			return "dat niveau"
		}
		return "die niveaus"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// No comma before en: a, b en c. en never changes form, and beschikbaar is a
	// predicative adjective that takes no inflection, so these differ only in
	// was / waren.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Geen ervan had een bereikbaar adres."
		case 1:
			return items[0] + " was niet beschikbaar."
		default:
			return naturalJoin(items, ", ", " en ") + " waren niet beschikbaar."
		}
	},

	// A bare past participle, matching the English's terseness. It also sidesteps
	// the word-order problem: a full clause (ik heb … gezocht) would want the
	// participle after both parts, which the concatenation cannot produce.
	Searched:    func(parts []string) string { return "gezocht " + naturalJoin(parts, ", ", " en ") },
	PartPrivate: func(count string) string { return "in je persoonlijke geheugen " + count },
	PartShared:  func(count string) string { return "in het huishoudgeheugen " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(niet leesbaar)"
		case n == 0:
			return "(niets)"
		case n == 1:
			return "(1 item)"
		default:
			return fmt.Sprintf("(%d items)", n)
		}
	},

	RemindFull:    "[je hebt al zoveel herinneringen als ik kan bewaren — annuleer er eerst een]",
	RemindPast:    "[dat tijdstip is al voorbij, dus ik heb niets ingesteld]",
	RemindFailed:  "[ik kon die herinnering niet instellen]",
	UnremindNone:  "[er is geen herinnering met die code]",
	UnremindFails: "[ik kon die herinnering niet annuleren]",
	ReminderSet: func(when, text, id string) string {
		return "[herinnering ingesteld, " + when + ": " + transport.Esc(text) + " — code " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[herinnering geannuleerd: " + transport.Esc(text) + "]"
	},
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " om " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "elke dag" + at
		case remind.EveryWeekly:
			return "elke " + nlWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return fmt.Sprintf("%d %s%s", d.Day(), nlMonths[d.Month()], at)
		}
	},

	SaveFailed: "Ik kon dat item niet opslaan — er is niets weggeschreven.",
	AskFailed: func(title string) string {
		return "Ik wilde vragen of ik " + transport.Bold(title) + " moest opslaan, maar de vraag kwam niet aan. Er is niets weggeschreven."
	},
	// The destination sits after the participle rather than before it. Both are
	// grammatical; extraposition keeps a long {title} from pushing the destination
	// away from the verb, and lands the part that is a promise at the end of the
	// clause where a Dutch reader expects the new information.
	Saved: func(private bool, title string) string {
		if private {
			return "Ik heb " + transport.Bold(title) + " opgeslagen in je persoonlijke geheugen."
		}
		return "Ik heb " + transport.Bold(title) + " opgeslagen in het huishoudgeheugen."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "in het huishoudgeheugen"
		if private {
			where = "in je persoonlijke geheugen"
		}
		return "Ik heb " + transport.Bold(title) + " opgeslagen " + where + ", maar de knop Ongedaan maken kwam niet aan, dus ik kan het hiervandaan niet meer terugnemen."
	},
	// Negative concord: "niet hier en niet op een ander apparaat". The English
	// "here or on any other device" sat under a negative and read ambiguously.
	Removed: func(private bool, title string) string {
		where := "uit het huishoudgeheugen"
		if private {
			where = "uit je persoonlijke geheugen"
		}
		return "Ik heb " + transport.Bold(title) + " verwijderd " + where + ". Het komt niet meer terug in een antwoord, niet hier en niet op een ander apparaat in het huishouden."
	},
	UndoFailed: func(private bool, title string) string {
		where := "in het huishoudgeheugen"
		if private {
			where = "in je persoonlijke geheugen"
		}
		return "Ik kon dat niet terugnemen: " + transport.Bold(title) + " staat nog " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "in het huishoudgeheugen"
		if private {
			where = "in je persoonlijke geheugen"
		}
		return "Ik kon " + transport.Bold(title) + " niet opslaan " + where + " — het geheugen weigerde de schrijfactie, dus er is niets bewaard."
	},
	WrongSpace: func(title string) string {
		return "Er is iets misgegaan: ik heb " + transport.Bold(title) + " niet opgeslagen waar het hoorde. Zeg het tegen degene die dit knooppunt beheert voordat je het opnieuw opslaat."
	},
	PublishNoShared:   "Ik kon dat item niet publiceren — er is niets gepubliceerd.",
	PublishUnreadable: "Ik kon dat item niet lezen, dus er is niets gepubliceerd.",
	PublishAskFailed: func(title string) string {
		return "Ik wilde vragen of ik " + transport.Bold(title) + " moest publiceren, maar de vraag kwam niet aan. Er is niets gepubliceerd."
	},
	PublishRefused: func(title string) string {
		return "Ik kon " + transport.Bold(title) + " niet publiceren — het geheugen weigerde de kopie, dus er is niets in het huishoudgeheugen terechtgekomen."
	},
	PublishWrongSpace: func(title string) string {
		return "Er is iets misgegaan: ik heb " + transport.Bold(title) + " niet gepubliceerd waar jij had gekozen. Zeg het tegen degene die dit knooppunt beheert."
	},
	Published: func(title string) string {
		return "Ik heb " + transport.Bold(title) + " gepubliceerd in het huishoudgeheugen. Iedereen in het huishouden kan het nu zien."
	},

	OnlyOneProposal: "Ik vraag maar naar één ding tegelijk; er is verder niets uit dat bericht opgeslagen.",

	ProposalOpener: "Dit kan ik opschrijven:",
	ProposalNoDest: "Waar moet ik het opslaan?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "Opslaan in je persoonlijke geheugen?"
		}
		return "Opslaan in het huishoudgeheugen?"
	},
	UndoExpiredNote: "de termijn om dit ongedaan te maken is verstreken; dit staat nog in je persoonlijke geheugen",
	WrittenOpener: func(private bool) string {
		if private {
			return "Dit heb ik weggeschreven naar je persoonlijke geheugen:"
		}
		return "Dit heb ik weggeschreven naar het huishoudgeheugen:"
	},
	WrittenHint: "De knop Ongedaan maken verwijdert het.",
	// verwijderd uit, never verwijderd in: Dutch changes the preposition with the
	// verb, which is why the destination is written out here rather than slotted.
	NotSaved: func(private bool) string {
		if private {
			return "Niet opgeslagen in je persoonlijke geheugen."
		}
		return "Niet opgeslagen in het huishoudgeheugen."
	},
	PromotionOpener: "Dit zou precies zo in het huishoudgeheugen gepubliceerd worden, en kan daarna niet meer ingetrokken worden:",
	PromotionCloser: "Publiceren?",
	AlsoKnownAs:     func(words []string) string { return "Ook: " + latinList(words) },
	EnglishGloss: func(summary string) string {
		return "De tekst hierboven wordt in het Engels bewaard. Er staat: " + summary
	},

	// Dutch has no Undo / Cancel collision, so both keep their standard labels.
	// Ongedaan maken is the longest label in the whole set and is the one to watch
	// in the three-button row and in the transport's outcome-line reservation.
	BtnUndo:             "Ongedaan maken",
	BtnPublishHousehold: "In huishouden publiceren",
	BtnCancel:           "Annuleren",
	BtnSavePersonal:     "Persoonlijk opslaan",
	BtnDontSave:         "Niet opslaan",
	BtnPersonal:         "Persoonlijk",
	BtnHousehold:        "Huishouden",
	BtnSaveHousehold:    "In huishouden opslaan",

	Dash:      "— ",
	Declined:  "geen antwoord, geldt als geweigerd",
	Withdrawn: "vraag ingetrokken",

	EnrolPrivateHeading: "Dit gesprek is privé",
	EnrolPrivateBody: "Dit gesprek — alleen jij en ik — is je persoonlijke geheugen. Wat je me hier vertelt wordt in je " +
		"eigen ruimte bewaard, en de groep van het huishouden kan het nooit lezen. Ik breng het daar ook niet ter " +
		"sprake.",
	EnrolPrivateSealed: "Dit huishouden draait in geïsoleerde modus: je assistent heeft een eigen proces en een eigen " +
		"sleutel. Niemand anders in het huishouden kan je persoonlijke geheugen lezen, en degene die deze computer " +
		"beheert ook niet. De eerlijke grens: wie root-toegang tot deze machine heeft, kan bij je sleutel komen " +
		"zolang je assistent draait.",
	EnrolGroupHeading: "De groepschat is gedeeld",
	EnrolGroupBody: "De groepschat van het huishouden is het gedeelde geheugen. Alles wat ik daar onthoud, kan iedereen zien. " +
		"Er gaat niets vanzelf van de ene kant naar de andere: als iets persoonlijks gedeeld moet worden, vraag het me, " +
		"dan laat ik je de exacte tekst zien voordat er iets verschuift.",
	EnrolMemoryHeading: "Wat er gebeurt als ik iets opschrijf",
	EnrolMemoryBodyDefault: "Als iets het waard lijkt om in je eigen geheugen te bewaren, schrijf ik het op en laat ik je daarna " +
		"precies zien wat ik heb geschreven en in welk geheugen het terecht is gekomen, met een knop Ongedaan maken die het " +
		"weer weghaalt: tik je erop, dan komt het niet meer terug in een antwoord, niet hier en niet op een ander apparaat " +
		"in het huishouden. Voor het gedeelde geheugen van het huishouden vraag ik het eerst en schrijf ik niets tot je op " +
		"In huishouden opslaan tikt.\n\nHoe dan ook zie je het altijd. Dat is alles. Praat gewoon normaal tegen me.",
	EnrolMemoryBodyAsk: "Ik sla nooit uit mezelf iets op. Als iets het waard lijkt om te bewaren, vraag ik het je — je ziet dan " +
		"wat ik zou opschrijven en in welk geheugen het terecht zou komen, en jij kiest een geheugen of tikt op Niet opslaan. " +
		"Als je niet antwoordt, sla ik het niet op.\n\nDat is alles. Praat gewoon normaal tegen me.",

	Notice: identity,
}
