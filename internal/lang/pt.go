package lang

import (
	"fmt"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// European Portuguese. Informal tu, post-1990 orthography, carregar em rather than
// clicar, conversa for a Telegram chat.
//
// "household" is a casa: agregado familiar is census jargon and lar is sentimental.
//
// Two contractions make the English shape impossible rather than merely awkward:
// de + os becomes dos in WhoseDirect and WhoseGroup, and em + a becomes na in the
// retrieval parts. Neither can be assembled from a preposition in the template and a
// bare noun phrase in the slot.
//
// Nothing in this table agrees a participle or a clitic with {title}, whose
// grammatical gender is unknowable. voltar atrás replaces "take it back", which
// would have needed retirá-lo or retirá-la. Keep it that way.

var ptWeekdays = [7]string{"domingo", "segunda-feira", "terça-feira", "quarta-feira",
	"quinta-feira", "sexta-feira", "sábado"}

var ptMonths = [13]string{"", "janeiro", "fevereiro", "março", "abril", "maio", "junho",
	"julho", "agosto", "setembro", "outubro", "novembro", "dezembro"}

var portuguese = Catalogue{
	Tag:         Portuguese,
	Name:        "Português",
	EnglishName: "Portuguese",

	Locked:        "O teu assistente está bloqueado. Tem de ser desbloqueado na máquina onde corre.",
	ContentFilter: "O modelo recusou-se a responder à tua mensagem.",
	Queued:        "Ainda estou com a tua mensagem anterior: esta fica em fila e trato dela a seguir.",
	Dropped:       "Estou sobrecarregado e tive de descartar essa mensagem. Volta a enviá-la daqui a pouco.",
	NoAnswer:      "Não obtive nenhuma resposta aproveitável. Tenta perguntar outra vez.",
	ToolMisfire:   "Tentei fazer algo com isso e enganei-me, por isso não aconteceu nada. Pede-me outra vez.",
	NothingSaved:  "Não guardei nada agora. Diz-me outra vez se queres que me lembre disso.",
	ResetNotice:   "Vamos começar de novo: apaguei a parte anterior desta conversa. Na tua memória não mudou nada; é o reinício programado.",

	BareAcknowledgements: []string{
		"feito", "está feito", "pronto", "tudo pronto", "entendido", "percebido",
		"anotado", "guardado", "combinado", "ok", "okay", "sem problema",
	},

	SaveClaims: []string{
		"guardado", "guardada", "anotado", "anotada", "registado", "registrado",
		"guardei", "anotei", "apontei", "tomei nota", "tomo nota", "fica anotado",
		"já tenho", "fica guardado",
		"adicionado à tua", "adicionado à memória", "está na tua memória",
	},

	SavePromises: []string{
		"vou lembrar", "vou-me lembrar", "não vou esquecer", "não me vou esquecer",
		"vou guardar", "vou anotar", "vou apontar", "não me esqueço",
	},

	SaveRequests: []string{
		"guarda", "guardar", "lembra", "lembrar", "não te esqueças",
		"não esqueças", "não me deixes esquecer", "anota", "aponta", "regista",
		"registra", "toma nota", "tomar nota", "escreve isto", "escreve isso",
		"memoriza", "para a próxima",
	},

	ModelBusy:         "O modelo está ocupado neste momento. Tenta outra vez daqui a pouco.",
	Misconfigured:     "Há alguma coisa mal na configuração desta casa; diz a quem a administra.",
	TurnFailed:        "Alguma coisa correu mal ao contactar o modelo e a tua mensagem ficou sem resposta. Tenta outra vez daqui a pouco.",
	ReasoningOnly:     "O modelo passou o tempo todo a pensar e não chegou a uma resposta. Não está nada avariado: tenta perguntar outra vez, ou por partes.",
	RefusalEmptyChain: "Nenhuma máquina está configurada para responder a esta conversa. Pede a quem administra este nó que configure uma.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Neste momento não consigo aceder a nenhuma máquina %s (%s). %s Esta conversa está limitada a %s, por isso não a envio para mais lado nenhum. Acorda uma delas e pergunta outra vez.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "dos teus níveis permitidos",
	WhoseGroup:  "dos níveis permitidos da casa",
	TierWord: func(n int) string {
		if n == 1 {
			return "esse nível"
		}
		return "esses níveis"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// Portuguese e is invariable, with no euphonic alternation.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Nenhum deles tinha um endereço acessível."
		case 1:
			return items[0] + " não estava disponível."
		default:
			return naturalJoin(items, ", ", " e ") + " não estavam disponíveis."
		}
	},

	Searched:    func(parts []string) string { return "procurei " + naturalJoin(parts, ", ", " e ") },
	PartPrivate: func(count string) string { return "na tua memória privada " + count },
	PartShared:  func(count string) string { return "na memória da casa " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(não foi possível ler)"
		case n == 0:
			return "(nada)"
		case n == 1:
			return "(1 entrada)"
		default:
			return fmt.Sprintf("(%d entradas)", n)
		}
	},

	RemindFull:    "[já tens todos os lembretes que consigo guardar; cancela um primeiro]",
	RemindPast:    "[essa hora já passou, por isso não marquei nada]",
	RemindFailed:  "[não consegui marcar esse lembrete]",
	UnremindNone:  "[não há nenhum lembrete com esse código]",
	UnremindFails: "[não consegui cancelar esse lembrete]",
	ReminderSet: func(when, text, id string) string {
		return "[lembrete marcado, " + when + ": " + transport.Esc(text) + " — código " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[lembrete cancelado: " + transport.Esc(text) + "]"
	},
	// "cada {weekday}", not "todos os {weekday}": the plural form needs both a
	// pluralised weekday and gender agreement on the article — todas as segundas
	// against todos os sábados. cada is gender-free and takes the singular table.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " às " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "todos os dias" + at
		case remind.EveryWeekly:
			return "cada " + ptWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return fmt.Sprintf("dia %d de %s%s", d.Day(), ptMonths[d.Month()], at)
		}
	},

	SaveFailed: "Não consegui guardar essa entrada; não foi escrito nada.",
	AskFailed: func(title string) string {
		return "Queria perguntar-te se guardava " + transport.Bold(title) + ", mas a pergunta não chegou. Não foi escrito nada."
	},
	Saved: func(private bool, title string) string {
		if private {
			return "Guardei " + transport.Bold(title) + " na tua memória privada."
		}
		return "Guardei " + transport.Bold(title) + " na memória da casa."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "na memória da casa"
		if private {
			where = "na tua memória privada"
		}
		return "Guardei " + transport.Bold(title) + " " + where + ", mas o botão Anular não chegou, por isso daqui não consigo voltar atrás."
	},
	Removed: func(private bool, title string) string {
		where := "da memória da casa"
		if private {
			where = "da tua memória privada"
		}
		return "Removi " + transport.Bold(title) + " " + where + ". Não volta a aparecer numa resposta, nem aqui nem em nenhum outro dispositivo da casa."
	},
	UndoFailed: func(private bool, title string) string {
		where := "na memória da casa"
		if private {
			where = "na tua memória privada"
		}
		return "Não consegui voltar atrás: " + transport.Bold(title) + " continua " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "na memória da casa"
		if private {
			where = "na tua memória privada"
		}
		return "Não consegui guardar " + transport.Bold(title) + " " + where + ": o repositório de memória recusou a escrita, por isso não foi guardado nada."
	},
	WrongSpace: func(title string) string {
		return "Alguma coisa correu mal: não guardei " + transport.Bold(title) + " onde devia. Diz a quem administra este nó antes de voltares a guardar."
	},
	PublishNoShared:   "Não consegui publicar essa entrada; não foi publicado nada.",
	PublishUnreadable: "Não consegui ler essa entrada, por isso não foi publicado nada.",
	PublishAskFailed: func(title string) string {
		return "Queria perguntar-te se publicava " + transport.Bold(title) + ", mas a pergunta não chegou. Não foi publicado nada."
	},
	PublishRefused: func(title string) string {
		return "Não consegui publicar " + transport.Bold(title) + ": o repositório de memória recusou a cópia, por isso não chegou nada à memória da casa."
	},
	PublishWrongSpace: func(title string) string {
		return "Alguma coisa correu mal: não publiquei " + transport.Bold(title) + " onde escolheste. Diz a quem administra este nó."
	},
	Published: func(title string) string {
		return "Publiquei " + transport.Bold(title) + " na memória da casa. Agora toda a gente pode ver."
	},

	OnlyOneProposal: "Só pergunto por uma coisa de cada vez; não foi guardado mais nada dessa mensagem.",

	ProposalOpener: "Posso apontar isto:",
	ProposalNoDest: "Onde guardo isto?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "Guardo isto na tua memória privada?"
		}
		return "Guardo isto na memória da casa?"
	},
	UndoExpiredNote: "o prazo para anular terminou; isto continua na tua memória privada",
	WrittenOpener: func(private bool) string {
		if private {
			return "Escrevi isto na tua memória privada:"
		}
		return "Escrevi isto na memória da casa:"
	},
	WrittenHint:     "O botão Anular retira isto.",
	PromotionOpener: "Isto seria publicado na casa exatamente como está, e não pode ser despublicado:",
	PromotionCloser: "Publico isto?",
	AlsoKnownAs:     func(words []string) string { return "Também: " + latinList(words) },
	EnglishGloss:    func(summary string) string { return "O texto acima fica guardado em inglês. Diz: " + summary },

	// Anular is the pt-PT platform convention for Undo, on both Microsoft and
	// Google. Desfazer is the pt-BR form and reads as an editor's undo rather than
	// "take that back".
	BtnUndo:             "Anular",
	BtnPublishHousehold: "Publicar na casa",
	BtnCancel:           "Cancelar",
	BtnSavePersonal:     "Guardar em pessoal",
	BtnDontSave:         "Não guardar",
	BtnPersonal:         "Pessoal",
	BtnHousehold:        "Casa",
	BtnSaveHousehold:    "Guardar na casa",

	Dash:      "— ",
	Declined:  "sem resposta, considera-se recusado",
	Withdrawn: "pergunta retirada",

	EnrolPrivateHeading: "Esta conversa é privada",
	EnrolPrivateBody: "Esta conversa — só tu e eu — é a tua memória privada. O que me contares aqui fica no teu espaço. " +
		"Mais ninguém na casa o pode ler, e eu não o trago para o grupo.",
	EnrolGroupHeading: "A conversa de grupo é partilhada",
	EnrolGroupBody: "A conversa de grupo da casa é a memória partilhada. Tudo o que eu guardar aí, toda a gente pode ver. " +
		"Nada passa de um lado para o outro sozinho: se alguma coisa privada tiver de passar a partilhada, pede-me, " +
		"e mostro-te o texto exato antes de alguma coisa se mexer.",
	EnrolMemoryHeading: "O que acontece quando aponto alguma coisa",
	EnrolMemoryBodyDefault: "Quando alguma coisa parece valer a pena guardar na tua própria memória, escrevo-a e depois " +
		"mostro-te exatamente o que escrevi e em que memória ficou, com um botão Anular que a retira. O que for para a " +
		"memória partilhada da casa pergunto primeiro e não escrevo nada até carregares em Guardar na casa.\n\n" +
		"De uma maneira ou de outra, vês sempre. É só isto. Fala comigo normalmente.",
	EnrolMemoryBodyAsk: "Nunca guardo nada por minha conta. Quando alguma coisa parecer valer a pena guardar, pergunto: " +
		"vais ver o que eu escreveria e em que memória vai ficar, e escolhes uma memória ou carregas em Não guardar. " +
		"Se não responderes, não guardo.\n\nÉ só isto. Fala comigo normalmente.",

	Notice: identity,
}
