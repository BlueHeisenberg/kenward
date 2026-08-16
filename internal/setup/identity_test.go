package setup

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// identityAnswers is simpleAnswers with the four identity-step answers replaced.
// simpleAnswers takes the default for all four; these tests are about what happens
// when somebody answers them.
func identityAnswers(identity, language, tone, character string) []string {
	answers := simpleAnswers()
	for i, a := range answers {
		// The identity step sits between the members' spaces and the first endpoint,
		// and simpleAnswers marks it with four empty answers in a row.
		if a == "" && i+3 < len(answers) && answers[i+1] == "" && answers[i+2] == "" && answers[i+3] == "" {
			out := append([]string(nil), answers[:i]...)
			out = append(out, identity, language, tone, character)
			return append(out, answers[i+4:]...)
		}
	}
	panic("simpleAnswers no longer has the identity step's four defaults in a row")
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

// TestIdentityQuestionStatesItsConsequence is the requirement IDENTITY.md is most
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
	_, cfg, io, err := runWizard(t, "linux", Options{},
		identityAnswers("2", "Spanish", "warm", "Knows the house well.")...)
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

// TestPersonaLanguageIsHonestAboutWhatItChanges. The language setting reaches the model
// and not kenward's own strings, and a household that is told otherwise will find out
// the first time it saves something and reads an English confirmation. Saying it in the
// wizard is cheaper than every other way of finding out.
func TestPersonaLanguageIsHonestAboutWhatItChanges(t *testing.T) {
	_, _, io, err := runWizard(t, "linux", Options{}, identityAnswers("1", "Spanish", "", "")...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if !strings.Contains(io.Transcript(), "are still English") {
		t.Errorf("the language question does not say what it leaves in English:\n%s", io.Transcript())
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
