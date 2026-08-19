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
	ToolMisfire:   "I tried to do something for that and got it wrong, so nothing happened. Ask me again.",
	NothingSaved:  "I didn't record anything just then. Say it again if you want me to remember it.",
	ResetNotice:   "Starting fresh — I've cleared the earlier part of this conversation. Nothing in your memory changed; this is the scheduled reset.",

	// Acknowledgements only. No "yes", no "no", no "correct": those answer
	// questions, and a matched reply is dropped rather than annotated.
	BareAcknowledgements: []string{
		"done", "all done", "got it", "gotcha", "got that", "noted", "duly noted",
		"saved", "stored", "recorded", "understood", "ok", "okay", "all set",
		"will do", "consider it done", "no problem", "sure thing",
	},

	// Words about a write, the promise to retain — the same lie in the future tense —
	// and the claim to hold the fact, which is what "got it" is.
	//
	// "Got it" is here and "done" is not, and the line between them is possession
	// against completion. "Done" says an errand finished and says nothing about
	// memory: "Done — the boiler service code is 4471." is an answer, and the guard
	// must leave it whole. "Got it — heron-ashfield-42." says the node is holding the
	// thing the member just handed it, which on a turn that stored nothing is false.
	// It is also where the residue actually lives — two of two non-calling turns in a
	// twenty-sample live run, and the shape docs/PROMPT.md's narration rule was split
	// out of the capture block to stop ("Got it — boiler service code is 4471, and
	// I've kept it just to you").
	//
	// The first-person forms on the second line are what every other table in this
	// package already carries — "he guardado", "ich habe gespeichert", "j'ai
	// enregistré", "ho salvato", "帮你记下了" — and English was the one language
	// missing them. That gap only became visible once the bare participles moved
	// behind a gate: they are also this language's bare acknowledgements, so
	// ClaimsASaveUnmistakably steps over them, and without "i saved" beside them a
	// reply like "Yes, I saved it earlier — the plumber's number is in your private
	// memory" claims a completed write in words nothing unconditional could see.
	//
	// A subject and a past-tense verb cannot be an acknowledgement in the way the bare
	// participle can: "Saved." answers a person, "I saved it" states a thing that
	// happened. That is the whole of why these belong on the unconditional side.
	SaveClaims: []string{
		"saved", "stored", "recorded", "noted", "jotted", "written down",
		"i saved", "i've saved", "i have saved", "i noted", "i've noted",
		"i have noted", "i stored", "i've stored", "i recorded", "i've recorded",
		"got it", "got that", "i have it", "i've got it",
		"made a note", "make a note", "making a note", "got it down",
		"added to your", "added to the household", "is now in your",
		"is now in the household", "in your memory", "in the household memory",
		// The destination named with the verb's object in between: "I've added that
		// to your private memory" slips past "added to your", and the phrases above
		// are truncated before the noun precisely so the qualifier does not matter —
		// which does not help when the gap is in the middle. These two are the tail
		// instead, so any verb reaches them.
		//
		// Not the bare noun phrase, deliberately. "your private memory" on its own is
		// in a sentence the prompt itself teaches — the household scope's disclosure
		// says the assistant cannot see one — and a table holding it would annotate a
		// model correctly repeating a scope rule.
		"to your memory", "to your private memory",
	},

	// The future tense of the same lie, and only a lie when it answers a request.
	// "Yep — drop me the day next time and I'll keep it." is an honest offer.
	SavePromises: []string{
		"i'll remember", "i will remember", "i'll keep that", "i will keep that",
		"i'll keep it", "i won't forget", "i will not forget", "i'll hold on to",
		"i'll note", "i will note", "i'll save", "i will save",
	},

	// The member handing something over to be kept. Nothing here appears in
	// "thanks", "ok" or "that was great" — those are the messages this list exists
	// to let past the two gated guards.
	SaveRequests: []string{
		"remember", "note this", "note that", "note it", "note down",
		"make a note", "take a note", "jot", "write this down", "write that down",
		"write it down", "write down", "save this", "save that", "save it",
		"keep this", "keep that", "keep a note", "keep a record",
		"store this", "store that", "add this to", "add that to", "put this in",
		"don't forget", "do not forget", "for future reference", "for next time",
	},

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
	WrittenHint: "The Undo button removes it.",
	// "Not saved", not "I didn't save it": the member is reading the entry, struck
	// through, immediately above — this is a label on it, not a second telling of
	// what they just did.
	NotSaved: func(private bool) string {
		if private {
			return "Not saved to your private memory."
		}
		return "Not saved to the household memory."
	},
	PromotionOpener: "This would be published to the household exactly as it stands, and cannot be unpublished:",
	PromotionCloser: "Publish it?",
	AlsoKnownAs:     func(words []string) string { return "Also known as: " + latinList(words) },
	EnglishGloss:    func(summary string) string { return "The text above is kept in English. It says: " + summary },

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
	// The second sentence is internal/privacy's simple-mode statement, word for word,
	// and enrol.TestSimpleOnboardingClaimsOnlyWhatPrivacySays fails if it stops being.
	// A claim a member reads in a chat and a claim an operator reads from `kenward
	// doctor` that have drifted apart are two claims, and the softer one is the lie.
	EnrolPrivateBody: "This chat — just you and me — is your private memory. What you tell me here is " +
		"stored in your own space, and the household group can never read it. I won't bring it up there either.",
	// Both strong sentences are internal/privacy's isolated-mode statement, word for
	// word, including the limit. The limit travels with the claim: a member who finds
	// out on their own that root on the box can reach a running key stops believing
	// the sentence before it.
	EnrolPrivateSealed: "This household runs in isolated mode: your assistant has a process of its own and a key " +
		"of its own. Nobody else in the household can read your private memory, and neither can the person who " +
		"runs this machine. The honest limit: someone with root access to this machine, while your assistant is " +
		"running, could reach your key.",
	EnrolSharedOnlyHeading: "We share one memory",
	EnrolSharedOnlyBody: "You're part of this household, so I answer you here and in the family group, " +
		"and you can ask me anything the household knows.\n\nWhat you don't have is a " +
		"memory of your own. There is no space here for just you and me: everything I " +
		"remember from you goes to the household's shared memory, where everyone can " +
		"read it. So I show you the exact words first and write nothing until you say " +
		"yes — in this chat as much as in the group, because it is the same memory " +
		"either way.\n\nIf you would rather have one of your own, ask whoever set me " +
		"up. Nothing carries over when that changes, because nothing was ever stored.",
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
	// The Undo sentence carries the bounded promise, because this is the card where
	// the button is introduced and there is room here for the one thing a member
	// needs to know about it: what tapping it actually buys. It used to be said on
	// every undo, which was a paragraph in place of a label. It is a tombstone, not a
	// shred — "won't come back in an answer", never "erased".
	EnrolMemoryBodyDefault: "When something sounds worth keeping for your own memory, I write it down and then show you " +
		"exactly what I wrote and which memory it went to, with an Undo button that takes it back — tap it and " +
		"the entry won't come back in an answer, not here and not on any other device in the household. Anything for " +
		"the household's shared memory I ask about first and write nothing until you tap Save to household.\n\n" +
		"Either way you always see it. That's all of it. Just talk to me normally.",
	EnrolMemoryBodyAsk: "I never save anything by myself. When something sounds worth keeping I'll ask — you'll see " +
		"what I'd write down and which memory it goes to, and you pick a memory or tap Don't save. If you don't " +
		"answer, I don't save it.\n\nThat's all of it. Just talk to me normally.",

	Notice: identity,
}
