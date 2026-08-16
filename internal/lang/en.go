package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// english is the reviewed source every other table was written from, and the one
// every golden file asserts. Where a translation and this disagree, this is right.
var english = Catalogue{
	Tag:         English,
	Name:        "English",
	EnglishName: "English",

	Locked: "Your assistant is locked. It needs to be unlocked on the machine it runs on.",
	// "that message" rather than "this": the thing declined is the member's
	// message, and Italian cannot say "answer this" without a gendered clitic or an
	// invented noun. A concrete noun translates everywhere.
	ContentFilter: "The model declined to answer your message.",
	Queued:        "Still working on your last message — this one is queued and I'll take it next.",
	Dropped:       "I'm backed up and had to drop that message. Send it again in a moment.",
	NoAnswer:      "I didn't get a usable answer to that. Try asking again.",
	ResetNotice:   "Starting fresh — I've cleared the earlier part of this conversation. Nothing in your memory changed; this is the scheduled reset.",

	ModelBusy:     "The model is busy right now. Try again in a moment.",
	Misconfigured: "Something is wrong with this household's setup — tell whoever runs it.",
	TurnFailed:    "Something went wrong reaching the model, and your message wasn't answered. Try again in a moment.",
	// "the whole time", not "the whole turn". A turn is an LLM word with no lay
	// equivalent — German has no good noun for it and Chinese needs 整轮 — and it
	// buys the member nothing here.
	ReasoningOnly: "The model spent the whole time thinking and didn't get to an answer. Nothing is broken — try asking again, or in smaller pieces.",
	// "tier chain is empty" was jargon in every language, and the clause did no
	// work for its reader: the sentence already tells them who to ask.
	RefusalEmptyChain: "No machine is set up to answer this conversation. Ask whoever runs this node to configure one.",

	// tried is its own sentence and ends in a full stop. It used to be spliced
	// into the middle of this one after an em dash, which produced "…right now —
	// Node-B was unavailable. This conversation is limited to…" — a complete
	// sentence inside another with no punctuation contract. The space after it
	// belongs to this template rather than to a shared format string, because
	// Chinese ends the fragment with 。and needs no space at all.
	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("No machine in %s (%s) is reachable right now. %s This conversation is limited to %s, so I won't send it anywhere else. Wake one of them and ask again.",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "your allowed tiers",
	WhoseGroup:  "the household's allowed tiers",
	TierWord: func(n int) string {
		if n == 1 {
			return "that tier"
		}
		return "those tiers"
	},
	Chain: func(names []string) string { return codeJoin(names, ", ") },
	// "Unavailable" is chosen over "I tried": Tried lists endpoints that were
	// attempted and endpoints skipped for cooldown or a failed probe, and claiming
	// an attempt that never happened is a small untruth in a message whose whole
	// value is being accurate.
	//
	// The zero case used to read "I found no endpoints to try", which contradicted
	// the clause it was spliced into — it named two tiers and then said it had
	// found nothing to try — and said "endpoints" where every neighbouring
	// sentence says "machine". A family does not have endpoints.
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "None of them had a reachable address."
		case 1:
			return items[0] + " was unavailable."
		default:
			return naturalJoin(items, ", ", " and ") + " were unavailable."
		}
	},

	Searched:    func(parts []string) string { return "searched " + strings.Join(parts, ", ") },
	PartPrivate: func(count string) string { return "your private memory " + count },
	PartShared:  func(count string) string { return "the household memory " + count },
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "(couldn't be read)"
		case n == 0:
			return "(nothing)"
		case n == 1:
			return "(1 entry)"
		default:
			return fmt.Sprintf("(%d entries)", n)
		}
	},

	RemindFull:    "[you already have as many reminders as I can keep — cancel one first]",
	RemindPast:    "[that time has already gone, so I have not set anything]",
	RemindFailed:  "[I could not set that reminder]",
	UnremindNone:  "[there is no reminder with that code]",
	UnremindFails: "[I could not cancel that reminder]",
	ReminderSet: func(when, text, id string) string {
		return "[reminder set, " + when + ": " + transport.Esc(text) + " — code " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[reminder cancelled: " + transport.Esc(text) + "]"
	},
	When: func(r remind.Reminder, loc *time.Location) string {
		switch r.Every {
		case remind.EveryDaily:
			return "every day at " + clock(r)
		case remind.EveryWeekly:
			return "every " + r.Weekday.String() + " at " + clock(r)
		default:
			return r.Next.In(loc).Format("Monday 2 January") + " at " + clock(r)
		}
	},

	// "that entry", not "that": Italian needs a gendered clitic or an invented noun
	// to say "I couldn't save that" at all, and English loses nothing by naming the
	// thing it failed to save.
	SaveFailed: "I couldn't save that entry — nothing was written.",
	// "saving", not "remembering". Remember is doing two jobs in the English —
	// here it means record, and Spanish recordar and Portuguese lembrar mean recall
	// first. Save is sharper in English and cleaner in every Romance language.
	AskFailed: func(title string) string {
		return "I meant to ask about saving " + transport.Bold(title) + ", but the question didn't go through. Nothing was written."
	},
	// Full sentences, not verbless fragments. "Saved X to …" and "Published X …"
	// sat beside "I couldn't save …" and read as log lines rather than as somebody
	// talking; German renders the fragment form as a machine's output.
	Saved: func(private bool, title string) string {
		if private {
			return "I've saved " + transport.Bold(title) + " to your private memory."
		}
		return "I've saved " + transport.Bold(title) + " to the household memory."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "the household memory"
		if private {
			where = "your private memory"
		}
		return "I've saved " + transport.Bold(title) + " to " + where + ", but the undo button didn't go through, so I can't take it back from here."
	},
	// "not here and not on any other device": the old "here or on any other
	// device" sat under a negative and read ambiguously. Dutch negative concord
	// forced the clearer form, which is clearer English too.
	Removed: func(private bool, title string) string {
		where := "the household memory"
		if private {
			where = "your private memory"
		}
		return "I've removed " + transport.Bold(title) + " from " + where + ". It won't come back in an answer, not here and not on any other device in the household."
	},
	UndoFailed: func(private bool, title string) string {
		where := "the household memory"
		if private {
			where = "your private memory"
		}
		return "I couldn't take that back: " + transport.Bold(title) + " is still in " + where + "."
	},
	// Names the destination. It was the only "I couldn't write" message that did
	// not say where the write was headed, and it is the one where the member most
	// wants to know.
	StoreRefused: func(private bool, title string) string {
		where := "the household memory"
		if private {
			where = "your private memory"
		}
		return "I couldn't save " + transport.Bold(title) + " to " + where + " — the memory store refused the write, so nothing was stored."
	},
	// Active, not passive. "{title} was not stored where it should have been"
	// makes the participle agree with a value of unknown grammatical gender in
	// French and Italian; there is nothing to agree with.
	WrongSpace: func(title string) string {
		return "Something went wrong: I didn't store " + transport.Bold(title) + " where it should have gone. Tell whoever runs this node before saving it again."
	},
	PublishNoShared:   "I couldn't publish that entry — nothing was published.",
	PublishUnreadable: "I couldn't read that entry, so nothing was published.",
	PublishAskFailed: func(title string) string {
		return "I meant to ask about publishing " + transport.Bold(title) + ", but the question didn't go through. Nothing was published."
	},
	PublishRefused: func(title string) string {
		return "I couldn't publish " + transport.Bold(title) + " — the memory store refused the copy, so nothing reached the household memory."
	},
	PublishWrongSpace: func(title string) string {
		return "Something went wrong: I didn't publish " + transport.Bold(title) + " where you chose. Tell whoever runs this node."
	},
	Published: func(title string) string {
		return "I've published " + transport.Bold(title) + " to the household memory. Everyone in the household can see it now."
	},

	OnlyOneProposal: "I only ask about one thing at a time; nothing else from that message was saved.",

	// "write down", not "remember", for the same reason as AskFailed. It is also
	// sharper about what actually happens: a specific entry, in a specific space.
	ProposalOpener: "I can write this down:",
	ProposalNoDest: "Where should I save it?",
	ProposalWithDest: func(private bool) string {
		if private {
			return "Save it to your private memory?"
		}
		return "Save it to the household memory?"
	},
	// Says which memory, in a product whose whole premise is that there are two.
	// This note only ever rides on a private write announcement.
	UndoExpiredNote: "the undo window has closed; this is still in your private memory",
	WrittenOpener: func(private bool) string {
		if private {
			return "I've written this to your private memory:"
		}
		return "I've written this to the household memory:"
	},
	// "The Undo button removes it", not "Undo removes it": the short form depends
	// on the button label being a bare verb, which it barely is in Catalan and
	// Portuguese and is not at all in Arabic.
	WrittenHint:     "The Undo button removes it.",
	PromotionOpener: "This would be published to the household exactly as it stands, and cannot be unpublished:",
	PromotionCloser: "Publish it?",
	AlsoKnownAs:     func(words []string) string { return "Also known as: " + latinList(words) },

	BtnUndo:             "Undo",
	BtnPublishHousehold: "Publish to household",
	BtnCancel:           "Cancel",
	BtnSavePersonal:     "Save to personal",
	BtnDontSave:         "Don't save",
	BtnPersonal:         "Personal",
	BtnHousehold:        "Household",
	BtnSaveHousehold:    "Save to household",

	Dash:      "— ",
	Declined:  "no answer, treated as declined",
	Withdrawn: "question withdrawn",

	EnrolPrivateHeading: "This chat is private",
	EnrolPrivateBody: "This chat — just you and me — is your private memory. What you tell me here stays in your space. " +
		"Nobody else in the household can read it, and I won't bring it up in the group.",
	EnrolGroupHeading: "The group chat is shared",
	EnrolGroupBody: "The household group chat is the shared memory. Whatever I remember there, everyone can see. " +
		"Nothing crosses over on its own: if something private should become shared, ask me, and I'll show you " +
		"the exact text before any of it moves.",
	// "write something down" rather than "remember something", matching
	// ProposalOpener.
	EnrolMemoryHeading: "What happens when I write something down",
	// Names buttons that exist. This used to say "until you tap Save", and there is
	// no Save button: the labels are Save to personal and Save to household. The
	// onboarding was teaching a label the member would never see.
	EnrolMemoryBodyDefault: "When something sounds worth keeping for your own memory, I write it down and then show you " +
		"exactly what I wrote and which memory it went to, with an Undo button that takes it back. Anything for " +
		"the household's shared memory I ask about first and write nothing until you tap Save to household.\n\n" +
		"Either way you always see it. That's all of it. Just talk to me normally.",
	EnrolMemoryBodyAsk: "I never save anything by myself. When something sounds worth keeping I'll ask — you'll see " +
		"what I'd write down and which memory it goes to, and you pick a memory or tap Don't save. If you don't " +
		"answer, I don't save it.\n\nThat's all of it. Just talk to me normally.",

	Notice: identity,
}
