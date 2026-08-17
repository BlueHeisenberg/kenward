package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Spanish. Informal tú, plain and calm, no exclamation marks.
//
// Three structural departures from the English, all forced:
//
//   - The destination is written into every sentence rather than slotted. Spanish
//     contracts the article after a preposition and the verb governs a different one
//     per template.
//   - WhoseDirect and WhoseGroup carry de. It does not contract before tus or los, so
//     Spanish alone would have survived the English shape; folding it in keeps the
//     template identical to Catalan and Portuguese, where it must be folded.
//   - The retrieval parts carry en and the prefix has lost it, for the same reason.

var esWeekdays = [7]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}

var esMonths = [13]string{"", "enero", "febrero", "marzo", "abril", "mayo", "junio",
	"julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// esAnd is the euphonic alternation: y becomes e before a word beginning with the
// sound /i/ — initial i- or hi- — but not before hie- or hia-, where the h is
// followed by a diphthong and the word begins with a consonant sound. Machine names
// are chosen by the household, so "pi e igloo" is reachable.
func esAnd(next string) string {
	r := []rune(strings.ToLower(strings.TrimSpace(next)))
	switch {
	case len(r) == 0:
		return " y "
	case r[0] == 'i' || r[0] == 'í':
		return " e "
	case r[0] == 'h' && len(r) > 1 && (r[1] == 'i' || r[1] == 'í'):
		if len(r) > 2 && (r[2] == 'e' || r[2] == 'a') {
			return " y "
		}
		return " e "
	}
	return " y "
}

var spanish = Catalogue{
	Tag:         Spanish,
	Name:        "Español",
	EnglishName: "Spanish",

	Locked:        "Tu asistente está bloqueado. Hay que desbloquearlo en la máquina donde se ejecuta.",
	ContentFilter: "El modelo se ha negado a responder a tu mensaje.",
	Queued:        "Sigo con tu mensaje anterior: este queda en cola y lo atenderé a continuación.",
	Dropped:       "Voy saturado y he tenido que descartar ese mensaje. Vuelve a enviarlo en un momento.",
	NoAnswer:      "No he obtenido una respuesta utilizable. Prueba a preguntarlo otra vez.",
	ToolMisfire:   "Intenté hacer algo con eso y me equivoqué, así que no ha pasado nada. Vuelve a pedírmelo.",
	NothingSaved:  "No he guardado nada ahora mismo. Dímelo otra vez si quieres que lo recuerde.",
	ResetNotice:   "Empezamos de nuevo: he borrado la parte anterior de esta conversación. En tu memoria no ha cambiado nada; es el reinicio programado.",

	BareAcknowledgements: []string{
		"hecho", "ya está", "listo", "todo listo", "entendido", "anotado",
		"apuntado", "guardado", "de acuerdo", "vale", "ok", "okay", "sin problema",
	},

	ModelBusy:         "El modelo está ocupado ahora mismo. Inténtalo de nuevo en un momento.",
	Misconfigured:     "Algo va mal en la configuración de este hogar; díselo a quien lo administra.",
	TurnFailed:        "Algo ha fallado al contactar con el modelo y tu mensaje se ha quedado sin respuesta. Inténtalo de nuevo en un momento.",
	ReasoningOnly:     "El modelo se ha pasado todo el rato pensando y no ha llegado a una respuesta. No hay nada roto: prueba a preguntarlo otra vez, o por partes.",
	RefusalEmptyChain: "Ninguna máquina está configurada para responder a esta conversación. Pide a quien administra este nodo que configure una.",

	// The English joins {tried} with an em dash; Spanish would then have to
	// capitalise after a dash, which is wrong. A full stop instead, and the three
	// fragments read as three sentences.
	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Ahora mismo no puedo acceder a ninguna máquina %s (%s). %s Esta conversación está limitada a %s, así que no la enviaré a ningún otro sitio. Despierta una y vuelve a preguntar.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "de tus niveles permitidos",
	WhoseGroup:  "de los niveles permitidos del hogar",
	TierWord: func(n int) string {
		if n == 1 {
			return "ese nivel"
		}
		return "esos niveles"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Ninguno de ellos tenía una dirección accesible."
		case 1:
			return items[0] + " no estaba disponible."
		default:
			return naturalJoin(items, ", ", esAnd(names[len(names)-1])) + " no estaban disponibles."
		}
	},

	Searched:    func(parts []string) string { return "he buscado " + naturalJoin(parts, ", ", " y ") },
	PartPrivate: func(count string) string { return "en tu memoria privada " + count },
	PartShared:  func(count string) string { return "en la memoria del hogar " + count },
	// Two forms: n == 1 is countOne and every other integer, zero included, is the
	// plural — "0 entradas" is correct Spanish. countZero survives as a lexical
	// override rather than as a plural category.
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(no se ha podido leer)"
		case n == 0:
			return "(nada)"
		case n == 1:
			return "(1 entrada)"
		default:
			return fmt.Sprintf("(%d entradas)", n)
		}
	},

	RemindFull:    "[ya tienes todos los recordatorios que puedo guardar; cancela uno primero]",
	RemindPast:    "[esa hora ya ha pasado, así que no he programado nada]",
	RemindFailed:  "[no he podido programar ese recordatorio]",
	UnremindNone:  "[no hay ningún recordatorio con ese código]",
	UnremindFails: "[no he podido cancelar ese recordatorio]",
	ReminderSet: func(when, text, id string) string {
		return "[recordatorio programado, " + when + ": " + transport.Esc(text) + " — código " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[recordatorio cancelado: " + transport.Esc(text) + "]"
	},
	// "cada {weekday}", not "todos los {weekday}": the plural form would need a
	// second, pluralised weekday table (sábado → sábados). cada takes the singular.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " a las " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "todos los días" + at
		case remind.EveryWeekly:
			return "cada " + esWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return fmt.Sprintf("el %d de %s%s", d.Day(), esMonths[d.Month()], at)
		}
	},

	SaveFailed: "No he podido guardar esa entrada; no se ha escrito nada.",
	AskFailed: func(title string) string {
		return "Quería preguntarte si guardaba " + transport.Bold(title) + ", pero la pregunta no ha llegado. No se ha escrito nada."
	},
	Saved: func(private bool, title string) string {
		if private {
			return "He guardado " + transport.Bold(title) + " en tu memoria privada."
		}
		return "He guardado " + transport.Bold(title) + " en la memoria del hogar."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "en la memoria del hogar"
		if private {
			where = "en tu memoria privada"
		}
		return "He guardado " + transport.Bold(title) + " " + where + ", pero el botón de Deshacer no ha llegado, así que desde aquí no puedo deshacerlo."
	},
	Removed: func(private bool, title string) string {
		where := "de la memoria del hogar"
		if private {
			where = "de tu memoria privada"
		}
		return "He quitado " + transport.Bold(title) + " " + where + ". No volverá a aparecer en ninguna respuesta, ni aquí ni en ningún otro dispositivo del hogar."
	},
	UndoFailed: func(private bool, title string) string {
		where := "en la memoria del hogar"
		if private {
			where = "en tu memoria privada"
		}
		return "No he podido deshacerlo: " + transport.Bold(title) + " sigue " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "en la memoria del hogar"
		if private {
			where = "en tu memoria privada"
		}
		return "No he podido guardar " + transport.Bold(title) + " " + where + ": el almacén de memoria ha rechazado la escritura, así que no se ha guardado nada."
	},
	// Nothing here agrees with {title}, whose grammatical gender is unknowable.
	// Do not "improve" these into agreeing forms.
	WrongSpace: func(title string) string {
		return "Algo ha ido mal: no he guardado " + transport.Bold(title) + " donde debía. Díselo a quien administra este nodo antes de volver a guardarlo."
	},
	PublishNoShared:   "No he podido publicar esa entrada; no se ha publicado nada.",
	PublishUnreadable: "No he podido leer esa entrada, así que no se ha publicado nada.",
	PublishAskFailed: func(title string) string {
		return "Quería preguntarte si publicaba " + transport.Bold(title) + ", pero la pregunta no ha llegado. No se ha publicado nada."
	},
	PublishRefused: func(title string) string {
		return "No he podido publicar " + transport.Bold(title) + ": el almacén de memoria ha rechazado la copia, así que no ha llegado nada a la memoria del hogar."
	},
	PublishWrongSpace: func(title string) string {
		return "Algo ha ido mal: no he publicado " + transport.Bold(title) + " donde elegiste. Díselo a quien administra este nodo."
	},
	Published: func(title string) string {
		return "He publicado " + transport.Bold(title) + " en la memoria del hogar. Ahora puede verlo todo el mundo."
	},

	OnlyOneProposal: "Solo pregunto por una cosa cada vez; no se guardó nada más de ese mensaje.",

	ProposalOpener: "Puedo anotar esto:",
	ProposalNoDest: "¿Dónde lo guardo?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "¿Lo guardo en tu memoria privada?"
		}
		return "¿Lo guardo en la memoria del hogar?"
	},
	UndoExpiredNote: "el plazo para deshacer ha terminado; esto sigue en tu memoria privada",
	WrittenOpener: func(private bool) string {
		if private {
			return "He escrito esto en tu memoria privada:"
		}
		return "He escrito esto en la memoria del hogar:"
	},
	WrittenHint:     "El botón Deshacer lo retira.",
	PromotionOpener: "Esto se publicaría en el hogar tal y como está, y no se puede despublicar:",
	PromotionCloser: "¿Lo publico?",
	AlsoKnownAs:     func(words []string) string { return "También: " + latinList(words) },
	EnglishGloss:    func(summary string) string { return "El texto de arriba se guarda en inglés. Dice: " + summary },

	BtnUndo:             "Deshacer",
	BtnPublishHousehold: "Publicar en el hogar",
	BtnCancel:           "Cancelar",
	BtnSavePersonal:     "Guardar en personal",
	BtnDontSave:         "No guardar",
	BtnPersonal:         "Personal",
	BtnHousehold:        "Hogar",
	BtnSaveHousehold:    "Guardar en el hogar",

	Dash:      "— ",
	Declined:  "sin respuesta, se considera rechazado",
	Withdrawn: "pregunta retirada",

	EnrolPrivateHeading: "Este chat es privado",
	EnrolPrivateBody: "Este chat —solo tú y yo— es tu memoria privada. Lo que me cuentes aquí se queda en tu espacio. " +
		"Nadie más del hogar puede leerlo, y no lo sacaré en el grupo.",
	EnrolGroupHeading: "El chat de grupo es compartido",
	EnrolGroupBody: "El chat de grupo del hogar es la memoria compartida. Todo lo que recuerde ahí lo puede ver todo el mundo. " +
		"Nada pasa de un lado a otro por sí solo: si algo privado tiene que hacerse compartido, pídemelo y te enseñaré " +
		"el texto exacto antes de que se mueva nada.",
	EnrolMemoryHeading: "Qué pasa cuando anoto algo",
	// The button names in this prose are byte-identical to BtnUndo, BtnDontSave and
	// BtnSaveHousehold. If a label changes, this changes with it.
	EnrolMemoryBodyDefault: "Cuando algo parece que vale la pena guardar en tu propia memoria, lo escribo y después te enseño " +
		"exactamente qué he escrito y en qué memoria ha quedado, con un botón de Deshacer que lo retira. Lo que sea " +
		"para la memoria compartida del hogar te lo pregunto antes y no escribo nada hasta que pulses Guardar en el hogar.\n\n" +
		"De una manera o de otra siempre lo ves. Eso es todo. Háblame con normalidad.",
	EnrolMemoryBodyAsk: "Nunca guardo nada por mi cuenta. Cuando algo parezca que vale la pena guardar, te lo preguntaré: " +
		"verás qué escribiría y en qué memoria va, y eliges una memoria o pulsas No guardar. Si no contestas, no lo guardo.\n\n" +
		"Eso es todo. Háblame con normalidad.",

	Notice: identity,
}
