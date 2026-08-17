package config

import (
	"strings"
	"testing"
)

// TestAMisplacedKeyIsToldWhereItBelongs.
//
// yaml.v3's strict decoder reports "field bot_token_env not found in type
// config.HouseholdConfig". Every word of that is true and none of it helps: the operator
// does not have a config.HouseholdConfig, they have a `household:` block, and the key
// they wrote is a real key that belongs somewhere else in the same file. This is the
// error deploy/compose.isolated.yml's first instruction used to produce.
func TestAMisplacedKeyIsToldWhereItBelongs(t *testing.T) {
	const doc = `
mode: isolated
household:
  name: Casa
  shared_space: 00000000-0000-4000-8000-000000000001
  bot_token_env: KENWARD_BOT_TOKEN_HOUSEHOLD
`
	_, err := Decode(strings.NewReader(doc))
	if err == nil {
		t.Fatal("a bot_token_env under household: was accepted")
	}
	if !strings.Contains(err.Error(), "telegram:") {
		t.Errorf("the error does not say which block bot_token_env belongs in:\n%v", err)
	}
}

// TestAKeyThatIsNowhereGetsNoInventedAdvice: a genuine typo has no home to be sent to,
// and guessing one would be worse than the plain refusal.
func TestAKeyThatIsNowhereGetsNoInventedAdvice(t *testing.T) {
	const doc = `
mode: simple
household:
  nmae: Casa
`
	_, err := Decode(strings.NewReader(doc))
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if strings.Contains(err.Error(), "belongs under") {
		t.Errorf("a typo was told it belongs somewhere:\n%v", err)
	}
}
