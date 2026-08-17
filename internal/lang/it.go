package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Italian. Informal tu. The node speaks in the first person, masculine singular where
// the auxiliary essere forces agreement (non sono riuscito); kenward is a masculine
// given name, so this is natural Italian rather than a masculine default imposed on a
// neutral thing. A gender-free voice would mean turning every sono riuscito into non
// è stato possibile, which is noticeably stiffer — flagged rather than pre-empted.
//
// No French-style spacing: Italian puts no space before ? : ; !
//
// household = casa · tier = livello · node = nodo · entry = voce · memory store = la
// memoria, with no separate noun.

var itWeekdays = [7]string{"domenica", "lunedì", "martedì", "mercoledì", "giovedì", "venerdì", "sabato"}

var itMonths = [13]string{"", "gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno",
	"luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"}

// itAnd is the d eufonica, applied under current Accademia della Crusca guidance:
// ed only before a word beginning with e or E. Before any other vowel modern usage
// keeps e, and a digit or a symbol takes e.
func itAnd(next string) string {
	r := []rune(strings.TrimSpace(next))
	if len(r) > 0 && (r[0] == 'e' || r[0] == 'E') {
		return " ed "
	}
	return " e "
}

var italian = Catalogue{
	Tag:         Italian,
	Name:        "Italiano",
	EnglishName: "Italian",

	Locked:        "Il tuo assistente è bloccato. Deve essere sbloccato sulla macchina su cui gira.",
	ContentFilter: "Il modello ha rifiutato di rispondere al tuo messaggio.",
	Queued:        "Sto ancora lavorando al tuo messaggio precedente — questo è in coda e lo prendo subito dopo.",
	Dropped:       "Sono sovraccarico e ho dovuto scartare quel messaggio. Rimandalo tra un momento.",
	NoAnswer:      "Non ho ottenuto una risposta utilizzabile. Prova a chiedere di nuovo.",
	ToolMisfire:   "Ho provato a fare qualcosa e ho sbagliato, quindi non è successo niente. Chiedimelo di nuovo.",
	ResetNotice:   "Ricominciamo da capo — ho cancellato la parte precedente di questa conversazione. Nella tua memoria non è cambiato nulla; è il reset programmato.",

	ModelBusy:         "Il modello è occupato in questo momento. Riprova tra un momento.",
	Misconfigured:     "C'è qualcosa che non va nella configurazione di questa casa — dillo a chi la gestisce.",
	TurnFailed:        "Qualcosa è andato storto nel raggiungere il modello, e il tuo messaggio è rimasto senza risposta. Riprova tra un momento.",
	ReasoningOnly:     "Il modello ha passato tutto il tempo a ragionare senza arrivare a una risposta. Non è rotto niente — riprova a chiedere, oppure dividi la domanda in parti più piccole.",
	RefusalEmptyChain: "Nessuna macchina è configurata per rispondere in questa conversazione. Chiedi a chi gestisce questo nodo di configurarne una.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Nessuna macchina in %s (%s) è raggiungibile in questo momento. %s Questa conversazione è limitata a %s, quindi non la manderò da nessun'altra parte. Svegliane una e riprova a chiedere.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "i tuoi livelli consentiti",
	WhoseGroup:  "i livelli consentiti di casa",
	TierWord: func(n int) string {
		if n == 1 {
			return "quel livello"
		}
		return "quei livelli"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// disponibile is invariable in gender and machine names are invariable, so
	// these differ by number only. No comma before e: a, b e c.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Nessuno di essi aveva un indirizzo raggiungibile."
		case 1:
			return items[0] + " non era disponibile."
		default:
			return naturalJoin(items, ", ", itAnd(names[len(names)-1])) + " non erano disponibili."
		}
	},

	// The preposition is in the parts and not the prefix, and here that is
	// mandatory: in + la contracts to nella and in + il to nel, so the contraction
	// depends on which part follows.
	Searched:    func(parts []string) string { return "ricerca " + naturalJoin(parts, ", ", " e ") },
	PartPrivate: func(count string) string { return "nella tua memoria privata " + count },
	PartShared:  func(count string) string { return "nella memoria di casa " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(non leggibile)"
		case n == 0:
			return "(niente)"
		case n == 1:
			return "(1 voce)"
		default:
			return fmt.Sprintf("(%d voci)", n)
		}
	},

	RemindFull:    "[hai già tutti i promemoria che posso tenere — annullane prima uno]",
	RemindPast:    "[quell'orario è già passato, quindi non ho impostato niente]",
	RemindFailed:  "[non sono riuscito a impostare quel promemoria]",
	UnremindNone:  "[non c'è nessun promemoria con quel codice]",
	UnremindFails: "[non sono riuscito ad annullare quel promemoria]",
	ReminderSet: func(when, text, id string) string {
		return "[promemoria impostato, " + when + ": " + transport.Esc(text) + " — codice " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[promemoria annullato: " + transport.Esc(text) + "]"
	},
	// alle {time} is correct for every 24-hour numeric reading. The irregular
	// all'una and a mezzogiorno only arise if times are ever spelled out in words.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " alle " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "ogni giorno" + at
		case remind.EveryWeekly:
			return "ogni " + itWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			var day string
			switch d.Day() {
			case 1:
				day = "il 1º"
			case 8, 11:
				day = fmt.Sprintf("l'%d", d.Day())
			default:
				day = fmt.Sprintf("il %d", d.Day())
			}
			return day + " " + itMonths[d.Month()] + at
		}
	},

	SaveFailed: "Non sono riuscito a salvare quella voce — non è stato scritto niente.",
	AskFailed: func(title string) string {
		return "Volevo chiederti se salvare " + transport.Bold(title) + ", ma la domanda non è arrivata. Non è stato scritto niente."
	},
	// All of these are avere plus a following direct object, so nothing agrees with
	// the unknown gender of {title}. WrongSpace and PublishWrongSpace were
	// rewritten from the English passive into the active for the same reason —
	// non è stato salvato would have had to agree.
	Saved: func(private bool, title string) string {
		if private {
			return "Ho salvato " + transport.Bold(title) + " nella tua memoria privata."
		}
		return "Ho salvato " + transport.Bold(title) + " nella memoria di casa."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "nella memoria di casa"
		if private {
			where = "nella tua memoria privata"
		}
		return "Ho salvato " + transport.Bold(title) + " " + where + ", ma il pulsante Rimuovi non è arrivato, quindi da qui non posso più tornare indietro."
	},
	Removed: func(private bool, title string) string {
		where := "dalla memoria di casa"
		if private {
			where = "dalla tua memoria privata"
		}
		return "Ho rimosso " + transport.Bold(title) + " " + where + ". Non tornerà più in una risposta, né qui né su nessun altro dispositivo di casa."
	},
	UndoFailed: func(private bool, title string) string {
		where := "nella memoria di casa"
		if private {
			where = "nella tua memoria privata"
		}
		return "Non sono riuscito a tornare indietro: " + transport.Bold(title) + " è ancora " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "nella memoria di casa"
		if private {
			where = "nella tua memoria privata"
		}
		return "Non sono riuscito a salvare " + transport.Bold(title) + " " + where + " — la memoria ha rifiutato la scrittura, quindi non è stato salvato niente."
	},
	WrongSpace: func(title string) string {
		return "Qualcosa è andato storto: non ho salvato " + transport.Bold(title) + " dove sarebbe dovuto andare. Dillo a chi gestisce questo nodo prima di salvarlo di nuovo."
	},
	PublishNoShared:   "Non sono riuscito a pubblicare quella voce — non è stato pubblicato niente.",
	PublishUnreadable: "Non sono riuscito a leggere quella voce, quindi non è stato pubblicato niente.",
	PublishAskFailed: func(title string) string {
		return "Volevo chiederti se pubblicare " + transport.Bold(title) + ", ma la domanda non è arrivata. Non è stato pubblicato niente."
	},
	PublishRefused: func(title string) string {
		return "Non sono riuscito a pubblicare " + transport.Bold(title) + " — la memoria ha rifiutato la copia, quindi nella memoria di casa non è arrivato niente."
	},
	PublishWrongSpace: func(title string) string {
		return "Qualcosa è andato storto: non ho pubblicato " + transport.Bold(title) + " dove avevi scelto tu. Dillo a chi gestisce questo nodo."
	},
	Published: func(title string) string {
		return "Ho pubblicato " + transport.Bold(title) + " nella memoria di casa. Adesso è visibile a tutti in casa."
	},

	OnlyOneProposal: "Chiedo di una cosa per volta; non è stato salvato nient'altro di quel messaggio.",

	ProposalOpener: "Posso annotare questo:",
	ProposalNoDest: "Dove lo salvo?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "Lo salvo nella tua memoria privata?"
		}
		return "Lo salvo nella memoria di casa?"
	},
	UndoExpiredNote: "il tempo per tornare indietro è scaduto; questo resta nella tua memoria privata",
	WrittenOpener: func(private bool) string {
		if private {
			return "Ho scritto questo nella tua memoria privata:"
		}
		return "Ho scritto questo nella memoria di casa:"
	},
	WrittenHint:     "Il pulsante Rimuovi lo cancella.",
	PromotionOpener: "Questo verrebbe pubblicato nella memoria di casa esattamente com'è, e non potrà più essere ritirato:",
	PromotionCloser: "Pubblicare?",
	AlsoKnownAs:     func(words []string) string { return "Anche: " + latinList(words) },
	EnglishGloss:    func(summary string) string { return "Il testo qui sopra viene salvato in inglese. Dice: " + summary },

	// Rimuovi, not Annulla. Italian uses Annulla for both Undo and Cancel and both
	// buttons exist here. Rimuovi is also the truthful word — the button deletes
	// the entry, which is what Removed then reports — and it leaves BtnCancel on
	// the standard Annulla.
	BtnUndo:             "Rimuovi",
	BtnPublishHousehold: "Pubblica in casa",
	BtnCancel:           "Annulla",
	BtnSavePersonal:     "Salva in privato",
	BtnDontSave:         "Non salvare",
	BtnPersonal:         "Privato",
	BtnHousehold:        "Casa",
	BtnSaveHousehold:    "Salva in casa",

	Dash:      "— ",
	Declined:  "nessuna risposta, considerato rifiutato",
	Withdrawn: "domanda ritirata",

	EnrolPrivateHeading: "Questa chat è privata",
	EnrolPrivateBody: "Questa chat — solo io e te — è la tua memoria privata. Quello che mi dici qui resta nel tuo spazio. " +
		"Nessun altro in casa può leggerlo, e io non lo tirerò fuori nel gruppo.",
	EnrolGroupHeading: "La chat di gruppo è condivisa",
	EnrolGroupBody: "La chat di gruppo di casa è la memoria condivisa. Tutto quello che ricordo lì lo possono vedere tutti. " +
		"Niente passa da una parte all'altra da solo: se qualcosa di privato deve diventare condiviso, chiedimelo, " +
		"e ti mostrerò il testo esatto prima che si muova qualsiasi cosa.",
	EnrolMemoryHeading: "Cosa succede quando annoto qualcosa",
	EnrolMemoryBodyDefault: "Quando qualcosa mi sembra da tenere nella tua memoria personale, lo scrivo e poi ti mostro " +
		"esattamente cosa ho scritto e in quale memoria è finito, con un pulsante Rimuovi che lo cancella. Per la memoria " +
		"condivisa di casa invece chiedo prima e non scrivo niente finché non tocchi Salva in casa.\n\n" +
		"In entrambi i casi lo vedi sempre. È tutto qui. Parlami normalmente.",
	EnrolMemoryBodyAsk: "Non salvo mai niente da solo. Quando qualcosa mi sembra da tenere te lo chiedo — vedrai cosa scriverei " +
		"e in quale memoria andrebbe, e scegli una memoria oppure tocchi Non salvare. Se non rispondi, non salvo niente.\n\n" +
		"È tutto qui. Parlami normalmente.",

	Notice: identity,
}
