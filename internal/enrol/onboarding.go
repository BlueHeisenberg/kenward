package enrol

import (
	"fmt"

	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Onboarding is what a member reads in the first minute of knowing kenward exists.
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
// askPrivate is capture.private_writes: false — kenward's default — means a note to
// this member's own memory is written and then shown to them with an Undo button;
// true means it is put as a question first. The third message says which, because
// it is the one the member acts on, and a member told to expect a Save button who
// gets a written note and an Undo instead has been lied to about the one promise
// this product is built on.
func Onboarding(chatID int64, name string, askPrivate bool) []transport.Outbound {
	third := "When something sounds worth keeping for your own memory, I write it " +
		"down and then show you exactly what I wrote and which memory it went to, " +
		"with an Undo button that takes it back. Anything for the household's " +
		"shared memory I ask about first and write nothing until you tap Save.\n\n" +
		"Either way you always see it. That's all of it. Just talk to me normally."
	if askPrivate {
		third = "I never save anything by myself. When something sounds worth " +
			"keeping I'll ask — you'll see what I'd write down and which memory it " +
			"goes to, and you tap Save or Don't save. If you don't answer, I don't " +
			"save it.\n\nThat's all of it. Just talk to me normally."
	}
	texts := []string{
		fmt.Sprintf(
			"Hello %s. You're in.\n\n"+
				"This chat — just you and me — is your private memory. What you tell me "+
				"here stays in your space. Nobody else in the household can read it, and "+
				"I won't bring it up in the group.",
			name),

		"The household group chat is the shared memory. Whatever I remember there, " +
			"everyone can see. Nothing crosses over on its own: if something private " +
			"should become shared, ask me, and I'll show you the exact text before any " +
			"of it moves.",

		third,
	}

	out := make([]transport.Outbound, 0, len(texts))
	for _, t := range texts {
		out = append(out, transport.Outbound{ChatID: chatID, Text: t})
	}
	return out
}
