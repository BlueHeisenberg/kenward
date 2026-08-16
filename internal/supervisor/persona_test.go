package supervisor

import (
	"context"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/assistant"
	"github.com/BlueHeisenberg/kenward/internal/config"
)

// TestPersonaReachesTheRightUnit is the wiring assertion. config.PersonaFor decides
// which persona belongs to which conversation and internal/assistant renders it; the
// only thing that can go wrong between them is this package handing a unit somebody
// else's, and that is a failure nothing else in the suite would see.
func TestPersonaReachesTheRightUnit(t *testing.T) {
	t.Run("one agent each gives a member their own and the group kenward's", func(t *testing.T) {
		// Isolated, and two pods, because that is what one agent each is: a member's
		// own bot runs their unit and the household's bot runs the group's. Simple
		// mode cannot deliver it — Config.AgentPerMember answers false there whatever
		// the file says, and config.Validate refuses the pair — so there is no
		// single-process version of this test to write.
		cfg := isolatedTestConfig()
		cfg.Household.Agents = config.AgentsPerMember
		cfg.Household.Persona = config.PersonaConfig{Language: "Spanish", Tone: "warm"}
		for i := range cfg.Members {
			if cfg.Members[i].ID == "david" {
				cfg.Members[i].Persona = config.PersonaConfig{AgentName: "Alfred", Tone: "very terse"}
			}
		}
		cfg.ApplyDefaults()

		davidPod := newSingleHarness(t, cfg, nil)
		defer func() { _ = davidPod.fake.Close() }()
		member := davidPod.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).Persona
		want := assistant.Persona{Name: "Alfred", Language: "Spanish", Tone: "very terse"}
		if member != want {
			t.Errorf("david's unit got %+v, want %+v (his own fields, the household's for the rest)", member, want)
		}

		// The group chat is always kenward's, whatever anybody named their own
		// agent. A member's name has no business in a room everybody reads.
		groupPod := newSingleHarness(t, cfg, func(o *SingleOptions) { o.Member, o.Group = "", true })
		defer func() { _ = groupPod.fake.Close() }()
		group := groupPod.sup.run.unitOptions(unitKey{group: true}, []string{"cloud"}).Persona
		wantGroup := assistant.Persona{Name: config.AgentName, Language: "Spanish", Tone: "warm"}
		if group != wantGroup {
			t.Errorf("the group unit got %+v, want %+v", group, wantGroup)
		}

		// And the third conversation — a member's private chat with kenward, which
		// runs in the household's pod — is kenward's too, not theirs.
		household := groupPod.sup.run.unitOptions(unitKey{member: "david", group: true}, []string{"cloud"}).Persona
		if household != wantGroup {
			t.Errorf("david's private chat with kenward got %+v, want kenward's own %+v", household, wantGroup)
		}
	})

	t.Run("one assistant gives every member the household's", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.Household.Agents = config.AgentsShared
		cfg.Household.Persona = config.PersonaConfig{Language: "Spanish"}
		for i := range cfg.Members {
			cfg.Members[i].Persona = config.PersonaConfig{AgentName: "Alfred", Tone: "very terse"}
		}
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		got := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).Persona
		want := assistant.Persona{Name: config.AgentName, Language: "Spanish"}
		if got != want {
			t.Errorf("under one assistant david's unit got %+v, want the household's %+v; a personal persona is carried in the file and ignored, never merged", got, want)
		}
	})

	t.Run("a household that chose nothing gets nothing but the name", func(t *testing.T) {
		cfg := budgetTestConfig()
		cfg.ApplyDefaults()
		h := newSimpleHarness(t, cfg, func(o *SimpleOptions) {})
		defer func() { _ = h.sup.Stop(context.Background()); _ = h.fake.Close() }()

		got := h.sup.run.unitOptions(unitKey{member: "david"}, []string{"local"}).Persona
		if want := (assistant.Persona{Name: config.AgentName}); got != want {
			t.Errorf("Persona = %+v, want %+v: the default has to be what kenward has always been", got, want)
		}
	})
}
