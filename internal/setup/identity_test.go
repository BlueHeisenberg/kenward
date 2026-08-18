package setup

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/lang"
)

// identityAnswers is simpleAnswers with the four identity-step answers replaced.
// simpleAnswers takes the default for all four; these tests are about what happens
// when somebody answers them.
func identityAnswers(identity, language, tone, character string) []string {
	return identityAnswersIn(simpleAnswers(), identity, language, tone, character)
}

// testGroupChatID is the household group's chat id, in the shape Telegram hands out
// for a supergroup. One agent each cannot be written without one — kenward lives in
// the group chat and nowhere else under that answer — so every script that chooses it
// carries this.
const testGroupChatID = "-1001234567890"

// isolatedIdentityAnswers is the same for a household that answered the trust
// question the other way. One agent each needs a bot for each member, and only
// isolated mode has them, so it is the only script in which choosing it gets past
// the question.
func isolatedIdentityAnswers(language, tone, character string) []string {
	answers := append([]string{"2"}, simpleAnswers()[1:]...)
	return identityAnswersIn(answers, "2", testGroupChatID, language, tone, character)
}

// identityAnswersIn replaces the identity step's four defaults with identity followed
// by rest. rest is variadic because one each asks one more question than one shared
// does: the household group's chat id.
func identityAnswersIn(answers []string, identity string, rest ...string) []string {
	for i, a := range answers {
		// The identity step sits between the last member's name and the first
		// endpoint, and simpleAnswers marks it with four empty answers in a row.
		// The run of four must be the *whole* run: the answer that closes the list
		// of members is itself empty and sits immediately before it, so a search
		// that accepts a prefix of five finds the wrong place by one.
		if a == "" && i+4 < len(answers) && answers[i+1] == "" && answers[i+2] == "" && answers[i+3] == "" && answers[i+4] != "" {
			out := append([]string(nil), answers[:i]...)
			out = append(out, identity)
			out = append(out, rest...)
			return append(out, answers[i+4:]...)
		}
	}
	panic("the answer script no longer has the identity step's four defaults in a row")
}

// TestIdentityDefaultsToOneAssistant: pressing Enter through the whole step writes the
// household kenward has always had. The identity question's default has to be today's
// behaviour, because every household that has never thought about it is answering it.
func TestIdentityDefaultsToOneAssistant(t *testing.T) {
	_, cfg, io, err := runWizard(t, "linux", Options{}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.Agents != config.AgentsShared {
		t.Errorf("household.agents = %q, want %q from an unanswered question", cfg.Household.Agents, config.AgentsShared)
	}
	if !cfg.Household.Persona.IsZero() {
		t.Errorf("household.persona = %+v, want empty; three Enters must change nothing", cfg.Household.Persona)
	}
}

// TestIdentityQuestionStatesItsConsequence is the requirement the identity design is most
// insistent about: under one assistant there is no personal layer, so the character the
// admin is about to write is everyone's. If the wizard does not say so at the point of
// asking, an admin chooses a character for the household believing they are choosing
// one for themselves.
func TestIdentityQuestionStatesItsConsequence(t *testing.T) {
	_, _, io, err := runWizard(t, "linux", Options{}, identityAnswers("1", "", "", "")...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	transcript := io.Transcript()
	if !strings.Contains(transcript, "what everyone in the house") {
		t.Errorf("choosing one assistant never said the persona is everyone's:\n%s", transcript)
	}
	// And it says nothing about the security question, which is a different question
	// with a different answer and is asked elsewhere.
	block := transcript[strings.Index(transcript, "Who kenward is"):]
	block = block[:strings.Index(block, "Endpoints")]
	for _, forbidden := range []string{"isolated", "Isolated", "container", "Podman", "sealed"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("the identity step mentions %q; it is a presentation question and must not be confused with the trust question", forbidden)
		}
	}
}

// TestOneEachIsWrittenAndSaysWhoWritesTheRest: choosing one agent each records it, and
// says plainly that the admin is not choosing anybody else's persona.
func TestOneEachIsWrittenAndSaysWhoWritesTheRest(t *testing.T) {
	// Isolated, because that is the only mode one agent each can be delivered in:
	// an agent is a Telegram contact and simple mode runs one bot. See
	// TestOneEachInSimpleModeIsRefusedWithTheReason for the other half.
	_, cfg, io, err := runWizard(t, "linux", Options{},
		isolatedIdentityAnswers("Spanish", "warm", "Knows the house well.")...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.Agents != config.AgentsPerMember {
		t.Errorf("household.agents = %q, want %q", cfg.Household.Agents, config.AgentsPerMember)
	}
	want := config.PersonaConfig{Language: "Spanish", Tone: "warm", Character: "Knows the house well."}
	if cfg.Household.Persona != want {
		t.Errorf("household.persona = %+v, want %+v", cfg.Household.Persona, want)
	}
	// Nobody's personal persona was written on their behalf.
	for _, m := range cfg.Members {
		if !m.Persona.IsZero() {
			t.Errorf("the wizard wrote a persona for %s; a member's own is theirs to write in Telegram", m.Name)
		}
	}
	if !strings.Contains(io.Transcript(), "you are not choosing for them") {
		t.Error("the wizard did not say that each member writes their own")
	}
}

// TestPersonaLanguageIsHonestAboutWhatItChanges. The setting reaches two places under
// two different rules — free text to the model, a closed list for kenward's own
// messages — and a household told about only the first finds out the difference the
// first time it names a language nobody has written the copy in. Saying it in the
// wizard is cheaper than every other way of finding out.
func TestPersonaLanguageIsHonestAboutWhatItChanges(t *testing.T) {
	_, _, io, err := runWizard(t, "linux", Options{}, identityAnswers("1", "Spanish", "", "")...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if !strings.Contains(io.Transcript(), "kenward's own messages stay\n  English") {
		t.Errorf("the language question does not say what falls back to English:\n%s", io.Transcript())
	}
	// The note's list is built from the catalogue rather than typed out, so an
	// eleventh language cannot ship with the wizard still describing ten. This is the
	// assertion that the wiring is actually in place.
	for _, name := range lang.EnglishNames() {
		if !strings.Contains(io.Transcript(), name) {
			t.Errorf("the language note does not list %s, which the catalogue holds", name)
		}
	}
}

// TestPersonaTooLongIsRefusedInTheWizard, so that the limit is met as a question rather
// than as a validation error after everything else has been answered.
func TestPersonaTooLongIsRefusedInTheWizard(t *testing.T) {
	answers := identityAnswers("1", "", "", strings.Repeat("x", config.MaxPersonaCharacter+1))
	// The refusal re-asks, so one more answer is needed than the flow would
	// otherwise take; it is appended at the point the question is re-put.
	for i, a := range answers {
		if strings.HasPrefix(a, "xxxx") {
			answers = append(answers[:i+1], append([]string{"fine"}, answers[i+1:]...)...)
			break
		}
	}
	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.Persona.Character != "fine" {
		t.Errorf("character = %q, want the second answer", cfg.Household.Persona.Character)
	}
	if !strings.Contains(io.Transcript(), "is never trimmed to fit") {
		t.Error("the wizard refused the long answer without saying why the limit exists")
	}
}

// TestOneEachInSimpleModeIsRefusedWithTheReason: the offer is made in both modes and
// declined in the one that cannot keep it, in the wizard rather than three screens
// later in config.Validate.
//
// Refused rather than hidden, and refused rather than downgraded. Hidden, and a
// household never learns the arrangement exists; downgraded, and they are handed
// kenward under several names with no way to tell. The reason it gives is a counting
// one — one bot, one contact, one agent — because the trust question has already been
// asked and this is not it being asked again.
func TestOneEachInSimpleModeIsRefusedWithTheReason(t *testing.T) {
	// Ask for one each, get told why not, then take the default. The refusal re-puts
	// the question, so one more answer is needed at that point.
	answers := identityAnswers("2", "", "", "")
	idx := identityIndex(answers)
	answers = append(answers[:idx+1], append([]string{"1"}, answers[idx+1:]...)...)

	_, cfg, io, err := runWizard(t, "linux", Options{}, answers...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.Agents != config.AgentsShared {
		t.Errorf("household.agents = %q, want %q; simple mode has one bot and therefore one agent",
			cfg.Household.Agents, config.AgentsShared)
	}
	transcript := io.Transcript()
	if !strings.Contains(transcript, "two agents behind one contact are one agent") {
		t.Errorf("the wizard refused one each without saying why:\n%s", transcript)
	}
	// And the configuration it wrote is one kenward will actually serve, which is the
	// property the refusal exists for.
	if err := cfg.Validate(func(string) (string, bool) { return "x", true }); err != nil {
		t.Errorf("the wizard wrote a configuration kenward refuses: %v", err)
	}
}

// identityIndex finds the identity step's answer in a script built by identityAnswers:
// the first of the four answers the step consumes.
func identityIndex(answers []string) int {
	for i := range answers {
		if answers[i] == "2" && i+3 < len(answers) && answers[i+1] == "" && answers[i+2] == "" && answers[i+3] == "" {
			return i
		}
	}
	panic("the answer script does not contain an identity step answered with one each")
}
