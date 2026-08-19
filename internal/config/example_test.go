package config_test

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// examplePath is kenward.example.yaml, two directories up from this package. It is the
// configuration a new operator copies and edits, so it must always parse and validate
// cleanly: an example that has drifted out of validity is worse than no example at all,
// because someone will copy it anyway.
const examplePath = "../../kenward.example.yaml"

// exampleEnv supplies every environment variable kenward.example.yaml references. Only
// the household's shared bot token and the OpenRouter API key are needed: the file is in
// simple mode, so the members' own bot_token_env values are documented but unused, and
// the two local endpoints deliberately carry no api_key_env at all.
func exampleEnv() config.LookupEnvFunc {
	vars := map[string]string{
		"KENWARD_BOT_TOKEN":  "123456789:AAExampleTokenNotARealSecret",
		"OPENROUTER_API_KEY": "sk-or-v1-example-not-a-real-secret",
		// The example names session.passphrase_env, and a named source is checked at
		// load — that being the whole reason to name one. Leaving it out here would
		// make this test assert the example is broken.
		"KENWARD_PASSPHRASE": "correct horse battery staple",
	}
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// loadExample loads kenward.example.yaml through the real LoadWithEnv path — the same
// load, merge-state, validate sequence a running node uses — with an injected
// environment so the test never touches the real process environment.
func loadExample(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadWithEnv(examplePath, exampleEnv())
	if err != nil {
		t.Fatalf("LoadWithEnv(%s) error: %v", examplePath, err)
	}
	return cfg
}

// TestExampleParsesAndValidates is the point of this file: a schema change that the
// example has not been updated for must fail the build loudly, here, rather than
// surfacing later when an operator copies a stale file.
func TestExampleParsesAndValidates(t *testing.T) {
	cfg := loadExample(t)

	if cfg.Mode != config.ModeSimple {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeSimple)
	}
	if len(cfg.Members) != 3 {
		t.Fatalf("len(Members) = %d, want 3", len(cfg.Members))
	}
	// The third is the point of being three: a household with only full members
	// would document shared_only nowhere, in the one file every household copies.
	if !cfg.Members[2].SharedOnly {
		t.Errorf("members[2] (%s) is not shared_only; the example must demonstrate the member with no memory of their own", cfg.Members[2].ID)
	}
	if cfg.Members[2].PrivateSpace != "" {
		t.Errorf("members[2].private_space = %q, want empty: a shared_only member has none", cfg.Members[2].PrivateSpace)
	}
	if len(cfg.Endpoints) != 3 {
		t.Fatalf("len(Endpoints) = %d, want 3", len(cfg.Endpoints))
	}

	// Re-validating an already-loaded, already-defaulted configuration must still find
	// nothing wrong; this is the "zero problems" assertion the task calls for, stated
	// directly rather than only inferred from Load having returned no error.
	if err := cfg.Validate(exampleEnv()); err != nil {
		t.Fatalf("Validate() on the loaded example: %v", err)
	}
}

// TestExampleDemonstratesTierAsymmetry pins down the one thing this file exists to show:
// david's private tier chain names only local tags and so never reaches a provider,
// while jordan's does. If this ever stops being true the example has stopped
// demonstrating the product's core privacy asymmetry, silently.
func TestExampleDemonstratesTierAsymmetry(t *testing.T) {
	cfg := loadExample(t)

	tags := make(map[string]bool)
	for _, e := range cfg.Endpoints {
		for _, tag := range e.Tags {
			tags[tag] = true
		}
	}
	localOnly := func(chain []string) bool {
		for _, t := range chain {
			if t == "cloud" {
				return false
			}
		}
		return len(chain) > 0
	}

	david, ok := cfg.MemberByID("david")
	if !ok {
		t.Fatal("member david not found")
	}
	if !localOnly(david.Tiers) {
		t.Errorf("david.Tiers = %v, want a local-only chain (no cloud tag)", david.Tiers)
	}

	jordan, ok := cfg.MemberByID("jordan")
	if !ok {
		t.Fatal("member jordan not found")
	}
	reachesCloud := false
	for _, tier := range jordan.Tiers {
		if tier == "cloud" {
			reachesCloud = true
		}
	}
	if !reachesCloud {
		t.Errorf("jordan.Tiers = %v, want a chain that reaches the cloud tier", jordan.Tiers)
	}
}

// TestExampleStatesEveryDefaultedKey closes the one hole TestExampleExercisesEveryField
// cannot see.
//
// That test reflects over a *loaded* configuration, and loading applies defaults — so a
// field with a default comes back non-zero whether the example set it or not, and the
// example could quietly drop the key without anything failing. The keys below all have
// defaults, and the example is meant to spell them out rather than rely on them: this is
// the file an operator copies to learn what the knobs are, and a knob it never mentions
// is a knob they will not know exists.
//
// It reads the YAML directly rather than through Load, because the whole point is to see
// what the file says before defaulting has had a chance to speak for it.
func TestExampleStatesEveryDefaultedKey(t *testing.T) {
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("reading %s: %v", examplePath, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parsing %s: %v", examplePath, err)
	}

	for _, path := range []string{
		"data_dir",
		"household.agents",
		"memory.search_limit",
		"history.reset_every",
		"session.idle_timeout",
		"capture.max_proposals_per_turn",
		"update.channel",
		"update.check_interval",
		"reminders.max_per_day",
		"reminders.catch_up_window",
		"reminders.max_stored",
	} {
		if v, ok := lookupYAMLPath(raw, path); !ok || isEmptyYAMLValue(v) {
			t.Errorf("kenward.example.yaml does not state %s; it has a default, so leaving it out still loads — "+
				"but the example is what an operator reads to learn the key exists", path)
		}
	}

	// Endpoint timeouts, windows and completion caps default too, and are
	// per-endpoint rather than a single key. The window and the cap especially:
	// an operator who never learns those keys exist runs a machine bought for the
	// size of its window at 16384 tokens of it, and never finds out.
	endpoints, ok := raw["endpoints"].([]any)
	if !ok || len(endpoints) == 0 {
		t.Fatalf("kenward.example.yaml has no endpoints list")
	}
	for i, e := range endpoints {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("endpoints[%d] is not a mapping", i)
		}
		for _, key := range []string{"timeout", "context_window", "max_completion_tokens"} {
			if v, ok := m[key]; !ok || isEmptyYAMLValue(v) {
				t.Errorf("kenward.example.yaml endpoints[%d] does not state %s; it defaults, so the example must show it", i, key)
			}
		}
	}
}

// lookupYAMLPath walks a dotted path through a decoded YAML mapping.
func lookupYAMLPath(root map[string]any, path string) (any, bool) {
	var cur any = root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// isEmptyYAMLValue reports whether a decoded value is present but says nothing — a null,
// an empty string or an empty list. Stating a key and leaving it blank is not stating it.
func isEmptyYAMLValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// allowedZeroFields lists the exact field paths that are legitimately still at their
// zero value after loading the example, each with the reason it is not an omission.
//
// Anything not on this list that comes back zero from TestExampleExercisesEveryField is
// a field the example forgot to demonstrate, and the schema-drift test below is
// deliberately unforgiving about that: allowing a field silently would let the file rot
// exactly the way this whole exercise exists to prevent.
var allowedZeroFields = map[string]string{
	// EnrolledAt is tagged `yaml:"-"`: it can never be set from the YAML itself, only
	// filled in by MergeState from a recorded binding in state.json. This example has
	// no state.json (a fresh household that has never enrolled anyone looks exactly
	// like this), so it is genuinely zero for both members after a real Load — not a
	// gap in the example, but the correct value for "nobody has redeemed a state
	// binding on this machine yet."
	"Members[0].EnrolledAt": "yaml:\"-\"; only MergeState from state.json sets it, and this example has none",
	"Members[1].EnrolledAt": "yaml:\"-\"; only MergeState from state.json sets it, and this example has none",
	"Members[2].EnrolledAt": "yaml:\"-\"; only MergeState from state.json sets it, and this example has none",

	// david and jordan are full members, which is what shared_only's zero value
	// means and what almost every row in almost every household will be. leo is the
	// example of the other kind, and demonstrating it on a member who also has a
	// private space is not possible: the two together are a validation error, and
	// deliberately, because it is the pair that would otherwise decide somebody's
	// privacy by which line the loader read last.
	"Members[0].SharedOnly": "david has a private space; shared_only and private_space together are a validation error",
	"Members[1].SharedOnly": "jordan has a private space; shared_only and private_space together are a validation error",

	// Everything leo does not have, which is the whole content of the example. Each
	// of these is a validation error beside shared_only rather than an omission, so
	// there is no value that could be put here instead — see validateMembers.
	"Members[2].PrivateSpace":      "leo is shared_only: they have no private memory, and naming one here is a validation error",
	"Members[2].Tiers":             "leo is shared_only: their conversations are the household's and run on household.tiers",
	"Members[2].BotTokenEnv":       "leo is shared_only: no assistant of their own, so no bot and no pod",
	"Members[2].BotTokenFile":      "leo is shared_only: no assistant of their own, so no bot and no pod",
	"Members[2].PassphraseEnv":     "leo is shared_only: nothing of theirs is stored anywhere, so there is no key to wrap",
	"Members[2].PassphraseFile":    "leo is shared_only: nothing of theirs is stored anywhere, so there is no key to wrap",
	"Members[2].Persona.AgentName": "leo has no agent of their own to name; config.PersonaFor gives them the household's voice",
	"Members[2].Persona.Tone":      "leo has no agent of their own; the household's tone is the one that answers them",
	"Members[2].Persona.Character": "leo has no agent of their own; the household's character is the one that answers them",

	// kenward's own name is not a setting, and household.persona.agent_name is the
	// one persona field that is a validation error rather than a choice: the name is
	// what this product's documentation, its logs and `kenward doctor` all say, and a
	// household that renamed it would have renamed the subject of all of them. Setting
	// it here to satisfy this test would ship an example that fails its own validation.
	// The field a member actually wants is members[].persona.agent_name, and both
	// members demonstrate it.
	"Household.Persona.AgentName": "kenward's name is not configurable; config.Validate refuses a value here",

	// idle_timeout's default is 0 — idle expiry off — and the example states it as
	// "0s" rather than dropping the key, because a knob the example never mentions is
	// a knob nobody knows exists. Setting a non-zero duration here to satisfy this test
	// would be worse than the gap it closes: it would put a value in the file every
	// household copies that stops a member's assistant answering with no in-band way
	// back (D-019), which is exactly why the default moved to off.
	"Session.IdleTimeout": "0s is the default and the correct value; the example states the key and explains the trade in a comment",

	// history.reset_every is off by default for the same reason it is stated anyway:
	// the example is where a household learns the key exists, and putting a live
	// schedule in the file everybody copies would change what every new household's
	// assistant appears to know mid-week without anyone having asked for it.
	"History.ResetEvery": "0s is the default and the correct value; the example states the key and explains what it does and does not clear",

	// api_key_env is documented in config.go as "Empty for endpoints that need no
	// authentication, which is the usual case for a machine on the household's own
	// network" — monster and battlestation are exactly that case. Requiring a value
	// here would misrepresent the common local-endpoint shape as needing a key it does
	// not.
	"Endpoints[0].APIKeyEnv": "monster is a local endpoint with no key in front of it",
	"Endpoints[1].APIKeyEnv": "battlestation is a local endpoint with no key in front of it",

	// The *_file forms are the alternative source for the same secrets the *_env
	// fields above name, and exactly one source per secret is permitted (see
	// secret.go). Demonstrating both in one example is not "more complete", it is the
	// configuration this package refuses to load; and the example cannot demonstrate
	// the file form on its own either, because the path would have to exist, with mode
	// 0600, on whatever machine runs the tests. The example shows the variable form
	// and documents the other two in comments.
	"Telegram.BotTokenFile":     "bot_token_env is the source here; stating both is a validation error",
	"Members[0].BotTokenFile":   "the file form is an alternative to bot_token_env, not an addition to it",
	"Members[1].BotTokenFile":   "the file form is an alternative to bot_token_env, not an addition to it",
	"Members[0].PassphraseFile": "the file form is an alternative to passphrase_env, not an addition to it",
	"Members[1].PassphraseFile": "the file form is an alternative to passphrase_env, not an addition to it",
	"Session.PassphraseFile":    "the file form is an alternative to session.passphrase_env, not an addition to it",
	"Endpoints[0].APIKeyFile":   "monster is a local endpoint with no key in front of it",
	"Endpoints[1].APIKeyFile":   "battlestation is a local endpoint with no key in front of it",
	"Endpoints[2].APIKeyFile":   "api_key_env is the source here; stating both is a validation error",

	// The dashboard's certificate pair is generated at the moment LAN exposure is
	// chosen, and this example is a loopback dashboard. Putting paths here to satisfy
	// this test would mean shipping an example whose two most consequential lines
	// point at files that do not exist — and, worse, would model TLS as something a
	// household configures by hand when the whole design is that it is generated and
	// its fingerprint shown once. The keys are documented in a comment in the example.
	"Dashboard.TLSCertFile": "generated when LAN exposure is chosen; this example is loopback and needs none",
	"Dashboard.TLSKeyFile":  "generated when LAN exposure is chosen; this example is loopback and needs none",
}

// TestExampleExercisesEveryField reflects over the loaded configuration and fails if any
// field is still at its zero value, other than the explicitly allowed exceptions above.
//
// This is what keeps kenward.example.yaml complete as the schema grows: add a field to
// any Config struct and forget to give it a value here, and this test names exactly
// which field was missed instead of the example silently going stale.
func TestExampleExercisesEveryField(t *testing.T) {
	cfg := loadExample(t)

	var zero []string
	collectZeroFields(reflect.ValueOf(*cfg), "", &zero)

	if len(zero) > 0 {
		t.Fatalf("kenward.example.yaml leaves these fields at their zero value:\n  - %s\n"+
			"either set a real value in kenward.example.yaml, or add the field to allowedZeroFields "+
			"in example_test.go with a comment explaining why zero is the correct value",
			joinLines(zero))
	}
}

// collectZeroFields walks every exported field reachable from v — recursing through
// structs and every element of a slice — and appends the dotted/indexed path of any
// field still at its zero value, unless that path is listed in allowedZeroFields.
//
// time.Time is treated as a leaf via its own IsZero, rather than recursed into: all of
// its fields are unexported, so recursing would find nothing to check and EnrolledAt
// would never be reported no matter its value.
func collectZeroFields(v reflect.Value, path string, out *[]string) {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		if v.Interface().(time.Time).IsZero() {
			addZero(out, path)
		}
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fieldPath := f.Name
			if path != "" {
				fieldPath = path + "." + f.Name
			}
			collectZeroFields(v.Field(i), fieldPath, out)
		}

	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			addZero(out, path+" (empty)")
			return
		}
		for i := 0; i < v.Len(); i++ {
			collectZeroFields(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}

	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			addZero(out, path+" (nil)")
			return
		}
		collectZeroFields(v.Elem(), path, out)

	default:
		if v.IsZero() {
			addZero(out, path)
		}
	}
}

// addZero records a zero-valued field unless it is allow-listed.
//
// The lookup is on the stripped path, the same form TestAllowedZeroFieldsAreReal
// compares against. Without that the two disagreed for every non-scalar: an empty
// slice is recorded as "X (empty)", so an entry spelled "X" never suppressed it while
// an entry spelled "X (empty)" was reported as naming no real field. Nothing in the
// allow-list was a slice or a pointer until now, so the disagreement had never been
// reached.
func addZero(out *[]string, path string) {
	if _, allowed := allowedZeroFields[strippedPath(path)]; allowed {
		return
	}
	*out = append(*out, path)
}

func joinLines(lines []string) string {
	s := ""
	for i, l := range lines {
		if i > 0 {
			s += "\n  - "
		}
		s += l
	}
	return s
}

// TestAllowedZeroFieldsAreReal guards the allow-list itself: every path in it must name
// a field that actually exists and actually is zero right now. Otherwise a future schema
// change could rename or remove one of these fields and the allow-list would keep
// silently permitting a path that no longer means anything, which defeats the point of
// naming exceptions explicitly.
func TestAllowedZeroFieldsAreReal(t *testing.T) {
	cfg := loadExample(t)

	var zero []string
	collectZeroFieldsIgnoringAllowlist(reflect.ValueOf(*cfg), "", &zero)

	seen := make(map[string]bool, len(zero))
	for _, z := range zero {
		seen[strippedPath(z)] = true
	}
	for path := range allowedZeroFields {
		if !seen[path] {
			t.Errorf("allowedZeroFields contains %q, but it is not an actual zero-valued field of the loaded example; remove it or fix the path", path)
		}
	}
}

// collectZeroFieldsIgnoringAllowlist is collectZeroFields without consulting
// allowedZeroFields, used only to check the allow-list is still accurate.
func collectZeroFieldsIgnoringAllowlist(v reflect.Value, path string, out *[]string) {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		if v.Interface().(time.Time).IsZero() {
			*out = append(*out, path)
		}
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fieldPath := f.Name
			if path != "" {
				fieldPath = path + "." + f.Name
			}
			collectZeroFieldsIgnoringAllowlist(v.Field(i), fieldPath, out)
		}

	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			*out = append(*out, path+" (empty)")
			return
		}
		for i := 0; i < v.Len(); i++ {
			collectZeroFieldsIgnoringAllowlist(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}

	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			*out = append(*out, path+" (nil)")
			return
		}
		collectZeroFieldsIgnoringAllowlist(v.Elem(), path, out)

	default:
		if v.IsZero() {
			*out = append(*out, path)
		}
	}
}

// strippedPath drops the " (empty)"/" (nil)" suffix collectZeroFields* adds for
// non-scalar zero values, so it can be compared against allowedZeroFields' plain paths.
func strippedPath(p string) string {
	for _, suffix := range []string{" (empty)", " (nil)"} {
		if len(p) > len(suffix) && p[len(p)-len(suffix):] == suffix {
			return p[:len(p)-len(suffix)]
		}
	}
	return p
}
