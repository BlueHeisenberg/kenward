package config_test

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// personaConfig is a minimal household with a persona on both layers.
//
// The mode follows the answer rather than being fixed, because the two are not
// independent: per_member needs a Telegram bot for each member and only isolated mode
// has them, so validateHousehold refuses the combination and Config.AgentPerMember
// answers false for it. A fixture that asked for per_member in simple mode would be
// asking what a member's persona is in a household kenward will not start.
func personaConfig(agents config.Agents) *config.Config {
	mode := config.ModeSimple
	if agents == config.AgentsPerMember {
		mode = config.ModeIsolated
	}
	david := config.MemberConfig{
		ID: "david", Name: "David", PrivateSpace: "david", Tiers: []string{"local"},
		Persona: config.PersonaConfig{AgentName: "Alfred", Tone: "very terse"},
	}
	jordan := config.MemberConfig{ID: "jordan", Name: "Jordan", PrivateSpace: "jordan", Tiers: []string{"local"}}
	if mode == config.ModeIsolated {
		david.BotTokenEnv, david.PassphraseEnv = "DAVID_BOT_TOKEN", "DAVID_PASSPHRASE"
		jordan.BotTokenEnv, jordan.PassphraseEnv = "JORDAN_BOT_TOKEN", "JORDAN_PASSPHRASE"
	}
	cfg := &config.Config{
		Mode:     mode,
		Telegram: config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Household: config.HouseholdConfig{
			Name:        "Home",
			Agents:      agents,
			SharedSpace: "shared",
			Tiers:       []string{"local"},
			Persona: config.PersonaConfig{
				Language:  "Spanish",
				Tone:      "warm",
				Character: "Knows the house well.",
			},
		},
		Members: []config.MemberConfig{david, jordan},
	}
	cfg.ApplyDefaults()
	return cfg
}

// TestPersonaForSharedIsEveryonesPersona is the claim the wizard has to make out loud
// before an admin answers: under one agent there is no personal layer, so the persona
// the admin chose for kenward is the persona every member gets in their own private
// chat. A member's own fields are carried and ignored, never merged.
func TestPersonaForSharedIsEveryonesPersona(t *testing.T) {
	cfg := personaConfig(config.AgentsShared)
	got := cfg.PersonaFor("david")
	want := config.PersonaConfig{
		AgentName: config.AgentName,
		Language:  "Spanish",
		Tone:      "warm",
		Character: "Knows the house well.",
	}
	if got != want {
		t.Errorf("PersonaFor(david) under shared = %+v, want the household's %+v", got, want)
	}
}

// TestPersonaForPerMemberFallsBackPerField: a member who chose a name and a tone and
// said nothing about language keeps the household's language. All-or-nothing would
// take Spanish away from somebody whose only edit was to ask for shorter answers.
func TestPersonaForPerMemberFallsBackPerField(t *testing.T) {
	cfg := personaConfig(config.AgentsPerMember)

	david := cfg.PersonaFor("david")
	want := config.PersonaConfig{
		AgentName: "Alfred",
		Language:  "Spanish",
		Tone:      "very terse",
		Character: "Knows the house well.",
	}
	if david != want {
		t.Errorf("PersonaFor(david) = %+v, want %+v", david, want)
	}

	// A member who has written nothing yet — the state every member is in between
	// redeeming an invite and finishing the tutorial — gets kenward's, unchanged.
	jordan := cfg.PersonaFor("jordan")
	if jordan != cfg.HouseholdPersona() {
		t.Errorf("PersonaFor(jordan) = %+v, want the household's %+v", jordan, cfg.HouseholdPersona())
	}

	// And a member the file does not name resolves to the household's rather than to
	// something empty or to somebody else's.
	if stranger := cfg.PersonaFor("nobody"); stranger != cfg.HouseholdPersona() {
		t.Errorf("PersonaFor(unknown) = %+v, want the household's", stranger)
	}
}

// TestDefaultPersonaIsTodaysBehaviour: a configuration that says nothing about personas
// resolves to nothing but the name kenward, for the household and for every member,
// whichever way agents is set.
func TestDefaultPersonaIsTodaysBehaviour(t *testing.T) {
	for _, agents := range []config.Agents{config.AgentsShared, config.AgentsPerMember} {
		cfg := personaConfig(agents)
		cfg.Household.Persona = config.PersonaConfig{}
		cfg.Members[0].Persona = config.PersonaConfig{}
		cfg.Members[1].Persona = config.PersonaConfig{}
		want := config.PersonaConfig{AgentName: config.AgentName}
		if got := cfg.PersonaFor("david"); got != want {
			t.Errorf("agents=%s: PersonaFor(david) = %+v, want %+v", agents, got, want)
		}
		if got := cfg.HouseholdPersona(); got != want {
			t.Errorf("agents=%s: HouseholdPersona() = %+v, want %+v", agents, got, want)
		}
	}
}

// TestAgentsDefaultsToShared: the identity question's default is today's behaviour, and
// an omitted key means it. Every configuration written before this key existed means
// one assistant for the household, and must go on meaning it.
func TestAgentsDefaultsToShared(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader("mode: simple\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Household.Agents != config.AgentsShared {
		t.Errorf("Household.Agents = %q with the key absent, want %q", cfg.Household.Agents, config.AgentsShared)
	}
}

// TestPersonaValidation covers the three rules: agents is one of two words, kenward's
// own name is not a setting, and persona text is bounded because it rides in every
// prompt and the budget loop never trims it.
func TestPersonaValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{
			name: "an unknown agents value",
			edit: func(c *config.Config) { c.Household.Agents = "one-each" },
			want: "household.agents",
		},
		{
			name: "renaming kenward",
			edit: func(c *config.Config) { c.Household.Persona.AgentName = "Jarvis" },
			want: "household.persona.agent_name",
		},
		{
			name: "a character longer than the limit",
			edit: func(c *config.Config) {
				c.Members[0].Persona.Character = strings.Repeat("a", config.MaxPersonaCharacter+1)
			},
			want: "members[0].persona.character",
		},
		{
			name: "a tone longer than the limit",
			edit: func(c *config.Config) {
				c.Household.Persona.Tone = strings.Repeat("b", config.MaxPersonaLine+1)
			},
			want: "household.persona.tone",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := personaConfig(config.AgentsPerMember)
			cfg.Endpoints = []config.EndpointConfig{
				{Name: "local", BaseURL: "http://localhost:8000/v1", Model: "m", Tags: []string{"local"}},
			}
			cfg.ApplyDefaults()
			c.edit(cfg)
			err := cfg.Validate(func(string) (string, bool) { return "x", true })
			if err == nil {
				t.Fatalf("Validate accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate reported %v, want a problem naming %s", err, c.want)
			}
		})
	}
}

// TestPersonaDefaultConfigurationValidates is the other half: the fixture above, with
// personas on both layers and nothing over the limits, is a configuration kenward will
// serve. A limit that refused ordinary use would be worse than no limit.
func TestPersonaDefaultConfigurationValidates(t *testing.T) {
	cfg := personaConfig(config.AgentsPerMember)
	cfg.Endpoints = []config.EndpointConfig{
		{Name: "local", BaseURL: "http://localhost:8000/v1", Model: "m", Tags: []string{"local"}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(func(string) (string, bool) { return "x", true }); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
