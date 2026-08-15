package config_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

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
	if len(cfg.Members) != 2 {
		t.Fatalf("len(Members) = %d, want 2", len(cfg.Members))
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

	// api_key_env is documented in config.go as "Empty for endpoints that need no
	// authentication, which is the usual case for a machine on the household's own
	// network" — monster and battlestation are exactly that case. Requiring a value
	// here would misrepresent the common local-endpoint shape as needing a key it does
	// not.
	"Endpoints[0].APIKeyEnv": "monster is a local endpoint with no key in front of it",
	"Endpoints[1].APIKeyEnv": "battlestation is a local endpoint with no key in front of it",
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

func addZero(out *[]string, path string) {
	if _, allowed := allowedZeroFields[path]; allowed {
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
