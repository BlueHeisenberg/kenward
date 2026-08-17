package enrol

import (
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Greeting is the first thing a member ever reads: the claim worked, and setup is
// about to start.
//
// It is the one message that cannot be in the language the member chose, because
// choosing it is the first question and this arrives before it. It goes out in the
// household's language, which the admin set in the wizard: kenward's language seeds
// the default for new members. See docs/ARCHITECTURE.md, "The node speaks ten
// languages; the prompt speaks one".
//
// questions is how many are actually coming, counted from the tutorial's own step
// list rather than written into the sentence: a household with one agent for
// everybody has nothing to name, so it is asked three and was told four.
func Greeting(chatID int64, member string, t text, questions int) transport.Outbound {
	return transport.Outbound{ChatID: chatID, Text: t.greeting(member, t.number(questions))}
}

// Explanation is what a member reads once the setup questions are done.
//
// It is three short messages and it is the only place the memory model is explained
// to the person it applies to. Everything downstream — scope resolution, the capture
// confirmation, the refusal to offer a private destination in a group — enforces
// exactly what these paragraphs promise. If the enforcement ever changes, this copy
// is wrong, and wrong copy about privacy is worse than none.
//
// Deliberately plain: no product voice, no feature list, no invitation to explore.
// Someone was handed a code by a person they live with. What they need is the shape
// of the thing, in the order they will run into it — this chat, the group chat, and
// what happens when it wants to remember something.
//
// It is sent on every ending the tutorial has, including the ones where the member
// answered nothing. The setup questions are conveniences; this is the part the
// product is obliged to say, and an abandoned tutorial must not swallow it.
//
// askPrivate is capture.private_writes: false — kenward's default — means a note to
// this member's own memory is written and then shown to them with an Undo button;
// true means it is put as a question first. The third message says which, because
// it is the one the member acts on, and a member told to expect a Save button who
// gets a written note and an Undo instead has been lied to about the one promise
// this product is built on.
//
// Each message opens with a bold heading naming what it is about, because three
// consecutive paragraphs of undifferentiated prose is what the member was getting
// and none of it survived the first read. The headings are the shape of the
// memory model in three words, and they are what a member scrolls back to find.
//
// They carry no glyph, unlike the messages they describe. The glyph vocabulary
// marks events — something was saved, something is being asked — and these are
// explanations rather than events. A member who sees 🧠 here and 🧠 on a real
// write learns nothing from either.
// The copy is the catalogue's rather than this package's tutorial table, and that is
// the one place the two language lists differ on purpose: the questions are written
// in two languages and this is written in ten. A member who asked for a language the
// tutorial cannot hold still gets the part of onboarding the product is obliged to
// deliver, in their own language.
func Explanation(chatID int64, c lang.Catalogue, askPrivate bool) []transport.Outbound {
	third := c.EnrolMemoryBodyDefault
	if askPrivate {
		third = c.EnrolMemoryBodyAsk
	}
	texts := []string{
		transport.Bold(c.EnrolPrivateHeading) + "\n\n" + c.EnrolPrivateBody,
		transport.Bold(c.EnrolGroupHeading) + "\n\n" + c.EnrolGroupBody,
		transport.Bold(c.EnrolMemoryHeading) + "\n\n" + third,
	}

	out := make([]transport.Outbound, 0, len(texts))
	for _, s := range texts {
		out = append(out, transport.Outbound{ChatID: chatID, Text: s})
	}
	return out
}
