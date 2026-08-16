package config_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// The persona a member writes in their Telegram tutorial goes where their binding
// goes: the state file, through the Binder, folded back into members[].persona on the
// next start. These are the two halves of that — it survives the process, and it
// arrives as configuration rather than as a second thing to consult.

func personaStateConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Mode:     config.ModeSimple,
		DataDir:  t.TempDir(),
		Telegram: config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Household: config.HouseholdConfig{
			Name: "Home", SharedSpace: "shared", Tiers: []string{"local"},
		},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", PrivateSpace: "david", Tiers: []string{"local"}},
			// A persona an operator wrote by hand. Nothing in the product asks them
			// to, and it has to survive a member who answers only some questions.
			{
				ID: "jordan", Name: "Jordan", PrivateSpace: "jordan", Tiers: []string{"local"},
				Persona: config.PersonaConfig{Language: "Spanish", Character: "Knows the house."},
			},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

// TestAMembersPersonaSurvivesTheProcessThatWroteIt: written through the Binder,
// readable from a state file loaded fresh, and folded into the member it belongs to.
func TestAMembersPersonaSurvivesTheProcessThatWroteIt(t *testing.T) {
	cfg := personaStateConfig(t)
	binder, err := config.NewBinder(cfg, config.Provisioning{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	want := config.MemberPersona{
		Persona:      config.PersonaConfig{AgentName: "Alfred", Language: "English", Tone: "warm"},
		TutorialChat: 500,
	}
	if err := binder.SetMemberPersona(ctx, "david", want); err != nil {
		t.Fatalf("SetMemberPersona: %v", err)
	}

	// A member the configuration does not declare is refused rather than written: a
	// row naming somebody the household does not have is a row somebody later has to
	// decide what to do about.
	if err := binder.SetMemberPersona(ctx, "nobody", want); err == nil {
		t.Error("a persona was recorded for a member the household does not have")
	}

	// Read back from disk, as the next start does.
	st, err := config.LoadState(filepath.Join(cfg.DataDir, config.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.MemberPersona("david")
	if !ok || got != want {
		t.Fatalf("state holds %+v (%v), want %+v", got, ok, want)
	}

	// And it arrives as configuration, not as a second source to consult.
	next := personaStateConfig(t)
	if err := next.MergeState(st); err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	if next.Members[0].Persona != want.Persona {
		t.Errorf("members[0].persona = %+v, want %+v", next.Members[0].Persona, want.Persona)
	}
}

// TestAnUnansweredQuestionLeavesWhatTheFileSaid is the rule that makes an abandoned
// tutorial equivalent to one that never started. Every field the member did not
// answer is empty here, and empty means "not answered" rather than "answered with
// nothing" — so it cannot blank out a persona somebody wrote by hand.
func TestAnUnansweredQuestionLeavesWhatTheFileSaid(t *testing.T) {
	cfg := personaStateConfig(t)
	st := config.NewState()
	st.SetMemberPersona("jordan", config.MemberPersona{
		Persona:      config.PersonaConfig{Tone: "very terse"},
		TutorialChat: 501,
	})
	if err := cfg.MergeState(st); err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	want := config.PersonaConfig{Language: "Spanish", Tone: "very terse", Character: "Knows the house."}
	if got := cfg.Members[1].Persona; got != want {
		t.Errorf("members[1].persona = %+v, want %+v", got, want)
	}
	// And a member with no record at all is untouched.
	if got := cfg.Members[0].Persona; !got.IsZero() {
		t.Errorf("members[0].persona = %+v, want empty", got)
	}
}

// TestAPersonaForADeletedMemberIsInert, exactly as a binding for one is: removing
// somebody from kenward.yaml is a legitimate way to remove them, and a leftover row
// must not resurrect them or refuse the start.
func TestAPersonaForADeletedMemberIsInert(t *testing.T) {
	cfg := personaStateConfig(t)
	st := config.NewState()
	st.SetMemberPersona("gone", config.MemberPersona{
		Persona: config.PersonaConfig{AgentName: "Ghost"},
	})
	if err := cfg.MergeState(st); err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	for _, m := range cfg.Members {
		if m.Persona.AgentName == "Ghost" {
			t.Fatalf("a persona for a member the file does not name reached %s", m.ID)
		}
	}
	if len(cfg.Members) != 2 {
		t.Errorf("MergeState created a member: %d, want 2", len(cfg.Members))
	}
	if _, ok := cfg.MemberByID(domain.MemberID("gone")); ok {
		t.Error("a member exists that the configuration never declared")
	}
}
