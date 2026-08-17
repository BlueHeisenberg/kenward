package main

import (
	"os"
	"strings"
	"testing"
)

// TestIsolatedComposeSaysWhichBlockEachSecretGoesIn.
//
// The first instruction in deploy/compose.isolated.yml told the operator to write "one
// `bot_token_env` AND one `passphrase_env` per member, plus one `bot_token_env` for the
// household group". Read literally beside "per member", the group's one goes under
// `household:`, and kenward answers:
//
//	kenward: config: parsing yaml: yaml: unmarshal errors:
//	  line 9: field bot_token_env not found in type config.HouseholdConfig
//
// which names a Go type nobody has and no block anybody can find. It belongs under the
// top-level `telegram:` block. This was the first thing the file asks for and it cost a
// tester two failed starts.
func TestIsolatedComposeSaysWhichBlockEachSecretGoesIn(t *testing.T) {
	const path = "../../deploy/compose.isolated.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	step := stepOne(t, string(b))
	for _, want := range []string{"telegram:", "members:"} {
		if !strings.Contains(step, want) {
			t.Errorf("%s step 1 does not say the secrets go under %s:\n%s", path, want, step)
		}
	}
	// household: is where the wrong answer puts the group's token. If step 1 names
	// that block at all it must be to say the token is not there.
	if strings.Contains(step, "household:") && !strings.Contains(strings.ToLower(step), "not under") {
		t.Errorf("%s step 1 mentions household: without saying the group's token is not there:\n%s", path, step)
	}
}

// stepOne returns the "1." item of the compose file's "Before running:" list.
func stepOne(t *testing.T, text string) string {
	t.Helper()
	const start = "#   1. "
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatalf("no numbered instructions in this file; this test has stopped checking anything")
	}
	rest := text[i:]
	if j := strings.Index(rest, "#   2."); j > 0 {
		rest = rest[:j]
	}
	return rest
}
