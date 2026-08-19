package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// TestSharedOnlyValidation is the argument for the field being stated rather than
// inferred, written as tests.
//
// The whole risk of this feature is one file losing one line. If an absent
// private_space were the signal, a member who was supposed to have private memory and
// whose line got deleted would keep working — and their next private note would land
// in the household's shared space, in front of everybody, with nothing having failed.
// So absence stays an error for everybody who has not said otherwise, and saying
// otherwise while also naming a private space is a contradiction rather than a
// precedence: there is no reading of that pair the loader is entitled to pick.
func TestSharedOnlyValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		env  map[string]string
		// want are substrings each of which must appear in some problem. Empty
		// means the configuration must load.
		want []string
	}{
		{
			name: "a shared_only member needs nothing but an id and a name",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: leo, name: Leo, shared_only: true}
endpoints:
  - {name: monster, base_url: http://m:8000/v1, model: q, tags: [local]}
`,
			env: map[string]string{"T": "t"},
		},
		{
			// The pair that must never be resolved by precedence. Whichever way a
			// loader picked, half the households writing it would get the other
			// answer, and one of the two answers publishes somebody's private
			// notes to the household.
			name: "shared_only and a private space together",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: leo, name: Leo, shared_only: true, private_space: lp}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`members[0].private_space: "lp" is set on a shared_only member`},
		},
		{
			// And the other direction, unchanged: absence is still a fault for
			// everybody who has not said shared_only. This is the case that must
			// keep failing, or the feature has quietly become "a missing line
			// downgrades your privacy".
			name: "a member who is not shared_only still needs a private space",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, shared_only: false}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"members[0].private_space: required"},
		},
		{
			// A pod that will never be started. Left unreported, an operator would
			// create two secrets, export them, and wait for a container that no
			// code path creates.
			name: "shared_only with a pod's credentials",
			yaml: `
mode: isolated
household: {shared_space: household, group_chat_id: -100}
telegram: {bot_token_env: T}
members:
  - {id: leo, name: Leo, shared_only: true, bot_token_env: TOK_LEO, passphrase_env: PASS_LEO}
`,
			env: map[string]string{"T": "t", "TOK_LEO": "x", "PASS_LEO": "y"},
			want: []string{
				`members[0].bot_token_env: "TOK_LEO" is set on a shared_only member`,
				`members[0].passphrase_env: "PASS_LEO" is set on a shared_only member`,
			},
		},
		{
			// The chain is the privacy policy for a member's own material and this
			// member has none, so nothing reads it. An operator who wrote
			// `tiers: [local]` here believes they have confined a teenager's
			// conversations to the house and has not.
			name: "shared_only with a tier chain of their own",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: leo, name: Leo, shared_only: true, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:8000/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"members[0].tiers: a shared_only member's conversations are the household's"},
		},
		{
			// The isolated-mode half of "no pod". Every other member in this file
			// is required to name a bot token and a passphrase, and this one must
			// not be: demanding them would make the household unstartable over
			// credentials for a container nobody creates.
			name: "isolated mode demands no secrets of a shared_only member",
			yaml: `
mode: isolated
household: {shared_space: household, group_chat_id: -100, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, private_space: dp, tiers: [local], bot_token_env: TOK_D, passphrase_env: PASS_D}
  - {id: leo, name: Leo, shared_only: true}
endpoints:
  - {name: monster, base_url: http://m:8000/v1, model: q, tags: [local]}
`,
			env: map[string]string{"T": "t", "TOK_D": "x", "PASS_D": "y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.ParseWithEnv(strings.NewReader(tc.yaml), lookup(tc.env))
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("ParseWithEnv() error = %v, want a valid configuration", err)
				}
				return
			}
			var ve *config.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ParseWithEnv() error = %v (%T), want *config.ValidationError", err, err)
			}
			for _, sub := range tc.want {
				found := false
				for _, p := range ve.Problems {
					if strings.Contains(p, sub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no problem contains %q; got:\n%s", sub, err)
				}
			}
		})
	}
}

func lookup(env map[string]string) config.LookupEnvFunc {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// TestSharedOnlyMemberHasNoAgentOfTheirOwn: PersonaFor must give them the household's
// voice whatever household.agents says, because there is no agent of theirs for a
// name, a tone or a character to belong to.
//
// The interesting row is the per_member one. AgentPerMember answers true about the
// household and says nothing about this member; a PersonaFor that only asked the
// household's question would hand them an agent_name for an assistant they do not have
// and that nothing would ever render.
func TestSharedOnlyMemberHasNoAgentOfTheirOwn(t *testing.T) {
	base := func(mode config.Mode, agents config.Agents) *config.Config {
		return &config.Config{
			Mode: mode,
			Household: config.HouseholdConfig{
				Name: "Home", SharedSpace: "household", GroupChatID: -100, Agents: agents,
				Persona: config.PersonaConfig{Language: "English", Tone: "warm", Character: "dry"},
			},
			Members: []config.MemberConfig{{
				ID: "leo", Name: "Leo", TelegramID: 3, SharedOnly: true,
				Persona: config.PersonaConfig{AgentName: "Alfred", Language: "Español", Tone: "playful"},
			}},
		}
	}

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		perMem bool
	}{
		{"one shared agent", base(config.ModeSimple, config.AgentsShared), false},
		{"one agent each", base(config.ModeIsolated, config.AgentsPerMember), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.AgentPerMember(); got != tc.perMem {
				t.Fatalf("AgentPerMember() = %v, want %v; this test is about the household's answer being irrelevant", got, tc.perMem)
			}
			p := tc.cfg.PersonaFor("leo")
			if p.AgentName != config.AgentName {
				t.Errorf("AgentName = %q, want the household's own %q; they have no agent of theirs to name", p.AgentName, config.AgentName)
			}
			if p.Tone != "warm" || p.Character != "dry" {
				t.Errorf("tone/character = %q/%q, want the household's warm/dry", p.Tone, p.Character)
			}
			// The one field that is still theirs: kenward answers them, so it
			// answers them in a language, and it is theirs to choose.
			if p.Language != "Español" {
				t.Errorf("Language = %q, want their own Español", p.Language)
			}
		})
	}
}

// TestSharedOnlyReachesTheDomainMember guards the conversion. scope.Resolve reads this
// field and nothing else tells it; a Domain() that dropped it would resolve every one
// of these members to a direct scope with an empty private space.
func TestSharedOnlyReachesTheDomainMember(t *testing.T) {
	cfg := &config.Config{Members: []config.MemberConfig{
		{ID: "leo", Name: "Leo", TelegramID: 3, SharedOnly: true},
		{ID: "david", Name: "David", TelegramID: 4, PrivateSpace: "dp"},
	}}

	leo, ok := cfg.MemberByID("leo")
	if !ok {
		t.Fatal("MemberByID lost the member")
	}
	if !leo.SharedOnly {
		t.Error("SharedOnly did not survive the conversion to domain.Member")
	}
	if leo.HasPrivateMemory() {
		t.Error("HasPrivateMemory() is true for a member with no private space")
	}

	david, _ := cfg.MemberByID("david")
	if david.SharedOnly {
		t.Error("SharedOnly leaked onto the member in the next row")
	}
	if !david.HasPrivateMemory() {
		t.Error("HasPrivateMemory() is false for a member who has one")
	}

	// The fault case, which must be neither: a member nobody marked shared_only and
	// whose private space is missing has no private memory, and is not the member
	// the household decided has none.
	broken := domain.Member{ID: "x", TelegramID: 5}
	if broken.SharedOnly || broken.HasPrivateMemory() {
		t.Error("a member with a missing private space must be neither shared_only nor holding private memory")
	}
}
