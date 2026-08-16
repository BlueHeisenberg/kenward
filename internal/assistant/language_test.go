package assistant

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// The boundary this file guards: everything the model reads is English and everything
// a member reads is theirs. It runs down the middle of one function — remind.Reminder.
// When serves the member, the model's system prompt and the operator's CLI — so it is
// the one place a careless build translates prompt text.

// spanishOptions is a member whose persona is Spanish, which is what selects the
// catalogue for their unit.
func spanishOptions() Options {
	o := testOptions()
	o.Persona = Persona{Language: "Spanish"}
	return o
}

// TestReminderWhenStaysEnglishWhateverTheMemberSpeaks. A Spanish-speaking member's
// reminders appear in their system prompt, and they must appear in English: the
// prompt is a specification the model is given, docs/PROMPT.md is checked against it
// verbatim, and the model is told the member's language by the persona rather than by
// having its instructions rewritten.
func TestReminderWhenStaysEnglishWhateverTheMemberSpeaks(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), spanishOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := remind.New("bin day", remind.EveryWeekly, 19, 0, time.Wednesday, "",
		testMemberChat, time.Date(2026, time.August, 15, 9, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.reminders.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("hola")); err != nil {
		t.Fatal(err)
	}
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("router never called")
	}
	system := req.Messages[0].Content

	if !strings.Contains(system, "every Wednesday at 19:00") {
		t.Errorf("the prompt does not state the schedule in English:\n%s", system)
	}
	// The Spanish reading of the same reminder, which is what the member gets and
	// what the prompt must not have.
	es := lang.For("Spanish")
	if got := es.When(r, time.UTC); strings.Contains(system, got) {
		t.Errorf("the member's language reached the model's prompt: %q", got)
	}
	for _, translated := range []string{"cada ", "miércoles", "todos los días"} {
		if strings.Contains(system, translated) {
			t.Errorf("the prompt contains %q; everything the model reads is English", translated)
		}
	}
}

// TestNoticesFollowTheMemberLanguage is the other half. The notices a member reads are
// emitted by the node, because there is no model to phrase them, and they are the
// strings that used to be English constants.
func TestNoticesFollowTheMemberLanguage(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), spanishOptions())
	if err != nil {
		t.Fatal(err)
	}
	es := lang.For("Spanish")
	for name, got := range map[string]string{
		"locked":        rig.unit.locked(),
		"contentFilter": rig.unit.contentFilter(),
		"queued":        rig.unit.queued(),
		"dropped":       rig.unit.dropped(),
		"noAnswer":      rig.unit.noAnswer(),
		"modelBusy":     rig.unit.modelBusy(),
		"misconfigured": rig.unit.misconfigured(),
		"turnFailed":    rig.unit.turnFailed(),
		"reasoningOnly": rig.unit.reasoningOnly(),
	} {
		if strings.Contains(got, "Try again") || strings.Contains(got, "your assistant") {
			t.Errorf("%s came out in English for a Spanish member: %q", name, got)
		}
	}
	if got := rig.unit.locked(); !strings.Contains(got, es.Locked) {
		t.Errorf("locked = %q, want the Spanish catalogue's wording", got)
	}
	// The glyph is structure rather than prose and is the code's in every language.
	if !strings.HasPrefix(rig.unit.noAnswer(), "⚠️ ") {
		t.Errorf("the problem glyph was lost: %q", rig.unit.noAnswer())
	}
}

// TestRefusalIsTranslatedIncludingItsListGrammar. The refusal is the longest thing a
// member reads and the one most likely to be read by somebody already annoyed. It is
// also where the English hardcoded its own list grammar.
func TestRefusalIsTranslatedIncludingItsListGrammar(t *testing.T) {
	u := &Unit{cat: lang.For("Spanish")}
	got := u.refusalText(testDirectScope(), &routing.NoBackendError{
		Chain: []string{"local"},
		Tried: []string{"pi", "igloo"},
	})
	if !strings.Contains(got, "de tus niveles permitidos") {
		t.Errorf("the refusal was not translated: %q", got)
	}
	// y becomes e before a word beginning with the sound /i/, and the machines are
	// named by the household.
	if !strings.Contains(got, " e <code>igloo</code>") {
		t.Errorf("Spanish list grammar did not alternate: %q", got)
	}
	if strings.Contains(got, " and ") {
		t.Errorf("English list grammar survived into a Spanish refusal: %q", got)
	}
}

// TestEnglishIsStillTheDefault. A unit with no persona resolves to English, which is
// what every golden file in this package asserts.
func TestEnglishIsStillTheDefault(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rig.unit.cat.Tag != lang.English {
		t.Errorf("a unit with no persona resolved to %q", rig.unit.cat.Tag)
	}
}
