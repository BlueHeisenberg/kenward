package lang

import (
	"fmt"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// French. Informal tu. The node speaks in the first person, masculine singular where
// an agreement is unavoidable (débordé) — kenward is a masculine given name, so this
// is natural rather than a default.
//
// Spacing: every ? : ; below is preceded by U+00A0 NO-BREAK SPACE, written as an
// escape so it is visible in a diff. U+202F NARROW NO-BREAK SPACE is the
// typographically preferred character and is the wrong choice here: it has patchy
// font coverage and renders as a missing-glyph box on some Android Telegram builds,
// which is a visible defect where a marginally wide space is not. TestFrenchSpacing
// asserts the rule holds.
//
// household = foyer · tier = niveau · node = nœud · entry = entrée · memory store =
// la mémoire, with no separate noun.

const nbsp = "\u00a0"

var frWeekdays = [7]string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}

var frMonths = [13]string{"", "janvier", "février", "mars", "avril", "mai", "juin",
	"juillet", "août", "septembre", "octobre", "novembre", "décembre"}

var french = Catalogue{
	Tag:         French,
	Name:        "Français",
	EnglishName: "French",

	Locked:        "Ton assistant est verrouillé. Il doit être déverrouillé sur la machine où il tourne.",
	ContentFilter: "Le modèle a refusé de répondre à ton message.",
	Queued:        "Je traite encore ton message précédent — celui-ci est en file d'attente, je m'en occupe juste après.",
	Dropped:       "Je suis débordé et j'ai dû abandonner ce message. Renvoie-le dans un instant.",
	NoAnswer:      "Je n'ai pas obtenu de réponse exploitable. Essaie de redemander.",
	ToolMisfire:   "J'ai essayé de faire quelque chose et je m'y suis mal pris, donc rien ne s'est passé. Redemande-le-moi.",
	NothingSaved:  "Je n'ai rien enregistré à l'instant. Redis-le-moi si tu veux que je m'en souvienne.",
	ResetNotice:   "On repart de zéro — j'ai effacé le début de cette conversation. Rien n'a changé dans ta mémoire" + nbsp + "; c'est la réinitialisation prévue.",

	BareAcknowledgements: []string{
		"c'est fait", "fait", "c'est noté", "noté", "entendu", "compris",
		"bien reçu", "d'accord", "enregistré", "ok", "okay", "pas de problème",
	},

	ModelBusy:         "Le modèle est occupé pour le moment. Réessaie dans un instant.",
	Misconfigured:     "Quelque chose ne va pas dans la configuration de ce foyer — préviens la personne qui s'en occupe.",
	TurnFailed:        "Un problème est survenu en contactant le modèle, et ton message est resté sans réponse. Réessaie dans un instant.",
	ReasoningOnly:     "Le modèle a passé tout son temps à réfléchir sans arriver à une réponse. Rien n'est cassé — redemande, ou découpe ta question en plus petits morceaux.",
	RefusalEmptyChain: "Aucune machine n'est configurée pour répondre dans cette conversation. Demande à la personne qui gère ce nœud d'en configurer une.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Aucune machine dans %s (%s) n'est joignable pour le moment. %s Cette conversation est limitée à %s, je ne l'enverrai donc nulle part ailleurs. Réveilles-en une et redemande.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "tes niveaux autorisés",
	WhoseGroup:  "les niveaux autorisés du foyer",
	TierWord: func(n int) string {
		if n == 1 {
			return "ce niveau"
		}
		return "ces niveaux"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// No Oxford comma: a, b et c. et never elides or changes form, machine names
	// are invariable and disponible has no gender inflection, so these vary by
	// number only.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Aucun d'eux n'avait d'adresse joignable."
		case 1:
			return items[0] + " n'était pas disponible."
		default:
			return naturalJoin(items, ", ", " et ") + " n'étaient pas disponibles."
		}
	},

	Searched:    func(parts []string) string { return "recherche " + naturalJoin(parts, ", ", " et ") },
	PartPrivate: func(count string) string { return "dans ta mémoire privée " + count },
	PartShared:  func(count string) string { return "dans la mémoire du foyer " + count },
	// Hardcoded 0 / 1 / many, and deliberately not delegated to a CLDR selector:
	// CLDR French puts zero in the one category, so a generic selector prints
	// "1 entrée" when nothing was found.
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(illisible)"
		case n == 0:
			return "(rien)"
		case n == 1:
			return "(1 entrée)"
		default:
			return fmt.Sprintf("(%d entrées)", n)
		}
	},

	RemindFull:    "[tu as déjà autant de rappels que je peux en garder — annules-en un d'abord]",
	RemindPast:    "[cette heure est déjà passée, je n'ai donc rien programmé]",
	RemindFailed:  "[je n'ai pas pu programmer ce rappel]",
	UnremindNone:  "[il n'y a aucun rappel avec ce code]",
	UnremindFails: "[je n'ai pas pu annuler ce rappel]",
	ReminderSet: func(when, text, id string) string {
		return "[rappel programmé, " + when + nbsp + ": " + transport.Esc(text) + " — code " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[rappel annulé" + nbsp + ": " + transport.Esc(text) + "]"
	},
	// chaque {weekday} rather than tous les {weekday}s: the French idiom for a
	// recurring weekday pluralises the noun, which would mean inflecting the
	// interpolated value. chaque takes the bare singular from the table.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " à " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "tous les jours" + at
		case remind.EveryWeekly:
			return "chaque " + frWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			day := fmt.Sprintf("%d", d.Day())
			if d.Day() == 1 {
				day = "1er"
			}
			return "le " + day + " " + frMonths[d.Month()] + at
		}
	},

	SaveFailed: "Je n'ai pas pu enregistrer cette entrée — rien n'a été écrit.",
	AskFailed: func(title string) string {
		return "Je voulais te demander si je devais enregistrer " + transport.Bold(title) + ", mais la question n'est pas passée. Rien n'a été écrit."
	},
	// Every split entry is avoir plus a following direct object, so no participle
	// agrees with the unknown gender of {title}. wrongSpace, publishWrongSpace and
	// published were rewritten from the English passive into the active for the
	// same reason.
	Saved: func(private bool, title string) string {
		if private {
			return "J'ai enregistré " + transport.Bold(title) + " dans ta mémoire privée."
		}
		return "J'ai enregistré " + transport.Bold(title) + " dans la mémoire du foyer."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "dans la mémoire du foyer"
		if private {
			where = "dans ta mémoire privée"
		}
		return "J'ai enregistré " + transport.Bold(title) + " " + where + ", mais le bouton Retirer n'est pas passé, je ne peux donc pas revenir en arrière d'ici."
	},
	Removed: func(private bool, title string) string {
		where := "de la mémoire du foyer"
		if private {
			where = "de ta mémoire privée"
		}
		return "J'ai retiré " + transport.Bold(title) + " " + where + ". Cela ne reviendra plus dans une réponse, ni ici ni sur aucun autre appareil du foyer."
	},
	UndoFailed: func(private bool, title string) string {
		where := "dans la mémoire du foyer"
		if private {
			where = "dans ta mémoire privée"
		}
		return "Je n'ai pas pu revenir en arrière" + nbsp + ": " + transport.Bold(title) + " est toujours " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "dans la mémoire du foyer"
		if private {
			where = "dans ta mémoire privée"
		}
		return "Je n'ai pas pu enregistrer " + transport.Bold(title) + " " + where + " — la mémoire a refusé l'écriture, rien n'a donc été stocké."
	},
	WrongSpace: func(title string) string {
		return "Un problème est survenu" + nbsp + ": je n'ai pas enregistré " + transport.Bold(title) + " à l'endroit prévu. Préviens la personne qui gère ce nœud avant de l'enregistrer à nouveau."
	},
	PublishNoShared:   "Je n'ai pas pu publier cette entrée — rien n'a été publié.",
	PublishUnreadable: "Je n'ai pas pu lire cette entrée, rien n'a donc été publié.",
	PublishAskFailed: func(title string) string {
		return "Je voulais te demander si je devais publier " + transport.Bold(title) + ", mais la question n'est pas passée. Rien n'a été publié."
	},
	PublishRefused: func(title string) string {
		return "Je n'ai pas pu publier " + transport.Bold(title) + " — la mémoire a refusé la copie, rien n'est donc arrivé dans la mémoire du foyer."
	},
	PublishWrongSpace: func(title string) string {
		return "Un problème est survenu" + nbsp + ": je n'ai pas publié " + transport.Bold(title) + " là où tu l'avais choisi. Préviens la personne qui gère ce nœud."
	},
	Published: func(title string) string {
		return "J'ai publié " + transport.Bold(title) + " dans la mémoire du foyer. Tout le foyer y a accès maintenant."
	},

	OnlyOneProposal: "Je ne pose la question que pour une chose à la fois" + nbsp + "; rien d'autre de ce message n'a été enregistré.",

	ProposalOpener: "Je peux noter ceci" + nbsp + ":",
	ProposalNoDest: "Où faut-il l'enregistrer" + nbsp + "?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "L'enregistrer dans ta mémoire privée" + nbsp + "?"
		}
		return "L'enregistrer dans la mémoire du foyer" + nbsp + "?"
	},
	UndoExpiredNote: "le délai pour revenir en arrière est écoulé" + nbsp + "; ceci reste dans ta mémoire privée",
	WrittenOpener: func(private bool) string {
		if private {
			return "J'ai écrit ceci dans ta mémoire privée" + nbsp + ":"
		}
		return "J'ai écrit ceci dans la mémoire du foyer" + nbsp + ":"
	},
	WrittenHint:     "Le bouton Retirer l'efface de la mémoire.",
	PromotionOpener: "Ceci serait publié dans la mémoire du foyer exactement tel quel, et ne pourra plus être retiré" + nbsp + ":",
	PromotionCloser: "Publier" + nbsp + "?",
	AlsoKnownAs:     func(words []string) string { return "Aussi" + nbsp + ": " + latinList(words) },
	EnglishGloss: func(summary string) string {
		return "Le texte ci-dessus est conservé en anglais. Il dit" + nbsp + ": " + summary
	},

	// Retirer, not Annuler. French uses Annuler for both Undo and Cancel and both
	// buttons exist in this product. Retirer is also the truthful word: the button
	// deletes the entry, which is exactly what Removed then reports. Every prose
	// reference to the button says Retirer to match.
	BtnUndo:             "Retirer",
	BtnPublishHousehold: "Publier au foyer",
	BtnCancel:           "Annuler",
	BtnSavePersonal:     "Enregistrer en privé",
	BtnDontSave:         "Ne pas enregistrer",
	BtnPersonal:         "Privé",
	BtnHousehold:        "Foyer",
	BtnSaveHousehold:    "Enregistrer pour le foyer",

	Dash:      "— ",
	Declined:  "pas de réponse, considéré comme refusé",
	Withdrawn: "question retirée",

	EnrolPrivateHeading: "Cette conversation est privée",
	EnrolPrivateBody: "Cette conversation — juste toi et moi — est ta mémoire privée. Ce que tu me dis ici reste dans ton espace. " +
		"Personne d'autre dans le foyer ne peut le lire, et je n'en parlerai pas dans le groupe.",
	EnrolGroupHeading: "La conversation de groupe est partagée",
	EnrolGroupBody: "La conversation de groupe du foyer est la mémoire partagée. Tout ce que j'y retiens est visible par tout le monde. " +
		"Rien ne passe d'un côté à l'autre tout seul" + nbsp + ": si quelque chose de privé doit devenir partagé, demande-le-moi, " +
		"et je te montrerai le texte exact avant que quoi que ce soit ne bouge.",
	EnrolMemoryHeading: "Ce qui se passe quand je note quelque chose",
	EnrolMemoryBodyDefault: "Quand quelque chose me paraît mériter d'être gardé dans ta propre mémoire, je l'écris, puis je te montre " +
		"exactement ce que j'ai écrit et dans quelle mémoire c'est allé, avec un bouton Retirer qui l'efface. Pour la mémoire " +
		"partagée du foyer, je demande d'abord et je n'écris rien tant que tu n'as pas appuyé sur Enregistrer pour le foyer.\n\n" +
		"Dans les deux cas, tu le vois toujours. C'est tout. Parle-moi normalement.",
	EnrolMemoryBodyAsk: "Je n'enregistre jamais rien de moi-même. Quand quelque chose me paraît mériter d'être gardé, je te le demande — " +
		"tu verras ce que j'écrirais et dans quelle mémoire ça irait, et tu choisis une mémoire ou tu appuies sur Ne pas enregistrer. " +
		"Si tu ne réponds pas, je n'enregistre rien.\n\nC'est tout. Parle-moi normalement.",

	Notice: identity,
}
