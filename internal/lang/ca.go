package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Catalan. Informal tu, plain and calm.
//
// "household" is la llar throughout: casa reads as the building, and llar is the
// household as a unit of people, which is what kenward means.
//
// WhoseDirect and WhoseGroup carry the preposition already contracted — de + els
// becomes dels — which is not a preference but the only grammatical shape.

var caWeekdays = [7]string{"diumenge", "dilluns", "dimarts", "dimecres", "dijous", "divendres", "dissabte"}

var caMonths = [13]string{"", "gener", "febrer", "març", "abril", "maig", "juny",
	"juliol", "agost", "setembre", "octubre", "novembre", "desembre"}

// caElides reports whether de must become d' before this month name. Catalan elides
// before a vowel or a silent h; of the twelve months only abril, agost and octubre
// qualify, but the rule is written out rather than the list, because a rule survives
// somebody editing the table.
func caElides(month string) bool {
	r := []rune(month)
	if len(r) == 0 {
		return false
	}
	return strings.ContainsRune("aàeèéiíoòóuúh", r[0])
}

var catalan = Catalogue{
	Tag:         Catalan,
	Name:        "Català",
	EnglishName: "Catalan",

	Locked:        "El teu assistent està bloquejat. Cal desbloquejar-lo a la màquina on s'executa.",
	ContentFilter: "El model s'ha negat a respondre el teu missatge.",
	Queued:        "Encara estic amb el teu missatge anterior: aquest queda a la cua i l'atendré tot seguit.",
	Dropped:       "Vaig saturat i he hagut de descartar aquest missatge. Torna a enviar-lo d'aquí a un moment.",
	NoAnswer:      "No n'he obtingut cap resposta aprofitable. Prova de preguntar-ho una altra vegada.",
	ResetNotice:   "Comencem de nou: he esborrat la part anterior d'aquesta conversa. A la teva memòria no ha canviat res; és el reinici programat.",

	ModelBusy:         "El model està ocupat ara mateix. Torna-ho a provar d'aquí a un moment.",
	Misconfigured:     "Hi ha alguna cosa malament a la configuració d'aquesta llar; digues-ho a qui l'administra.",
	TurnFailed:        "Alguna cosa ha fallat en contactar amb el model i el teu missatge s'ha quedat sense resposta. Torna-ho a provar d'aquí a un moment.",
	ReasoningOnly:     "El model s'ha passat tota l'estona pensant i no ha arribat a cap resposta. No hi ha res espatllat: prova de preguntar-ho una altra vegada, o per parts.",
	RefusalEmptyChain: "Cap màquina no està configurada per respondre aquesta conversa. Demana a qui administra aquest node que en configuri una.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Ara mateix no puc accedir a cap màquina %s (%s). %s Aquesta conversa està limitada a %s, així que no l'enviaré enlloc més. Desperta'n una i torna a preguntar.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "dels teus nivells permesos",
	WhoseGroup:  "dels nivells permesos de la llar",
	TierWord: func(n int) string {
		if n == 1 {
			return "aquest nivell"
		}
		return "aquests nivells"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// Catalan i is invariable: no euphonic alternation, unlike Spanish y/e. The
	// per-language function is still needed because the conjunction itself differs.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Cap d'ells no tenia una adreça accessible."
		case 1:
			return items[0] + " no estava disponible."
		default:
			return naturalJoin(items, ", ", " i ") + " no estaven disponibles."
		}
	},

	Searched:    func(parts []string) string { return "he cercat " + naturalJoin(parts, ", ", " i ") },
	PartPrivate: func(count string) string { return "a la teva memòria privada " + count },
	PartShared:  func(count string) string { return "a la memòria de la llar " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(no s'ha pogut llegir)"
		case n == 0:
			return "(res)"
		case n == 1:
			return "(1 entrada)"
		default:
			return fmt.Sprintf("(%d entrades)", n)
		}
	},

	RemindFull:    "[ja tens tots els recordatoris que puc guardar; cancel·la'n un primer]",
	RemindPast:    "[aquesta hora ja ha passat, així que no he programat res]",
	RemindFailed:  "[no he pogut programar aquest recordatori]",
	UnremindNone:  "[no hi ha cap recordatori amb aquest codi]",
	UnremindFails: "[no he pogut cancel·lar aquest recordatori]",
	ReminderSet: func(when, text, id string) string {
		return "[recordatori programat, " + when + ": " + transport.Esc(text) + " — codi " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[recordatori cancel·lat: " + transport.Esc(text) + "]"
	},
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " a les " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "cada dia" + at
		case remind.EveryWeekly:
			return "cada " + caWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			month := caMonths[d.Month()]
			article, prep := "el ", "de "
			if d.Day() == 1 {
				article = "l'"
			}
			if caElides(month) {
				prep = "d'"
			}
			return fmt.Sprintf("%s%d %s%s%s", article, d.Day(), prep, month, at)
		}
	},

	SaveFailed: "No he pogut desar aquesta entrada; no s'ha escrit res.",
	AskFailed: func(title string) string {
		return "Et volia preguntar si desava " + transport.Bold(title) + ", però la pregunta no ha arribat. No s'ha escrit res."
	},
	Saved: func(private bool, title string) string {
		if private {
			return "He desat " + transport.Bold(title) + " a la teva memòria privada."
		}
		return "He desat " + transport.Bold(title) + " a la memòria de la llar."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "a la memòria de la llar"
		if private {
			where = "a la teva memòria privada"
		}
		return "He desat " + transport.Bold(title) + " " + where + ", però el botó de Desfer no ha arribat, així que des d'aquí no ho puc desfer."
	},
	Removed: func(private bool, title string) string {
		where := "de la memòria de la llar"
		if private {
			where = "de la teva memòria privada"
		}
		return "He tret " + transport.Bold(title) + " " + where + ". No tornarà a sortir en cap resposta, ni aquí ni en cap altre dispositiu de la llar."
	},
	UndoFailed: func(private bool, title string) string {
		where := "a la memòria de la llar"
		if private {
			where = "a la teva memòria privada"
		}
		return "No ho he pogut desfer: " + transport.Bold(title) + " continua " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "a la memòria de la llar"
		if private {
			where = "a la teva memòria privada"
		}
		return "No he pogut desar " + transport.Bold(title) + " " + where + ": el magatzem de memòria ha rebutjat l'escriptura, així que no s'ha desat res."
	},
	WrongSpace: func(title string) string {
		return "Alguna cosa ha anat malament: no he desat " + transport.Bold(title) + " on tocava. Digues-ho a qui administra aquest node abans de tornar-ho a desar."
	},
	PublishNoShared:   "No he pogut publicar aquesta entrada; no s'ha publicat res.",
	PublishUnreadable: "No he pogut llegir aquesta entrada, així que no s'ha publicat res.",
	PublishAskFailed: func(title string) string {
		return "Et volia preguntar si publicava " + transport.Bold(title) + ", però la pregunta no ha arribat. No s'ha publicat res."
	},
	PublishRefused: func(title string) string {
		return "No he pogut publicar " + transport.Bold(title) + ": el magatzem de memòria ha rebutjat la còpia, així que no ha arribat res a la memòria de la llar."
	},
	PublishWrongSpace: func(title string) string {
		return "Alguna cosa ha anat malament: no he publicat " + transport.Bold(title) + " on havies triat. Digues-ho a qui administra aquest node."
	},
	Published: func(title string) string {
		return "He publicat " + transport.Bold(title) + " a la memòria de la llar. Ara ho pot veure tothom."
	},

	ProposalOpener: "Puc apuntar això:",
	ProposalNoDest: "On ho deso?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "Ho deso a la teva memòria privada?"
		}
		return "Ho deso a la memòria de la llar?"
	},
	UndoExpiredNote: "el termini per desfer-ho s'ha acabat; això continua a la teva memòria privada",
	WrittenOpener: func(private bool) string {
		if private {
			return "He escrit això a la teva memòria privada:"
		}
		return "He escrit això a la memòria de la llar:"
	},
	WrittenHint:     "El botó Desfer ho retira.",
	PromotionOpener: "Això es publicaria a la llar exactament tal com està, i no es pot despublicar:",
	PromotionCloser: "Ho publico?",

	BtnUndo:             "Desfer",
	BtnPublishHousehold: "Publicar a la llar",
	BtnCancel:           "Cancel·lar",
	BtnSavePersonal:     "Desar a personal",
	BtnDontSave:         "No desar",
	BtnPersonal:         "Personal",
	BtnHousehold:        "Llar",
	BtnSaveHousehold:    "Desar a la llar",

	Dash:      "— ",
	Declined:  "sense resposta, es considera rebutjat",
	Withdrawn: "pregunta retirada",

	EnrolPrivateHeading: "Aquest xat és privat",
	EnrolPrivateBody: "Aquest xat —només tu i jo— és la teva memòria privada. El que m'expliquis aquí es queda al teu espai. " +
		"Ningú més de la llar no ho pot llegir, i jo no ho trauré al grup.",
	EnrolGroupHeading: "El xat de grup és compartit",
	EnrolGroupBody: "El xat de grup de la llar és la memòria compartida. Tot el que hi recordi ho pot veure tothom. " +
		"No passa res d'un costat a l'altre tot sol: si alguna cosa privada ha de passar a compartida, demana-m'ho " +
		"i et mostraré el text exacte abans que es mogui res.",
	EnrolMemoryHeading: "Què passa quan apunto alguna cosa",
	EnrolMemoryBodyDefault: "Quan alguna cosa sembla que val la pena guardar a la teva pròpia memòria, l'escric i després " +
		"et mostro exactament què he escrit i a quina memòria ha anat, amb un botó de Desfer que ho retira. El que sigui " +
		"per a la memòria compartida de la llar t'ho pregunto abans i no escric res fins que premis Desar a la llar.\n\n" +
		"D'una manera o d'una altra sempre ho veus. Això és tot. Parla'm amb normalitat.",
	EnrolMemoryBodyAsk: "No deso mai res pel meu compte. Quan alguna cosa sembli que val la pena guardar, t'ho preguntaré: " +
		"veuràs què escriuria i a quina memòria va, i tries una memòria o prems No desar. Si no contestes, no ho deso.\n\n" +
		"Això és tot. Parla'm amb normalitat.",

	Notice: identity,
}
