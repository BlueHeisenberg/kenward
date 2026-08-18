package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/session"
)

// env returns a LookupEnvFunc over a fixed map, so no test touches the real process
// environment — which is global state shared with every other test in the binary.
func env(pairs map[string]string) config.LookupEnvFunc {
	return func(name string) (string, bool) {
		v, ok := pairs[name]
		return v, ok
	}
}

const fullYAML = `
mode: simple

household:
  name: "Home"
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local, local-slow, cloud]

telegram:
  bot_token_env: KENWARD_BOT_TOKEN

members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]
  - id: maria
    name: Maria
    telegram_id: 0
    private_space: maria-private
    tiers: [local, cloud]

endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local, local-slow]
    timeout: 120s
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    tags: [cloud]

memory:
  search_limit: 8

session:
  idle_timeout: 30m

capture:
  max_proposals_per_turn: 1

update:
  channel: stable
  check_interval: 6h
`

func fullEnv() config.LookupEnvFunc {
	return env(map[string]string{
		"KENWARD_BOT_TOKEN":  "1234:token",
		"OPENROUTER_API_KEY": "sk-secret-value",
	})
}

func TestParseFullDocument(t *testing.T) {
	cfg, err := config.ParseWithEnv(strings.NewReader(fullYAML), fullEnv())
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}

	if cfg.Mode != config.ModeSimple {
		t.Errorf("Mode = %q, want simple", cfg.Mode)
	}
	if cfg.Household.GroupChatID != -1001234567890 {
		t.Errorf("GroupChatID = %d", cfg.Household.GroupChatID)
	}
	if got, want := cfg.Household.Tiers, []string{"local", "local-slow", "cloud"}; !reflect.DeepEqual(got, want) {
		t.Errorf("household.tiers = %v, want %v", got, want)
	}
	if len(cfg.Members) != 2 || cfg.Members[0].ID != "david" || cfg.Members[1].TelegramID != 0 {
		t.Fatalf("members = %+v", cfg.Members)
	}
	if got, want := cfg.Endpoints[0].Timeout.Duration(), 120*time.Second; got != want {
		t.Errorf("endpoints[0].timeout = %v, want %v", got, want)
	}
	if got, want := cfg.Session.IdleTimeout.Duration(), 30*time.Minute; got != want {
		t.Errorf("session.idle_timeout = %v, want %v", got, want)
	}
	if got, want := cfg.Update.CheckInterval.Duration(), 6*time.Hour; got != want {
		t.Errorf("update.check_interval = %v, want %v", got, want)
	}
}

// TestDefaults checks the values an operator gets by saying nothing at all.
func TestDefaults(t *testing.T) {
	const minimal = `
mode: simple
household:
  name: Home
  shared_space: household
  tiers: [local]
telegram:
  bot_token_env: TOKEN
members:
  - id: david
    private_space: david-private
    tiers: [local]
endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen
    tags: [local]
`
	cfg, err := config.ParseWithEnv(strings.NewReader(minimal), env(map[string]string{"TOKEN": "t"}))
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"memory.search_limit", cfg.Memory.SearchLimit, config.DefaultSearchLimit},
		{"session.idle_timeout", cfg.Session.IdleTimeout.Duration(), config.DefaultIdleTimeout},
		{"capture.max_proposals_per_turn", cfg.Capture.MaxProposalsPerTurn, config.DefaultMaxProposalsPerTurn},
		{"update.channel", cfg.Update.Channel, config.DefaultUpdateChannel},
		{"update.check_interval", cfg.Update.CheckInterval.Duration(), config.DefaultCheckInterval},
		{"endpoints[0].timeout", cfg.Endpoints[0].Timeout.Duration(), config.DefaultEndpointTimeout},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

}

// TestLoreCommandIsGone: the key no longer exists, in any mode.
//
// It located a binary for kenward to execute, and kenward executes none � the store
// and, since the sync daemon moved in-process, everything else are library calls. The
// tests it replaces asserted the opposite in detail: that isolated mode defaulted it,
// that the default was a fresh slice, that a blank program name was a validation error.
// All of them were testing the upkeep of a value nothing read.
//
// Refused rather than ignored, because the strict decoder refuses every unknown key and
// silently accepting this one would be the only place kenward pretends a setting exists.
// What it must not do is refuse it in the decoder's own words, so the hint is the
// property under test: a household that ran setup before this change has the key on
// disk, and "field lore_command not found in type config.MemoryConfig" tells them
// nothing they can act on.
func TestLoreCommandIsGone(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeSimple, config.ModeIsolated} {
		t.Run(string(mode), func(t *testing.T) {
			_, err := config.Decode(strings.NewReader("mode: " + string(mode) + "\nmemory: {lore_command: [lore]}\n"))
			if err == nil {
				t.Fatal("a configuration naming a removed key was accepted")
			}
			for _, want := range []string{"lore_command", "delete the line"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}

	// And an isolated configuration is complete without it. This is the half that used
	// to fail: validateMemory required the key in isolated mode, so the mode kenward
	// ships for privacy could not be described without naming a program it never ran.
	isolated := &config.Config{
		Mode:      config.ModeIsolated,
		Household: config.HouseholdConfig{SharedSpace: "household", Tiers: []string{"local"}},
		Telegram:  config.TelegramConfig{BotTokenEnv: "T"},
		Endpoints: []config.EndpointConfig{{Name: "m", BaseURL: "http://m:1/v1", Model: "q", Tags: []string{"local"}}},
		Update:    config.UpdateConfig{Channel: config.UpdateStable},
	}
	if err := isolated.Validate(env(map[string]string{"T": "t"})); err != nil {
		t.Fatalf("an isolated configuration with no lore command was refused: %v", err)
	}
}

func TestApplyDefaultsIsIdempotent(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(fullYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	before := *cfg
	cfg.ApplyDefaults()
	cfg.ApplyDefaults()
	if !reflect.DeepEqual(before.Session, cfg.Session) || !reflect.DeepEqual(before.Update, cfg.Update) {
		t.Error("ApplyDefaults() is not idempotent")
	}
}

func TestUnknownFieldIsAnError(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"top level", "mode: simple\nnonsense: 1\n", "nonsense"},
		{"nested", "household:\n  shred_space: household\n", "shred_space"},
		{"in a list element", "members:\n  - id: david\n    privat_space: x\n", "privat_space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Decode(strings.NewReader(tt.yaml))
			if err == nil {
				t.Fatal("Decode() error = nil, want an unknown-field error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the offending field %q", err, tt.want)
			}
		})
	}
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"seconds", "120s", 120 * time.Second, false},
		{"minutes", "30m", 30 * time.Minute, false},
		{"hours", "6h", 6 * time.Hour, false},
		{"quoted", `"90s"`, 90 * time.Second, false},
		{"compound", "1h30m", 90 * time.Minute, false},
		{"millis", "500ms", 500 * time.Millisecond, false},
		{"empty means unset", `""`, 0, false},
		{"bare number is not a duration", "30", 0, true},
		{"unitless string", `"30"`, 0, true},
		{"nonsense", "soon", 0, true},
		{"negative", "-5m", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := fmt.Sprintf("session:\n  idle_timeout: %s\n", tt.value)
			cfg, err := config.Decode(strings.NewReader(doc))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Decode() error = nil, want an error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode() error: %v", err)
			}
			// Zero stands: idle expiry is off by default and an empty value is
			// not rewritten into a duration behind the operator's back.
			if got := cfg.Session.IdleTimeout.Duration(); got != tt.want {
				t.Errorf("idle_timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDurationString(t *testing.T) {
	d := config.Duration(90 * time.Second)
	if got, want := d.String(), "1m30s"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	v, err := d.MarshalYAML()
	if err != nil || v != "1m30s" {
		t.Errorf("MarshalYAML() = (%v, %v), want (\"1m30s\", nil)", v, err)
	}
}

// validationCase drives the table below: a document, an environment, and the substrings
// every problem message is expected to contain.
type validationCase struct {
	name     string
	yaml     string
	env      map[string]string
	want     []string // substrings that must each appear in some problem
	wantNone bool     // the configuration is expected to be valid
}

func TestValidation(t *testing.T) {
	tests := []validationCase{
		{
			name: "valid",
			yaml: fullYAML,
			env:  map[string]string{"KENWARD_BOT_TOKEN": "t", "OPENROUTER_API_KEY": "k"},
			want: nil, wantNone: true,
		},
		{
			name: "missing mode",
			yaml: `
household: {shared_space: household}
telegram: {bot_token_env: T}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"mode: required"},
		},
		{
			name: "unknown mode",
			yaml: `
mode: paranoid
household: {shared_space: household}
`,
			want: []string{`mode: "paranoid" is not a mode`},
		},
		{
			name: "household tier is not a tag on any endpoint",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local, cloud]}
telegram: {bot_token_env: T}
endpoints:
  - {name: monster, base_url: http://m:8000/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`household.tiers: tier "cloud" is not a tag on any endpoint`},
		},
		{
			name: "member tier is not a tag on any endpoint",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp, tiers: [local, secret-lab]}
endpoints:
  - {name: monster, base_url: http://m:8000/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`members[0].tiers: tier "secret-lab" is not a tag on any endpoint`},
		},
		{
			name: "duplicate private space",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: shared-by-accident}
  - {id: maria, private_space: shared-by-accident}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`members[1].private_space: "shared-by-accident" is already members[0]'s private space`},
		},
		{
			name: "private space equal to the shared space",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: household}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`members[0].private_space: "household" is also household.shared_space`},
		},
		{
			name: "duplicate telegram id",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, telegram_id: 42, private_space: dp}
  - {id: maria, telegram_id: 42, private_space: mp}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`members[1].telegram_id: 42 is already members[0]'s telegram id`},
		},
		{
			name: "several unclaimed members are fine",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, telegram_id: 0, private_space: dp, tiers: [local]}
  - {id: maria, telegram_id: 0, private_space: mp, tiers: [local]}
  - {id: sam, private_space: sp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: nil, wantNone: true,
		},
		{
			name: "duplicate and empty member ids",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp}
  - {id: david, private_space: dp2}
  - {id: "", private_space: dp3}
  - {id: "   ", private_space: dp4}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				`members[1].id: duplicate member id "david"`,
				"members[2].id: required",
				"members[3].id: required",
			},
		},
		{
			name: "missing private space",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"members[0].private_space: required"},
		},
		{
			name: "missing shared space",
			yaml: `
mode: simple
telegram: {bot_token_env: T}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"household.shared_space: required"},
		},
		{
			name: "simple mode without a bot token variable",
			yaml: `
mode: simple
household: {shared_space: household}
`,
			want: []string{"telegram.bot_token_env: required in simple mode"},
		},
		{
			name: "isolated mode without per-member bot tokens",
			yaml: `
mode: isolated
household: {shared_space: household}
members:
  - {id: david, private_space: dp}
  - {id: maria, private_space: mp, bot_token_env: T_MARIA}
`,
			env:  map[string]string{"T_MARIA": "t"},
			want: []string{"members[0].bot_token_env: required in isolated mode"},
		},
		{
			name: "isolated mode with a shared bot token variable",
			yaml: `
mode: isolated
household: {shared_space: household}
members:
  - {id: david, private_space: dp, bot_token_env: T_ONE}
  - {id: maria, private_space: mp, bot_token_env: T_ONE}
`,
			env:  map[string]string{"T_ONE": "t"},
			want: []string{"members[1].bot_token_env: T_ONE is already members[0]'s token variable"},
		},
		{
			// Uniqueness used to be checked member-vs-member only, so the one bot
			// that serves the whole household could also be a member's own and
			// nothing said so: the group pod and that member's pod on one token,
			// reading each other's messages, in the mode whose entire purpose is
			// that they cannot.
			name: "isolated mode with a member on the household's bot token variable",
			yaml: `
mode: isolated
household: {shared_space: household, group_chat_id: -1001234567890, tiers: [local]}
telegram: {bot_token_env: T_HOUSE}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_HOUSE}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_HOUSE": "t"},
			want: []string{"members[0].bot_token_env: T_HOUSE is also telegram.bot_token_env"},
		},
		{
			// The file form of the same fault, and with no group chat configured:
			// isolated mode does not use the household token until a group exists,
			// and a file that validates today must not become a shared bot the day
			// somebody adds group_chat_id.
			name: "isolated mode with a member on the household's bot token file",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_file: /etc/kenward/token}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_file: /etc/kenward/token}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			want: []string{"members[0].bot_token_file: /etc/kenward/token is also telegram.bot_token_file"},
		},
		{
			// The supervisor builds a pod name by collapsing everything outside
			// [A-Za-z0-9_-] to a hyphen, so these two ids are one pod: whoever
			// starts second is served by the first member's pod, with the first
			// member's lore volume and bot token.
			name: "member ids that differ only where the pod name is sanitised",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local]}
members:
  - {id: a.b, private_space: dp, tiers: [local], bot_token_env: T_ONE}
  - {id: a-b, private_space: mp, tiers: [local], bot_token_env: T_TWO}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_ONE": "t", "T_TWO": "t"},
			want: []string{`members[1].id: "a-b" and "a.b" are different ids but the same pod name "a-b"`},
		},
		{
			// The emptiness checks trim; the uniqueness maps used not to, so a
			// leading space was a way to write the same id twice and be told
			// nothing.
			name: "ids and private spaces differing only in whitespace",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp, tiers: [local]}
  - {id: "david ", private_space: " dp", tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				"duplicate member id",
				"is already members[0]'s private space",
			},
		},
		{
			// scope.Resolve matches the group on chat id before it looks at the
			// sender, so this collision would put a member's direct messages in the
			// household's scope. Resolve refuses both now; the file is still wrong.
			name: "group chat id that is also a member's telegram id",
			yaml: `
mode: simple
household: {shared_space: household, group_chat_id: 4242, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, telegram_id: 4242, private_space: dp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: []string{"members[0].telegram_id: 4242 is also household.group_chat_id"},
		},
		{
			// A member token is inert in simple mode, so the two-sources rule used
			// to go unchecked on it: the file validated, and the contradiction
			// surfaced only for whoever later switched the mode.
			name: "two sources for a secret this mode does not use",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID, bot_token_file: /etc/kenward/david.token}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t", "T_DAVID": "t"},
			want: []string{"members[0].bot_token: bot_token_file and bot_token_env are both set"},
		},
		{
			// The group pod runs on the household bot even when every member has
			// their own, so a group chat plus no household token used to validate and
			// then fail when the first group message arrived.
			name: "isolated mode with a group chat needs the household token",
			yaml: `
mode: isolated
household: {shared_space: household, group_chat_id: -1001234567890, tiers: [local]}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_DAVID": "t"},
			want: []string{"telegram.bot_token", "household.group_chat_id"},
		},
		{
			name: "isolated mode with a group chat and a household token",
			yaml: `
mode: isolated
household: {shared_space: household, group_chat_id: -1001234567890, tiers: [local]}
telegram: {bot_token_env: T_HOUSE}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID, passphrase_env: P_DAVID}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_HOUSE": "t", "T_DAVID": "t", "P_DAVID": "p"},
			want: nil, wantNone: true,
		},
		{
			name: "isolated mode needs no household token",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local]}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID, passphrase_env: P_DAVID}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_DAVID": "t", "P_DAVID": "p"},
			want: nil, wantNone: true,
		},
		{
			name: "missing environment variables are listed by name",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: KENWARD_BOT_TOKEN}
endpoints:
  - {name: openrouter, base_url: https://openrouter.ai/api/v1, model: m, api_key_env: OPENROUTER_API_KEY, tags: [cloud]}
`,
			env: map[string]string{},
			want: []string{
				"environment variable KENWARD_BOT_TOKEN is not set",
				"environment variable OPENROUTER_API_KEY is not set",
			},
		},
		{
			name: "an environment variable set to nothing is as bad as an unset one",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
`,
			env:  map[string]string{"T": "   "},
			want: []string{"environment variable T is set but empty"},
		},
		{
			name: "endpoints without a key need no variable",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
endpoints:
  - {name: monster, base_url: http://monster.tail:8000/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: nil, wantNone: true,
		},
		{
			name: "endpoint faults",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
endpoints:
  - {name: "", base_url: http://a:1/v1, model: m}
  - {name: dup, base_url: http://b:1/v1, model: m}
  - {name: dup, base_url: http://c:1/v1, model: m}
  - {name: nomodel, base_url: http://d:1/v1}
  - {name: nourl, model: m}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				"endpoints[0].name: required",
				`endpoints[2].name: duplicate endpoint name "dup"`,
				"endpoints[3].model: required",
				"endpoints[4].base_url: required",
			},
		},
		{
			// An unstated chain is refused rather than defaulted: inheriting the
			// household's chain would widen a member's privacy policy without anyone
			// saying so, and leaving it empty would parse, start, and then refuse
			// every turn for a reason nobody can see.
			name: "an unstated tier chain is an error, not a default",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp}
  - {id: maria, private_space: mp, tiers: []}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				"household.tiers: required",
				"members[0].tiers: required",
				"members[1].tiers: required",
				"the privacy policy",
			},
		},
		{
			name: "empty tier names",
			yaml: `
mode: simple
household: {shared_space: household, tiers: ["", local]}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp, tiers: [" "]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local, ""]}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				"household.tiers: contains an empty tier name",
				"members[0].tiers: contains an empty tier name",
				"endpoints[0].tags: contains an empty tier name",
			},
		},
		{
			name: "unknown update channel",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
update: {channel: yolo}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`update.channel: "yolo" is not a channel`},
		},
		{
			name: "update channel off is supported",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
update: {channel: off}
`,
			env:  map[string]string{"T": "t"},
			want: nil, wantNone: true,
		},
		{
			// One agent each needs a bot for each member, and simple mode runs one
			// bot for the whole household. Refused rather than downgraded: the
			// downgrade is silent and costs every member their private assistant,
			// because their own chat would resolve to the household's.
			name: "one agent each in simple mode",
			yaml: `
mode: simple
household: {shared_space: household, tiers: [local], agents: per_member}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, private_space: david-private, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t"},
			want: []string{`household.agents: "per_member" needs a bot for each member`},
		},
		{
			name: "an agents value that is neither choice",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local], agents: several}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, private_space: david-private, tiers: [local], bot_token_env: D, passphrase_env: DP}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T": "t", "D": "d", "DP": "p"},
			want: []string{`household.agents: "several" is not one of shared or per_member`},
		},
		{
			name: "one agent each in isolated mode is accepted",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local], agents: per_member, group_chat_id: -100123}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, private_space: david-private, tiers: [local], bot_token_env: D, passphrase_env: DP}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
update: {channel: off}
`,
			env:      map[string]string{"T": "t", "D": "d", "DP": "p"},
			want:     nil,
			wantNone: true,
		},
		{
			name: "negative limits",
			yaml: `
mode: simple
household: {shared_space: household}
telegram: {bot_token_env: T}
memory: {search_limit: -1}
capture: {max_proposals_per_turn: -3}
`,
			env: map[string]string{"T": "t"},
			want: []string{
				"memory.search_limit: -1 is negative",
				"capture.max_proposals_per_turn: -3 is negative",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.ParseWithEnv(strings.NewReader(tt.yaml), env(tt.env))
			if tt.wantNone {
				if err != nil {
					t.Fatalf("ParseWithEnv() error = %v, want a valid configuration", err)
				}
				return
			}
			var ve *config.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ParseWithEnv() error = %v (%T), want *config.ValidationError", err, err)
			}
			for _, wantSub := range tt.want {
				if !containsSub(ve.Problems, wantSub) {
					t.Errorf("no problem contains %q; got:\n%s", wantSub, err)
				}
			}
		})
	}
}

func containsSub(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// TestValidationAccumulates is the point of the ValidationError type: an operator with
// a badly broken file sees the whole list once, not one fault per restart.
func TestValidationAccumulates(t *testing.T) {
	const broken = `
mode: paranoid
household: {shared_space: household, tiers: [cloud]}
members:
  - {id: david, telegram_id: 7, private_space: household}
  - {id: david, telegram_id: 7, private_space: dp, tiers: [gpu]}
endpoints:
  - {name: bad, base_url: "not a url", model: ""}
update: {channel: yolo}
`
	_, err := config.ParseWithEnv(strings.NewReader(broken), env(nil))

	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v (%T), want *config.ValidationError", err, err)
	}
	if len(ve.Problems) < 8 {
		t.Fatalf("got %d problems, want every fault at once:\n%s", len(ve.Problems), err)
	}

	msg := err.Error()
	for _, wantSub := range []string{
		"is not a mode",
		`tier "cloud" is not a tag on any endpoint`,
		"is also household.shared_space",
		"duplicate member id",
		"is already members[0]'s telegram id",
		`tier "gpu" is not a tag on any endpoint`,
		"endpoints[0].model: required",
		"endpoints[0].base_url",
		"is not a channel",
	} {
		if !strings.Contains(msg, wantSub) {
			t.Errorf("accumulated error is missing %q; got:\n%s", wantSub, msg)
		}
	}
	if !strings.HasPrefix(msg, fmt.Sprintf("config: %d problems:", len(ve.Problems))) {
		t.Errorf("error does not open with a count: %q", msg)
	}
}

func TestValidationErrorSingularWording(t *testing.T) {
	ve := &config.ValidationError{Problems: []string{"mode: required"}}
	if got := ve.Error(); !strings.HasPrefix(got, "config: 1 problem:") {
		t.Errorf("Error() = %q, want a singular opening", got)
	}
}

func TestBaseURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"http with port and path", "http://monster.tail:8000/v1", false},
		{"https", "https://openrouter.ai/api/v1", false},
		{"http bare host", "http://localhost", false},
		{"ip literal", "http://192.168.1.10:8000/v1", false},
		{"relative", "monster.tail:8000/v1", true},
		{"scheme-relative", "//monster.tail/v1", true},
		{"no host", "http:///v1", true},
		{"unsupported scheme", "ws://monster.tail/v1", true},
		{"file scheme", "file:///models", true},
		{"control character", "http://mon\x7fster/v1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode:      config.ModeSimple,
				Household: config.HouseholdConfig{SharedSpace: "household"},
				Telegram:  config.TelegramConfig{BotTokenEnv: "T"},
				Endpoints: []config.EndpointConfig{{Name: "e", BaseURL: tt.url, Model: "m"}},
			}
			cfg.ApplyDefaults()
			err := cfg.Validate(env(map[string]string{"T": "t"}))

			var ve *config.ValidationError
			hasURLProblem := errors.As(err, &ve) && containsSub(ve.Problems, "endpoints[0].base_url")
			if hasURLProblem != tt.wantErr {
				t.Errorf("base_url %q: problem = %v, want %v (err: %v)", tt.url, hasURLProblem, tt.wantErr, err)
			}
		})
	}
}

// budgetYAML is a household whose two local machines have very different windows and
// whose cloud endpoint states neither field, so one file exercises all three cases:
// declared, declared-and-smaller, and defaulted.
const budgetYAML = `
mode: simple

household:
  name: Home
  shared_space: household
  tiers: [local, local-slow, cloud]

telegram:
  bot_token_env: KENWARD_BOT_TOKEN

members:
  - id: david
    name: David
    private_space: david-private
    tiers: [local]

endpoints:
  - name: monster
    base_url: http://192.168.1.20:8000/v1
    model: monster
    tags: [local]
    context_window: 262144
    max_completion_tokens: 32768
  - name: mini
    base_url: http://localhost:11434/v1
    model: qwen2.5:3b
    tags: [local-slow]
    context_window: 8192
    max_completion_tokens: 2048
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    tags: [cloud]

memory:
  search_limit: 8
`

// TestEndpointBudgetFields is the whole feature read off one configuration: a stated
// window is honoured, a chain takes the minimum across every endpoint it reaches, and
// an endpoint that states nothing is defaulted rather than left at zero.
func TestEndpointBudgetFields(t *testing.T) {
	cfg, err := config.ParseWithEnv(strings.NewReader(budgetYAML), env(map[string]string{"KENWARD_BOT_TOKEN": "t"}))
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}

	// Declared values survive the load untouched.
	if got := cfg.Endpoints[0]; got.ContextWindow != 262144 || got.MaxCompletionTokens != 32768 {
		t.Errorf("monster = (%d, %d), want (262144, 32768)", got.ContextWindow, got.MaxCompletionTokens)
	}
	// An endpoint that states neither gets the defaults, which is what makes the
	// fallback a number an operator can look up rather than a silent zero.
	if got := cfg.Endpoints[2]; got.ContextWindow != config.DefaultContextWindow ||
		got.MaxCompletionTokens != config.DefaultMaxCompletionTokens {
		t.Errorf("openrouter = (%d, %d), want the defaults (%d, %d)",
			got.ContextWindow, got.MaxCompletionTokens,
			config.DefaultContextWindow, config.DefaultMaxCompletionTokens)
	}

	for _, tt := range []struct {
		name             string
		chain            []string
		window, maxToken int
	}{
		{"one big endpoint", []string{"local"}, 262144, 32768},
		{"the defaulted endpoint", []string{"cloud"}, config.DefaultContextWindow, config.DefaultMaxCompletionTokens},
		{"a chain takes the minimum", []string{"local", "local-slow"}, 8192, 2048},
		{"order does not matter", []string{"local-slow", "local"}, 8192, 2048},
		{"and across all three", []string{"local", "local-slow", "cloud"}, 8192, 2048},
		{"a chain reaching nothing derives nothing", []string{"mystery"}, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, m := cfg.ChainLimits(tt.chain)
			if w != tt.window || m != tt.maxToken {
				t.Errorf("ChainLimits(%v) = (%d, %d), want (%d, %d)", tt.chain, w, m, tt.window, tt.maxToken)
			}
		})
	}
}

// TestEndpointBudgetValidation covers the refusal. A cap that does not fit inside its
// own endpoint's window is a configuration error caught at load, with the endpoint
// named, rather than a unit that fails to construct at startup on two derived numbers
// that name nobody.
func TestEndpointBudgetValidation(t *testing.T) {
	tests := []struct {
		name      string
		window    int
		maxTokens int
		wantErr   bool
	}{
		{"comfortably inside", 262144, 32768, false},
		{"both defaulted", 0, 0, false},
		{"a small window against the default cap", 2048, 0, true},
		{"cap equals window", 8192, 8192, true},
		{"cap exceeds window", 8192, 16384, true},
		{"negative window", -1, 1024, true},
		{"negative cap", 8192, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode:      config.ModeSimple,
				Household: config.HouseholdConfig{SharedSpace: "household", Tiers: []string{"local"}},
				Telegram:  config.TelegramConfig{BotTokenEnv: "T"},
				Endpoints: []config.EndpointConfig{{
					Name: "monster", BaseURL: "http://192.168.1.20:8000/v1", Model: "monster",
					Tags: []string{"local"}, ContextWindow: tt.window, MaxCompletionTokens: tt.maxTokens,
				}},
			}
			cfg.ApplyDefaults()
			err := cfg.Validate(env(map[string]string{"T": "t"}))

			var ve *config.ValidationError
			got := errors.As(err, &ve) && containsSub(ve.Problems, "endpoints[0].")
			if got != tt.wantErr {
				t.Fatalf("budget problem = %v, want %v (err: %v)", got, tt.wantErr, err)
			}
			// The endpoint's name, not just its index: an operator reading this has
			// a file with several endpoints in it and needs to know which one.
			if tt.wantErr && !containsSub(ve.Problems, `"monster"`) {
				t.Errorf("problem does not name the endpoint: %v", ve.Problems)
			}
		})
	}
}

// TestMissingEnvNames covers the list `kenward doctor` prints.
func TestMissingEnvNames(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(fullYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	got := cfg.MissingEnvNames(env(map[string]string{"KENWARD_BOT_TOKEN": "t"}))
	if want := []string{"OPENROUTER_API_KEY"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MissingEnvNames() = %v, want %v", got, want)
	}

	var ve *config.ValidationError
	if err := cfg.Validate(env(nil)); !errors.As(err, &ve) {
		t.Fatalf("Validate() error = %v, want *config.ValidationError", err)
	}
	if want := []string{"KENWARD_BOT_TOKEN", "OPENROUTER_API_KEY"}; !reflect.DeepEqual(ve.MissingEnv, want) {
		t.Errorf("MissingEnv = %v, want %v", ve.MissingEnv, want)
	}
}

// TestUnusedTokensAreNotRequired: a per-member token left in a simple-mode file is
// inert, and demanding it be exported would be demanding a secret nothing reads.
func TestUnusedTokensAreNotRequired(t *testing.T) {
	const doc = `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID_LEFTOVER}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`
	if _, err := config.ParseWithEnv(strings.NewReader(doc), env(map[string]string{"T": "t"})); err != nil {
		t.Fatalf("ParseWithEnv() error = %v, want a valid configuration", err)
	}
}

// TestNoSecretIsEverFormatted guards the rule that configuration holds names, not
// values: neither a rendered Config nor a validation error may contain a key.
func TestNoSecretIsEverFormatted(t *testing.T) {
	const secret = "sk-do-not-print-me"
	cfg, err := config.Decode(strings.NewReader(fullYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	lookup := env(map[string]string{"KENWARD_BOT_TOKEN": secret, "OPENROUTER_API_KEY": secret})

	rendered := fmt.Sprintf("%+v %#v", cfg, cfg)
	if strings.Contains(rendered, secret) {
		t.Error("formatting a Config exposed a secret value")
	}
	if err := cfg.Validate(lookup); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}

	// And when the variables are missing, the message names them without quoting a
	// value from anywhere.
	err = cfg.Validate(env(map[string]string{"KENWARD_BOT_TOKEN": secret}))
	if err == nil {
		t.Fatal("Validate() error = nil, want a missing-variable error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("validation error exposed a secret value: %v", err)
	}
	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("validation error does not name the missing variable: %v", err)
	}
}

func TestConversionToDomain(t *testing.T) {
	cfg, err := config.ParseWithEnv(strings.NewReader(fullYAML), fullEnv())
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}

	h := cfg.DomainHousehold()
	want := domain.Household{
		Name:        "Home",
		Shared:      "household",
		GroupChatID: -1001234567890,
		Tiers:       []string{"local", "local-slow", "cloud"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("DomainHousehold() = %+v, want %+v", h, want)
	}

	members := cfg.DomainMembers()
	if len(members) != 2 {
		t.Fatalf("DomainMembers() returned %d members", len(members))
	}
	if members[0].ID != "david" || members[0].Private != "david-private" || !members[0].Enrolled() {
		t.Errorf("members[0] = %+v", members[0])
	}
	if members[1].Enrolled() {
		t.Errorf("members[1] with telegram_id 0 reports as enrolled: %+v", members[1])
	}

	// Editing what the conversion handed back must not reach the configuration.
	members[0].Tiers[0] = "compromised"
	h.Tiers[0] = "compromised"
	if cfg.Members[0].Tiers[0] != "local" || cfg.Household.Tiers[0] != "local" {
		t.Error("conversion aliased the configuration's tier chains")
	}
}

func TestRoutingEndpoints(t *testing.T) {
	cfg, err := config.ParseWithEnv(strings.NewReader(fullYAML), fullEnv())
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}
	eps := cfg.RoutingEndpoints()
	if len(eps) != 2 {
		t.Fatalf("RoutingEndpoints() returned %d endpoints", len(eps))
	}

	want := routing.Endpoint{
		Name:    "monster",
		BaseURL: "http://monster.tail:8000/v1",
		Model:   "qwen3.6-27b-awq",
		Tags:    []string{"local", "local-slow"},
		Timeout: 120 * time.Second,
	}
	if !reflect.DeepEqual(eps[0], want) {
		t.Errorf("endpoints[0] = %+v, want %+v", eps[0], want)
	}
	if eps[1].Name != "openrouter" || eps[1].Model != "anthropic/claude-sonnet-5" {
		t.Errorf("endpoints[1] = %+v", eps[1])
	}
	// The default, not the file, is what routing must see when no timeout is written.
	if eps[1].Timeout != config.DefaultEndpointTimeout {
		t.Errorf("endpoints[1].Timeout = %v, want the default %v", eps[1].Timeout, config.DefaultEndpointTimeout)
	}

	eps[0].Tags[0] = "compromised"
	if cfg.Endpoints[0].Tags[0] != "local" {
		t.Error("RoutingEndpoints() aliased the configuration's tags")
	}
}

// TestRoutingEndpointsCarryEverythingRoutingNeeds fails when routing grows a field the
// conversion does not fill. Given an endpoint with every option written out, no field
// of the converted value should still be at its zero value; if one is, config has
// quietly stopped telling routing something routing asked for.
func TestRoutingEndpointsCarryEverythingRoutingNeeds(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(`
mode: simple
household: {shared_space: household, tiers: [cloud]}
telegram: {bot_token_env: T}
endpoints:
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    tags: [cloud]
    timeout: 90s
`))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}

	got := cfg.RoutingEndpoints()[0]
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("routing.Endpoint.%s is zero after conversion; either the conversion must fill it or routing should not have it",
				v.Type().Field(i).Name)
		}
	}
}

// TestRoutingEndpointsCarryNoCredential is the boundary this conversion exists to keep.
//
// An endpoint's key may come from a file, an environment variable, or a systemd
// credential, and which one is a question routing must not be able to answer: the
// completer is handed a resolver, resolves at the point of use, and retains nothing.
// So the assertion is an absence — routing.Endpoint carries no credential and no name
// of a place one lives, whichever source the operator chose.
//
// The absence is checked structurally as well as by value, so that re-adding a field
// like APIKeyEnv fails here rather than quietly widening what routing knows.
func TestRoutingEndpointsCarryNoCredential(t *testing.T) {
	// Every source shape at once: an environment variable, a file, and an endpoint
	// that names nothing and would be served by a systemd credential. Decode rather
	// than Parse, because this is about what conversion carries, not about whether the
	// secrets can be read here.
	const doc = `
mode: simple
household: {shared_space: household, tiers: [cloud]}
telegram: {bot_token_env: KENWARD_BOT_TOKEN}
endpoints:
  - {name: from-env, base_url: https://a.example/v1, model: m, api_key_env: OPENROUTER_API_KEY, tags: [cloud]}
  - {name: from-file, base_url: https://b.example/v1, model: m, api_key_file: /run/secrets/openrouter.key, tags: [cloud]}
  - {name: from-credential, base_url: https://c.example/v1, model: m, tags: [cloud]}
`
	cfg, err := config.Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	eps := cfg.RoutingEndpoints()
	if len(eps) != 3 {
		t.Fatalf("RoutingEndpoints() returned %d endpoints, want 3", len(eps))
	}

	// Structural: no field of routing.Endpoint may be about a credential.
	credentialish := []string{"key", "token", "secret", "credential", "password", "passphrase", "auth"}
	rt := reflect.TypeOf(routing.Endpoint{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, bad := range credentialish {
			if strings.Contains(name, bad) {
				t.Errorf("routing.Endpoint has field %s; routing must not be able to learn where a key lives, let alone hold one",
					rt.Field(i).Name)
			}
		}
	}

	// By value: neither a secret nor the name of a place one lives may survive the
	// conversion, in any rendering of it.
	rendered := fmt.Sprintf("%+v %#v", eps, eps)
	for _, leaked := range []string{
		"OPENROUTER_API_KEY",          // an environment variable name
		"/run/secrets/openrouter.key", // a file path
		"openrouter.key",
		"CREDENTIALS_DIRECTORY",
		config.EndpointAPIKeyCredential("from-credential"), // a credential name
	} {
		if leaked == "" {
			// A name systemd would reject resolves to "", and every string contains
			// the empty string. Skipping it keeps the assertion about real names.
			continue
		}
		if strings.Contains(rendered, leaked) {
			t.Errorf("the conversion carried %q into routing:\n%s", leaked, rendered)
		}
	}

	// And the endpoints did arrive, so the absences above are about a populated value
	// rather than an empty one.
	for i, want := range []string{"from-env", "from-file", "from-credential"} {
		if eps[i].Name != want || eps[i].BaseURL == "" || eps[i].Model == "" {
			t.Errorf("endpoints[%d] = %+v, want the %s endpoint carried through", i, eps[i], want)
		}
	}
}

func TestMemberLookups(t *testing.T) {
	cfg, err := config.ParseWithEnv(strings.NewReader(fullYAML), fullEnv())
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}

	if m, ok := cfg.MemberByTelegramID(12345678); !ok || m.ID != "david" {
		t.Errorf("MemberByTelegramID(12345678) = (%+v, %v)", m, ok)
	}
	if _, ok := cfg.MemberByTelegramID(0); ok {
		t.Error("MemberByTelegramID(0) matched an unclaimed member; zero must never match")
	}
	if _, ok := cfg.MemberByTelegramID(12345679); ok {
		t.Error("MemberByTelegramID() matched an adjacent id")
	}
	if m, ok := cfg.MemberByID("maria"); !ok || m.Private != "maria-private" {
		t.Errorf("MemberByID(maria) = (%+v, %v)", m, ok)
	}
	if _, ok := cfg.MemberByID("nobody"); ok {
		t.Error("MemberByID(nobody) matched")
	}
}

// TestIdleTimeoutIsOffUnlessAskedFor is the half of the session package's decision that
// takes effect at runtime. session.DefaultIdleTimeout is zero because a passphrase never
// travels over Telegram (D-019), so an expired key strands a member until somebody
// unlocks it at the machine — but this package had its own 30-minute default and
// ApplyDefaults rewrote an unset value into it, and this is the value cmd/kenward hands
// the manager. A configuration that says nothing must reach the manager saying nothing.
func TestIdleTimeoutIsOffUnlessAskedFor(t *testing.T) {
	if config.DefaultIdleTimeout != session.DefaultIdleTimeout {
		t.Errorf("config.DefaultIdleTimeout = %v, session.DefaultIdleTimeout = %v; the run path passes the former to the latter's package and they have to agree",
			config.DefaultIdleTimeout, session.DefaultIdleTimeout)
	}

	const silent = `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: TOKEN}
members:
  - {id: david, private_space: dp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`
	cfg, err := config.ParseWithEnv(strings.NewReader(silent), env(map[string]string{"TOKEN": "t"}))
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}
	if got := cfg.Session.IdleTimeout.Duration(); got != 0 {
		t.Errorf("session.idle_timeout = %v for a file that does not mention it; want 0, meaning the key stays until the process stops or the member locks it", got)
	}

	// Still configurable: a household that wants expiry says so and gets it.
	chosen, err := config.Decode(strings.NewReader("session:\n  idle_timeout: 45m\n"))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if got, want := chosen.Session.IdleTimeout.Duration(), 45*time.Minute; got != want {
		t.Errorf("session.idle_timeout = %v, want %v", got, want)
	}
}

// TestHistoryResetIsOffUnlessAskedFor is the compatibility guarantee for the scheduled
// conversation reset, and the assertion that it is a separate thing from the setting it
// most resembles.
//
// A file written before history.reset_every existed has to keep behaving as it did: a
// conversation that runs until the node restarts. That means zero, and it means zero
// surviving ApplyDefaults rather than being rewritten into "a sensible daily reset",
// which is the exact mistake session.idle_timeout had to be rescued from.
func TestHistoryResetIsOffUnlessAskedFor(t *testing.T) {
	const silent = `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: TOKEN}
members:
  - {id: david, private_space: dp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`
	cfg, err := config.ParseWithEnv(strings.NewReader(silent), env(map[string]string{"TOKEN": "t"}))
	if err != nil {
		t.Fatalf("ParseWithEnv() error: %v", err)
	}
	if got := cfg.History.ResetEvery.Duration(); got != 0 {
		t.Errorf("history.reset_every = %v for a file that does not mention it; want 0, meaning a conversation runs until the node restarts", got)
	}
	if config.DefaultHistoryReset != 0 {
		t.Errorf("config.DefaultHistoryReset = %v, want 0: an existing household's conversations may not start being cleared because it upgraded", config.DefaultHistoryReset)
	}

	// It is not session.idle_timeout, and setting one must not move the other. They
	// are both durations, both off by default, and adjacent in the file; the only
	// thing keeping them apart is that they are separate fields, so that is what is
	// asserted.
	chosen, err := config.Decode(strings.NewReader("history:\n  reset_every: 6h\n"))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if got, want := chosen.History.ResetEvery.Duration(), 6*time.Hour; got != want {
		t.Errorf("history.reset_every = %v, want %v", got, want)
	}
	if got := chosen.Session.IdleTimeout.Duration(); got != 0 {
		t.Errorf("asking for a conversation reset also set session.idle_timeout to %v, which would stop the member's assistant answering", got)
	}
}

// TestHistoryResetLongerThanADayIsRefused holds the anchor honest.
//
// Boundaries are counted from local midnight, so an interval longer than a day cannot
// be kept: the node would reset once per midnight while the file said 48h. Refusing is
// the only outcome that does not leave a file claiming something untrue about the
// running system.
func TestHistoryResetLongerThanADayIsRefused(t *testing.T) {
	base := `
mode: simple
household: {shared_space: household, tiers: [local]}
telegram: {bot_token_env: TOKEN}
members:
  - {id: david, private_space: dp, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
history: {reset_every: %s}
`
	lookup := env(map[string]string{"TOKEN": "t"})

	if _, err := config.ParseWithEnv(strings.NewReader(fmt.Sprintf(base, "24h")), lookup); err != nil {
		t.Errorf("24h is the longest honourable interval and must load: %v", err)
	}

	_, err := config.ParseWithEnv(strings.NewReader(fmt.Sprintf(base, "48h")), lookup)
	if err == nil {
		t.Fatal("48h loaded; it cannot be honoured, because boundaries are counted from midnight")
	}
	if !strings.Contains(err.Error(), "history.reset_every") {
		t.Errorf("the error does not name the key an operator has to edit: %v", err)
	}
}

// TestMemberByTelegramIDIsFailClosed: two members bound to one Telegram account is a
// configuration Load rejects, but this lookup is what scope.Resolve authorises with, and
// Resolve is documented as being handed configurations nobody validated — a Config built
// in Go by a test, a tool, or the next package that finds it convenient. Answering the
// first of the two would write one person's messages into the other's private space.
func TestMemberByTelegramIDIsFailClosed(t *testing.T) {
	cfg := &config.Config{
		Mode:      config.ModeSimple,
		Household: config.HouseholdConfig{SharedSpace: "household"},
		Members: []config.MemberConfig{
			{ID: "david", TelegramID: 7, PrivateSpace: "dp"},
			{ID: "maria", TelegramID: 7, PrivateSpace: "mp"},
			{ID: "sam", TelegramID: 9, PrivateSpace: "sp"},
		},
	}

	if m, ok := cfg.MemberByTelegramID(7); ok {
		t.Errorf("MemberByTelegramID(7) = (%+v, true); an ambiguous binding must resolve to nobody", m)
	}
	// The rest of the household is unaffected: one bad row is not an outage.
	if m, ok := cfg.MemberByTelegramID(9); !ok || m.ID != "sam" {
		t.Errorf("MemberByTelegramID(9) = (%+v, %v), want sam", m, ok)
	}
}

func TestEmptyDocument(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Decode(\"\") error = %v, want an empty configuration", err)
	}
	if cfg.Memory.SearchLimit != config.DefaultSearchLimit {
		t.Errorf("defaults were not applied to an empty document: %+v", cfg.Memory)
	}
	if err := cfg.Validate(env(nil)); err == nil {
		t.Error("Validate() on an empty configuration = nil, want problems")
	}
}

func TestSyntaxErrorIsNotAValidationError(t *testing.T) {
	_, err := config.Decode(strings.NewReader("mode: [unclosed\n"))
	if err == nil {
		t.Fatal("Decode() error = nil, want a syntax error")
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		t.Errorf("a syntax error was reported as a *ValidationError: %v", err)
	}
}

// withDataDir points a fixture at a scratch data directory, so no test reads or writes
// the state file of whoever is running them.
func withDataDir(doc, dir string) string {
	return doc + "\ndata_dir: " + strconv.Quote(dir) + "\n"
}

func TestDataDir(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(fullYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if cfg.DataDir == "" {
		t.Error("DataDir was left empty; ApplyDefaults must give it a per-OS location")
	}
	if got, want := cfg.StatePath(), filepath.Join(config.DefaultDataDir(), config.StateFileName); got != want {
		t.Errorf("StatePath() = %q, want %q", got, want)
	}

	explicit, err := config.Decode(strings.NewReader(withDataDir(fullYAML, "/var/lib/kenward")))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if explicit.DataDir != "/var/lib/kenward" {
		t.Errorf("DataDir = %q, want the value from the file", explicit.DataDir)
	}
	if got, want := explicit.StatePath(), filepath.Join("/var/lib/kenward", "state.json"); got != want {
		t.Errorf("StatePath() = %q, want %q", got, want)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(withDataDir(fullYAML, dir)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := config.LoadWithEnv(path, fullEnv())
	if err != nil {
		t.Fatalf("LoadWithEnv() error: %v", err)
	}
	if cfg.Household.Name != "Home" {
		t.Errorf("Household.Name = %q", cfg.Household.Name)
	}

	if _, err := config.LoadWithEnv(filepath.Join(dir, "absent.yaml"), fullEnv()); err == nil {
		t.Error("LoadWithEnv() on a missing file = nil, want an error")
	} else if !strings.Contains(err.Error(), "absent.yaml") {
		t.Errorf("error does not name the file: %v", err)
	}

	// A load failure must still name the file it was reading.
	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte(withDataDir("mode: simple\n", dir)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	err = func() error { _, err := config.LoadWithEnv(badPath, env(nil)); return err }()
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error does not name the file: %v", err)
	}
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("wrapped load error lost the *ValidationError: %v", err)
	}
}

// TestValidateWithNilLookupUsesTheProcessEnvironment is the one test that reads the real
// environment, and it does so only to prove the nil fallback is wired.
func TestValidateWithNilLookupUsesTheProcessEnvironment(t *testing.T) {
	t.Setenv("KENWARD_TEST_TOKEN", "value")
	cfg := &config.Config{
		Mode:      config.ModeSimple,
		Household: config.HouseholdConfig{SharedSpace: "household", Tiers: []string{"local"}},
		Telegram:  config.TelegramConfig{BotTokenEnv: "KENWARD_TEST_TOKEN"},
		Endpoints: []config.EndpointConfig{{Name: "m", BaseURL: "http://m:1/v1", Model: "q", Tags: []string{"local"}}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(nil); err != nil {
		t.Fatalf("Validate(nil) error: %v", err)
	}

	cfg.Telegram.BotTokenEnv = "KENWARD_TEST_TOKEN_THAT_IS_NOT_SET"
	if err := cfg.Validate(nil); err == nil {
		t.Error("Validate(nil) = nil for an unset variable")
	}
}

// TestPrivateWritesPolicy covers capture.private_writes: what it defaults to, what
// it accepts, and what it says about anything else.
//
// The default is the load-bearing half. It decides whether a note to a member's own
// memory is written before they see it, so an unset key falling through to the wrong
// side is not a cosmetic default — and it is the value most households will run,
// because most will never write the key at all.
func TestPrivateWritesPolicy(t *testing.T) {
	t.Run("unset means save", func(t *testing.T) {
		var c config.Config
		c.ApplyDefaults()
		if c.Capture.PrivateWrites != config.PrivateWriteSave {
			t.Errorf("private_writes defaulted to %q, want %q", c.Capture.PrivateWrites, config.PrivateWriteSave)
		}
	})

	t.Run("ask survives defaulting", func(t *testing.T) {
		var c config.Config
		c.Capture.PrivateWrites = config.PrivateWriteAsk
		c.ApplyDefaults()
		if c.Capture.PrivateWrites != config.PrivateWriteAsk {
			t.Errorf("private_writes = %q after defaulting, want %q; a household's opt-out was overwritten",
				c.Capture.PrivateWrites, config.PrivateWriteAsk)
		}
	})

	t.Run("anything else is a validation error naming both policies", func(t *testing.T) {
		doc := strings.Replace(fullYAML,
			"  max_proposals_per_turn: 1",
			"  max_proposals_per_turn: 1\n  private_writes: never", 1)
		_, err := config.ParseWithEnv(strings.NewReader(doc), fullEnv())
		if err == nil {
			t.Fatal("an unrecognised policy loaded cleanly; it would fall through to writing without asking")
		}
		for _, want := range []string{"capture.private_writes", "save", "ask"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not mention %q: %v", want, err)
			}
		}
	})
}

// TestAnnounceReadsDefaultsToOn. A bare bool cannot tell "not stated" from "stated
// false", and the default here is true, so the field is a pointer and the accessor is
// the only correct way to read it. This is the test that keeps someone from
// simplifying it back into a bool and silently turning the line off for every
// household that never mentioned it.
func TestAnnounceReadsDefaultsToOn(t *testing.T) {
	cases := []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset", nil, true},
		{"true", ptrTo(true), true},
		{"false", ptrTo(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c config.Config
			c.Memory.AnnounceReads = tc.set
			c.ApplyDefaults()
			if got := c.Memory.AnnouncesReads(); got != tc.want {
				t.Errorf("AnnouncesReads() = %v, want %v", got, tc.want)
			}
			// Defaulting must not rewrite the absence into a pointer: the
			// accessor is where the default lives, and two places holding it is
			// how the two disagree.
			if tc.set == nil && c.Memory.AnnounceReads != nil {
				t.Error("ApplyDefaults materialised announce_reads; the absence has to stay an absence")
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }
