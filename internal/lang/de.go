package lang

import (
	"fmt"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// German. Address form du throughout.
//
// Terminology, fixed once: your private memory = dein privates Gedächtnis (neuter,
// nominative equals accusative) · the household memory = das Haushaltsgedächtnis ·
// household = Haushalt · node = Knoten · tier = Stufe · memory store =
// Gedächtnisspeicher · Undo = Rückgängig · a reminder is gestellt and gelöscht, as
// with an alarm clock.
//
// The destination split is a case split and not a stylistic one, and the two halves
// are not even the same shape:
//
//	speichern  in + dative      in deinem privaten Gedächtnis / im Haushaltsgedächtnis
//	entfernen  aus + dative     aus deinem privaten Gedächtnis / aus dem Haushaltsgedächtnis
//	stehen     in + dative      in deinem privaten Gedächtnis / im Haushaltsgedächtnis
//	schreiben  in + accusative  in dein privates Gedächtnis / ins Haushaltsgedächtnis
//
// in + dem contracts to im and in + das to ins, and both contractions are obligatory
// in running prose. So the split falls between speichern and schreiben, not between
// "to" and "from".

var deWeekdays = [7]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}

var deMonths = [13]string{"", "Januar", "Februar", "März", "April", "Mai", "Juni",
	"Juli", "August", "September", "Oktober", "November", "Dezember"}

var german = Catalogue{
	Tag:         German,
	Name:        "Deutsch",
	EnglishName: "German",

	Locked:        "Dein Assistent ist gesperrt. Er muss auf dem Rechner entsperrt werden, auf dem er läuft.",
	ContentFilter: "Das Modell hat die Antwort auf deine Nachricht abgelehnt.",
	Queued:        "Ich arbeite noch an deiner letzten Nachricht — diese hier steht in der Warteschlange und ich nehme sie mir als Nächstes vor.",
	Dropped:       "Ich komme gerade nicht hinterher und musste diese Nachricht verwerfen. Schick sie gleich noch einmal.",
	NoAnswer:      "Ich habe darauf keine brauchbare Antwort bekommen. Frag es noch einmal.",
	ToolMisfire:   "Ich wollte etwas dafür tun und habe es falsch gemacht, also ist nichts passiert. Frag mich noch einmal.",
	NothingSaved:  "Ich habe gerade nichts gespeichert. Sag es noch einmal, wenn ich es mir merken soll.",
	ResetNotice:   "Neuer Anfang — ich habe den früheren Teil dieses Gesprächs gelöscht. An deinem Gedächtnis hat sich nichts geändert; das ist der planmäßige Reset.",

	BareAcknowledgements: []string{
		"erledigt", "fertig", "alles klar", "notiert", "gespeichert", "verstanden",
		"in ordnung", "okay", "ok", "mach ich", "geht klar", "kein problem",
	},

	ModelBusy:         "Das Modell ist gerade ausgelastet. Versuch es gleich noch einmal.",
	Misconfigured:     "Mit der Einrichtung dieses Haushalts stimmt etwas nicht — sag der Person Bescheid, die ihn betreibt.",
	TurnFailed:        "Beim Zugriff auf das Modell ist etwas schiefgegangen, und deine Nachricht wurde nicht beantwortet. Versuch es gleich noch einmal.",
	ReasoningOnly:     "Das Modell hat die ganze Zeit nachgedacht und ist zu keiner Antwort gekommen. Es ist nichts kaputt — frag noch einmal, oder in kleineren Teilen.",
	RefusalEmptyChain: "Es ist keine Maschine dafür eingerichtet, dieses Gespräch zu beantworten. Bitte die Person, die diesen Knoten betreibt, darum, eine einzurichten.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("Keine Maschine in %s (%s) ist gerade erreichbar. %s Dieses Gespräch ist auf %s beschränkt, deshalb schicke ich es nirgendwo anders hin. Weck eine davon auf und frag noch einmal.",
			whose, chain, tried, tierWord)
	},
	// After the preposition in, so dative. These appear in exactly one sentence; a
	// second sentence with a different preposition would need them split like the
	// capture entries.
	WhoseDirect: "deinen erlaubten Stufen",
	WhoseGroup:  "den erlaubten Stufen des Haushalts",
	// After beschränkt auf, so accusative — and diese Stufe / diese Stufen are
	// identical in nominative and accusative, which is what makes this slot stable.
	TierWord: func(n int) string {
		if n == 1 {
			return "diese Stufe"
		}
		return "diese Stufen"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "Keine davon hatte eine erreichbare Adresse."
		case 1:
			return items[0] + " war nicht erreichbar."
		default:
			return naturalJoin(items, ", ", " und ") + " waren nicht erreichbar."
		}
	},

	// The verb goes last, which is why the parts moved to the front — that is the
	// natural German order and the reason the placeholders are reorderable at all.
	// The parts are the accusative object of durchsuchen; both destination nouns
	// are neuter, so accusative and nominative are identical and this slot needs no
	// split.
	Searched:    func(parts []string) string { return naturalJoin(parts, ", ", " und ") + " durchsucht" },
	PartPrivate: func(count string) string { return "dein privates Gedächtnis " + count },
	PartShared:  func(count string) string { return "das Haushaltsgedächtnis " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(nicht lesbar)"
		case n == 0:
			return "(nichts)"
		case n == 1:
			return "(1 Eintrag)"
		default:
			return fmt.Sprintf("(%d Einträge)", n)
		}
	},

	RemindFull:    "[du hast schon so viele Erinnerungen, wie ich behalten kann — lösch zuerst eine]",
	RemindPast:    "[dieser Zeitpunkt ist schon vorbei, deshalb habe ich nichts gestellt]",
	RemindFailed:  "[ich konnte diese Erinnerung nicht stellen]",
	UnremindNone:  "[es gibt keine Erinnerung mit diesem Code]",
	UnremindFails: "[ich konnte diese Erinnerung nicht löschen]",
	ReminderSet: func(when, text, id string) string {
		return "[Erinnerung gestellt, " + when + ": " + transport.Esc(text) + " — Code " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[Erinnerung gelöscht: " + transport.Esc(text) + "]"
	},
	// jeden works for all seven weekdays because every German weekday is masculine.
	// That is luck rather than design: a language with mixed-gender weekdays in this
	// slot needs seven sentences and not one.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " um " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "jeden Tag" + at
		case remind.EveryWeekly:
			return "jeden " + deWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return fmt.Sprintf("%d. %s%s", d.Day(), deMonths[d.Month()], at)
		}
	},

	SaveFailed: "Ich konnte diesen Eintrag nicht speichern — es wurde nichts geschrieben.",
	AskFailed: func(title string) string {
		return "Ich wollte nachfragen, ob ich " + transport.Bold(title) + " speichern soll, aber die Frage ist nicht durchgekommen. Es wurde nichts geschrieben."
	},
	Saved: func(private bool, title string) string {
		if private {
			return "Ich habe " + transport.Bold(title) + " in deinem privaten Gedächtnis gespeichert."
		}
		return "Ich habe " + transport.Bold(title) + " im Haushaltsgedächtnis gespeichert."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "im Haushaltsgedächtnis"
		if private {
			where = "in deinem privaten Gedächtnis"
		}
		return "Ich habe " + transport.Bold(title) + " " + where + " gespeichert, aber der Rückgängig-Knopf ist nicht durchgekommen, deshalb kann ich es von hier aus nicht zurücknehmen."
	},
	Removed: func(private bool, title string) string {
		where := "aus dem Haushaltsgedächtnis"
		if private {
			where = "aus deinem privaten Gedächtnis"
		}
		return "Ich habe " + transport.Bold(title) + " " + where + " entfernt. Es taucht in keiner Antwort mehr auf, weder hier noch auf einem anderen Gerät im Haushalt."
	},
	UndoFailed: func(private bool, title string) string {
		where := "im Haushaltsgedächtnis"
		if private {
			where = "in deinem privaten Gedächtnis"
		}
		return "Ich konnte das nicht zurücknehmen: " + transport.Bold(title) + " steht immer noch " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "im Haushaltsgedächtnis"
		if private {
			where = "in deinem privaten Gedächtnis"
		}
		return "Ich konnte " + transport.Bold(title) + " nicht " + where + " speichern — der Gedächtnisspeicher hat den Schreibvorgang abgelehnt, es wurde also nichts gespeichert."
	},
	WrongSpace: func(title string) string {
		return "Da ist etwas schiefgegangen: Ich habe " + transport.Bold(title) + " nicht dort gespeichert, wo es hingehört hätte. Sag der Person Bescheid, die diesen Knoten betreibt, bevor du es noch einmal speicherst."
	},
	PublishNoShared:   "Ich konnte diesen Eintrag nicht veröffentlichen — es wurde nichts veröffentlicht.",
	PublishUnreadable: "Ich konnte diesen Eintrag nicht lesen, deshalb wurde nichts veröffentlicht.",
	PublishAskFailed: func(title string) string {
		return "Ich wollte nachfragen, ob ich " + transport.Bold(title) + " veröffentlichen soll, aber die Frage ist nicht durchgekommen. Es wurde nichts veröffentlicht."
	},
	PublishRefused: func(title string) string {
		return "Ich konnte " + transport.Bold(title) + " nicht veröffentlichen — der Gedächtnisspeicher hat die Kopie abgelehnt, es ist also nichts im Haushaltsgedächtnis angekommen."
	},
	PublishWrongSpace: func(title string) string {
		return "Da ist etwas schiefgegangen: Ich habe " + transport.Bold(title) + " nicht dort veröffentlicht, wo du es ausgewählt hast. Sag der Person Bescheid, die diesen Knoten betreibt."
	},
	Published: func(title string) string {
		return "Ich habe " + transport.Bold(title) + " im Haushaltsgedächtnis veröffentlicht. Jetzt können es alle im Haushalt sehen."
	},

	OnlyOneProposal: "Ich frage immer nur nach einer Sache; sonst wurde aus dieser Nachricht nichts gespeichert.",

	ProposalOpener: "Das kann ich aufschreiben:",
	ProposalNoDest: "Wo soll ich es speichern?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "In deinem privaten Gedächtnis speichern?"
		}
		return "Im Haushaltsgedächtnis speichern?"
	},
	UndoExpiredNote: "das Zeitfenster zum Rückgängigmachen ist abgelaufen; das hier steht weiterhin in deinem privaten Gedächtnis",
	// schreiben is directional, so accusative here — the one place the split is not
	// dative, and the reason a single slot could never have worked.
	WrittenOpener: func(private bool) string {
		if private {
			return "Ich habe das in dein privates Gedächtnis geschrieben:"
		}
		return "Ich habe das ins Haushaltsgedächtnis geschrieben:"
	},
	WrittenHint:     "Der Rückgängig-Knopf entfernt es wieder.",
	PromotionOpener: "Das würde genau in dieser Form im Haushalt veröffentlicht und kann nicht zurückgezogen werden:",
	PromotionCloser: "Veröffentlichen?",
	AlsoKnownAs:     func(words []string) string { return "Auch: " + latinList(words) },
	EnglishGloss: func(summary string) string {
		return "Der Text oben wird auf Englisch gespeichert. Er lautet: " + summary
	},

	BtnUndo:             "Rückgängig",
	BtnPublishHousehold: "Im Haushalt veröffentlichen",
	BtnCancel:           "Abbrechen",
	BtnSavePersonal:     "Persönlich speichern",
	BtnDontSave:         "Nicht speichern",
	BtnPersonal:         "Persönlich",
	BtnHousehold:        "Haushalt",
	BtnSaveHousehold:    "Im Haushalt speichern",

	Dash:      "— ",
	Declined:  "keine Antwort, gilt als abgelehnt",
	Withdrawn: "Frage zurückgezogen",

	EnrolPrivateHeading: "Dieser Chat ist privat",
	EnrolPrivateBody: "Dieser Chat — nur du und ich — ist dein privates Gedächtnis. Was du mir hier erzählst, bleibt in deinem Bereich. " +
		"Niemand sonst im Haushalt kann es lesen, und ich bringe es in der Gruppe nicht zur Sprache.",
	EnrolGroupHeading: "Der Gruppenchat ist gemeinsam",
	EnrolGroupBody: "Der Gruppenchat des Haushalts ist das gemeinsame Gedächtnis. Was ich mir dort merke, können alle sehen. " +
		"Nichts wechselt von allein hinüber: Wenn etwas Privates gemeinsam werden soll, sag es mir, und ich zeige dir " +
		"den genauen Wortlaut, bevor irgendetwas davon umzieht.",
	EnrolMemoryHeading: "Was passiert, wenn ich etwas aufschreibe",
	EnrolMemoryBodyDefault: "Wenn etwas es wert klingt, für dein eigenes Gedächtnis aufgehoben zu werden, schreibe ich es auf " +
		"und zeige dir dann genau, was ich geschrieben habe und in welches Gedächtnis es gegangen ist, mit einem " +
		"Rückgängig-Knopf, der es zurückholt. Bei allem, was ins gemeinsame Gedächtnis des Haushalts soll, frage ich " +
		"vorher nach und schreibe nichts, bevor du auf Im Haushalt speichern tippst.\n\n" +
		"So oder so siehst du es immer. Das ist alles. Rede einfach ganz normal mit mir.",
	EnrolMemoryBodyAsk: "Ich speichere nie etwas von allein. Wenn etwas es wert klingt, aufgehoben zu werden, frage ich nach — " +
		"du siehst, was ich aufschreiben würde und in welches Gedächtnis es geht, und du wählst ein Gedächtnis oder tippst " +
		"auf Nicht speichern. Wenn du nicht antwortest, speichere ich es nicht.\n\n" +
		"Das ist alles. Rede einfach ganz normal mit mir.",

	Notice: identity,
}
