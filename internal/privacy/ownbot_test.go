package privacy

import "testing"

// TestOwnBotNoteIsGolden pins the one paragraph in this package that exists to prevent a
// specific wrong belief rather than to describe a mechanism.
//
// A member under one-agent-each makes a bot in BotFather, gives it a name, and starts
// talking to their own assistant. Under isolated mode that bot is the seal. Under simple
// mode the ceremony is identical and the bot is nothing but a contact. Nothing on either
// screen distinguishes them, so the difference has to be stated, in the mode where the
// answer is the disappointing one, in words nobody can read past.
func TestOwnBotNoteIsGolden(t *testing.T) {
	t.Parallel()

	t.Run("simple mode says a bot is not a seal", func(t *testing.T) {
		t.Parallel()
		s := OwnBotNote(ModeSimple)

		mustContain(t, s, "does NOT mean your memory is sealed",
			"this is the precise misunderstanding the note exists to prevent, and it must be emphatic")
		mustContain(t, s, "separate contact, not a separate secret",
			"the distinction between identity and isolation is the whole content of this paragraph")
		mustContain(t, s, "can still read every member's private memory",
			"the operator's reach is unchanged by a per-member bot and must be restated here")
		mustContain(t, s, "the other question",
			"the member must be told that sealing is answered somewhere else, not by this setting")

		// It must not offer the reassurance the mode does not earn. "Your own" plus
		// "sealed" in a sentence that is not a denial is how this paragraph would rot.
		mustNotContain(t, s, "nobody else can read",
			"simple mode may not borrow isolated mode's claim, however a bot is described")
	})

	t.Run("isolated mode says the mode did it, not the bot", func(t *testing.T) {
		t.Parallel()
		s := OwnBotNote(ModeIsolated)

		mustContain(t, s, "It is the mode that seals your memory",
			"a member who moves house must not learn that their bot was what protected them")
		mustContain(t, s, "plain text",
			"the mechanism is that no component anybody else controls sees plaintext")
	})

	t.Run("an unknown mode has nothing to say", func(t *testing.T) {
		t.Parallel()
		if got := OwnBotNote(ModeUnknown); got != "" {
			t.Errorf("OwnBotNote(ModeUnknown) = %q, want empty: a privacy claim for a mode nobody chose is a bug, not a default", got)
		}
	})
}
