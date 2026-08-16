package config_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// isolatedDoc is a two-member isolated household with a group conversation: the shape
// deploy/compose.isolated.yml runs, and the one every scope below is judged against.
const isolatedDoc = `
mode: isolated
household: {name: Casa, shared_space: household, group_chat_id: -1001234567890, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, name: David, private_space: david-private, tiers: [local], bot_token_env: TOK_DAVID, passphrase_env: PASS_DAVID}
  - {id: jordan, name: Jordan, private_space: jordan-private, tiers: [local], bot_token_env: TOK_JORDAN, passphrase_env: PASS_JORDAN}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`

func mustDecode(t *testing.T, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	return cfg
}

func secretsFrom(vars map[string]string) *config.Secrets {
	return config.NewSecrets(config.SecretOptions{LookupEnv: env(vars)})
}

// TestValidateForUnitScopesSecretsToTheUnit is the whole of D-007 expressed as a test.
//
// A member's pod is given the household's configuration and only its own bot token,
// because holding a sibling's would be the isolation failure the mode exists to prevent.
// Before this scoping existed the pod refused to start over every token it correctly did
// not have, and the only way to satisfy it was to put every token in every pod.
func TestValidateForUnitScopesSecretsToTheUnit(t *testing.T) {
	t.Parallel()
	cfg := mustDecode(t, isolatedDoc)

	cases := []struct {
		name  string
		scope config.UnitScope
		have  map[string]string
		// wantNames are the secret paths that must be reported missing, in file
		// order as ValidationError carries them. Empty means the configuration must
		// validate.
		wantNames []string
	}{{
		name:  "david's pod holds david's token and passphrase and nothing else",
		scope: config.UnitScope{Member: "david"},
		have:  map[string]string{"TOK_DAVID": "t", "PASS_DAVID": "p"},
	}, {
		name:  "jordan's pod holds jordan's token and passphrase and nothing else",
		scope: config.UnitScope{Member: "jordan"},
		have:  map[string]string{"TOK_JORDAN": "t", "PASS_JORDAN": "p"},
	}, {
		// The group pod serves the shared space and holds no member's key, so a
		// passphrase there would be a secret that unwraps nothing — and a member's
		// passphrase in the one pod everybody talks to is the isolation loss the
		// mode exists to prevent.
		name:  "the group's pod holds the household token and no member's anything",
		scope: config.UnitScope{Group: true},
		have:  map[string]string{"TOK_HOUSEHOLD": "t"},
	}, {
		name:      "a member's pod still needs its own token",
		scope:     config.UnitScope{Member: "david"},
		have:      map[string]string{"TOK_JORDAN": "t", "TOK_HOUSEHOLD": "t", "PASS_DAVID": "p"},
		wantNames: []string{"members[0].bot_token"},
	}, {
		// Without this, the pod starts and answers every private message with the
		// locked notice: the failure isolated mode shipped with until it was found
		// against a real container runtime.
		name:      "a member's pod still needs its own passphrase",
		scope:     config.UnitScope{Member: "david"},
		have:      map[string]string{"TOK_DAVID": "t", "PASS_JORDAN": "p"},
		wantNames: []string{"members[0].passphrase"},
	}, {
		name:      "the group's pod still needs the household token",
		scope:     config.UnitScope{Group: true},
		have:      map[string]string{"TOK_DAVID": "t", "TOK_JORDAN": "t"},
		wantNames: []string{"telegram.bot_token"},
	}, {
		// The zero scope is the household node, and nothing about it changes: a
		// process that runs every unit needs every unit's token, and one missing
		// token is a member silently unserved.
		name:      "the household node needs every token",
		scope:     config.UnitScope{},
		have:      map[string]string{"TOK_DAVID": "t", "PASS_DAVID": "p", "PASS_JORDAN": "p"},
		wantNames: []string{"telegram.bot_token", "members[1].bot_token"},
	}, {
		name:  "the household node with every token",
		scope: config.UnitScope{},
		have: map[string]string{
			"TOK_DAVID": "t", "TOK_JORDAN": "t", "TOK_HOUSEHOLD": "t",
			"PASS_DAVID": "p", "PASS_JORDAN": "p",
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := secretsFrom(tc.have)
			err := cfg.ValidateForUnit(s, tc.scope)

			if len(tc.wantNames) == 0 {
				if err != nil {
					t.Fatalf("ValidateForUnit() error = %v, want nil", err)
				}
				if got := cfg.MissingSecretNamesForUnit(s, tc.scope); len(got) > 0 {
					t.Errorf("MissingSecretNamesForUnit() = %v, want none", got)
				}
				return
			}

			var ve *config.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateForUnit() error = %v, want *config.ValidationError", err)
			}
			assertSame(t, "MissingSecrets", ve.MissingSecrets, tc.wantNames)
			// The same set, and the accessor sorts it.
			sorted := append([]string(nil), tc.wantNames...)
			sort.Strings(sorted)
			assertSame(t, "MissingSecretNamesForUnit", cfg.MissingSecretNamesForUnit(s, tc.scope), sorted)
		})
	}
}

// TestValidateForUnitScopesEndpointKeysToTheChain.
//
// The second half of the same defect, and it kept deploy/compose.isolated.yml unstartable
// after the bot tokens were fixed. That file gives each container only the provider keys
// its own tier chain can reach — a key present in an environment is a key that can be
// used whatever routing intended — so david's local-only pod has no OPENROUTER_API_KEY.
// A named-but-unset variable is a hard fault rather than an absent optional secret, so an
// unscoped check refused david's pod for obeying its own configuration.
func TestValidateForUnitScopesEndpointKeysToTheChain(t *testing.T) {
	t.Parallel()
	const doc = `
mode: isolated
household: {shared_space: household, group_chat_id: -1, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: TOK_DAVID, passphrase_env: PASS_DAVID}
  - {id: jordan, private_space: jp, tiers: [local, cloud], bot_token_env: TOK_JORDAN, passphrase_env: PASS_JORDAN}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
  - {name: openrouter, base_url: https://openrouter.ai/api/v1, model: c, api_key_env: OPENROUTER_API_KEY, tags: [cloud]}
`
	cfg := mustDecode(t, doc)

	cases := []struct {
		name    string
		scope   config.UnitScope
		have    map[string]string
		wantErr bool
		// wantEndpoints are the endpoints this unit may route to, in file order.
		wantEndpoints []string
	}{{
		name:          "a local-only member's pod needs no provider key",
		scope:         config.UnitScope{Member: "david"},
		have:          map[string]string{"TOK_DAVID": "t", "PASS_DAVID": "p"},
		wantEndpoints: []string{"monster"},
	}, {
		name:          "a member whose chain reaches cloud needs that key",
		scope:         config.UnitScope{Member: "jordan"},
		have:          map[string]string{"TOK_JORDAN": "t", "PASS_JORDAN": "p"},
		wantErr:       true,
		wantEndpoints: []string{"monster", "openrouter"},
	}, {
		name:          "and starts once it has it",
		scope:         config.UnitScope{Member: "jordan"},
		have:          map[string]string{"TOK_JORDAN": "t", "PASS_JORDAN": "p", "OPENROUTER_API_KEY": "k"},
		wantEndpoints: []string{"monster", "openrouter"},
	}, {
		name:          "the group's pod follows household.tiers",
		scope:         config.UnitScope{Group: true},
		have:          map[string]string{"TOK_HOUSEHOLD": "t"},
		wantEndpoints: []string{"monster"},
	}, {
		name:          "the household node reaches everything and needs everything",
		scope:         config.UnitScope{},
		have:          map[string]string{"TOK_HOUSEHOLD": "t", "TOK_DAVID": "t", "TOK_JORDAN": "t", "PASS_DAVID": "p", "PASS_JORDAN": "p"},
		wantErr:       true,
		wantEndpoints: []string{"monster", "openrouter"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := cfg.ValidateForUnit(secretsFrom(tc.have), tc.scope)
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("ValidateForUnit() error = nil, want a refusal for the unset provider key")
			case tc.wantErr && !strings.Contains(err.Error(), "OPENROUTER_API_KEY"):
				t.Fatalf("ValidateForUnit() error = %v, want it to name OPENROUTER_API_KEY", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateForUnit() error = %v, want nil", err)
			}

			var names []string
			for _, ep := range cfg.EndpointsForUnit(tc.scope) {
				names = append(names, ep.Name)
			}
			assertSame(t, "EndpointsForUnit", names, tc.wantEndpoints)
		})
	}
}

// TestValidateForUnitDoesNotWeakenStructuralChecks: the scope is about which secrets
// have to resolve and about nothing else.
//
// Every pod reads the same file, so a file that cannot be served must be refused by
// whichever pod picks it up. A pod that started on a configuration the household node
// would reject is a pod running a household nobody else agrees exists.
func TestValidateForUnitDoesNotWeakenStructuralChecks(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		doc  string
		want string
	}{
		"two members on one private space": {
			doc: `
mode: isolated
household: {shared_space: household, group_chat_id: -1, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, private_space: shared-by-mistake, tiers: [local], bot_token_env: TOK_DAVID}
  - {id: jordan, private_space: shared-by-mistake, tiers: [local], bot_token_env: TOK_JORDAN}
endpoints:
  - {name: m, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			want: "is already members[0]'s private space",
		},
		"a tier no endpoint serves": {
			doc: `
mode: isolated
household: {shared_space: household, group_chat_id: -1, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, private_space: dp, tiers: [imaginary], bot_token_env: TOK_DAVID}
endpoints:
  - {name: m, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			want: `tier "imaginary" is not a tag on any endpoint`,
		},
		"two pods on one bot token": {
			doc: `
mode: isolated
household: {shared_space: household, group_chat_id: -1, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: TOK_SHARED}
  - {id: jordan, private_space: jp, tiers: [local], bot_token_env: TOK_SHARED}
endpoints:
  - {name: m, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			want: "sharing a bot defeats isolated mode",
		},
		"a secret naming two sources": {
			doc: `
mode: isolated
household: {shared_space: household, group_chat_id: -1, tiers: [local]}
telegram: {bot_token_env: TOK_HOUSEHOLD}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: TOK_DAVID, bot_token_file: /run/secrets/david}
endpoints:
  - {name: m, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			want: "members[0].bot_token",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustDecode(t, tc.doc)
			// Every secret this pod could want, so that nothing but the structural
			// fault can be what the refusal is about.
			s := secretsFrom(map[string]string{
				"TOK_HOUSEHOLD": "t", "TOK_DAVID": "t", "TOK_JORDAN": "t", "TOK_SHARED": "t",
			})
			err := cfg.ValidateForUnit(s, config.UnitScope{Member: "david"})
			if err == nil {
				t.Fatalf("ValidateForUnit() error = nil, want a refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateForUnit() error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateForUnitRefusesAMemberTheFileDoesNotName.
//
// Scoping is what makes this necessary. A selector pointing at nobody selects no
// secrets, so without this check the pod would validate clean, report itself healthy and
// then refuse to start — the worst of the three outcomes.
func TestValidateForUnitRefusesAMemberTheFileDoesNotName(t *testing.T) {
	t.Parallel()
	cfg := mustDecode(t, isolatedDoc)
	s := secretsFrom(map[string]string{"TOK_DAVID": "t", "TOK_JORDAN": "t", "TOK_HOUSEHOLD": "t"})

	err := cfg.ValidateForUnit(s, config.UnitScope{Member: "nobody"})
	if err == nil {
		t.Fatal("ValidateForUnit() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "names no member") {
		t.Errorf("ValidateForUnit() error = %v, want it to say the member is not there", err)
	}
}

// TestValidateWithSecretsIsTheHouseholdScope: the old entry point is the zero scope, so
// nothing that does not ask to be scoped is scoped.
func TestValidateWithSecretsIsTheHouseholdScope(t *testing.T) {
	t.Parallel()
	cfg := mustDecode(t, isolatedDoc)
	s := secretsFrom(map[string]string{"TOK_DAVID": "t"})

	whole := cfg.ValidateWithSecrets(s)
	zero := cfg.ValidateForUnit(s, config.UnitScope{})
	if whole == nil || zero == nil {
		t.Fatalf("both must refuse: ValidateWithSecrets() = %v, ValidateForUnit(zero) = %v", whole, zero)
	}
	if whole.Error() != zero.Error() {
		t.Errorf("ValidateWithSecrets() = %v\nValidateForUnit(zero) = %v\nwant identical", whole, zero)
	}
}

// TestUnitScopeMembership covers the three unit kinds directly, because the two
// predicates are what every scoped check in cmd/kenward asks.
func TestUnitScopeMembership(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		scope  config.UnitScope
		david  bool
		jordan bool
		group  bool
	}{
		{"household node", config.UnitScope{}, true, true, true},
		{"david's pod", config.UnitScope{Member: "david"}, true, false, false},
		{"the group's pod", config.UnitScope{Group: true}, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.scope.Serves("david"); got != tc.david {
				t.Errorf("Serves(david) = %v, want %v", got, tc.david)
			}
			if got := tc.scope.Serves("jordan"); got != tc.jordan {
				t.Errorf("Serves(jordan) = %v, want %v", got, tc.jordan)
			}
			if got := tc.scope.ServesGroup(); got != tc.group {
				t.Errorf("ServesGroup() = %v, want %v", got, tc.group)
			}
		})
	}
}

func assertSame(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}
