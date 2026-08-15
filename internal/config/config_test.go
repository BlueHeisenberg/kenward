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
  lore_command: ["lore", "mcp"]
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
	if got, want := cfg.Memory.LoreCommand, []string{"lore", "mcp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("memory.lore_command = %v, want %v", got, want)
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
			// Zero is replaced by the default, so an explicit empty reads as unset.
			want := tt.want
			if want == 0 {
				want = config.DefaultIdleTimeout
			}
			if got := cfg.Session.IdleTimeout.Duration(); got != want {
				t.Errorf("idle_timeout = %v, want %v", got, want)
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
			name: "isolated mode needs no household token",
			yaml: `
mode: isolated
household: {shared_space: household, tiers: [local]}
members:
  - {id: david, private_space: dp, tiers: [local], bot_token_env: T_DAVID}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`,
			env:  map[string]string{"T_DAVID": "t"},
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
	if eps[0].Name != "monster" || eps[0].Timeout != 120*time.Second {
		t.Errorf("endpoints[0] = %+v", eps[0])
	}
	if eps[1].APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Errorf("endpoints[1].APIKeyEnv = %q", eps[1].APIKeyEnv)
	}
	if got, want := eps[0].Tags, []string{"local", "local-slow"}; !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints[0].Tags = %v, want %v", got, want)
	}

	eps[0].Tags[0] = "compromised"
	if cfg.Endpoints[0].Tags[0] != "local" {
		t.Error("RoutingEndpoints() aliased the configuration's tags")
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
