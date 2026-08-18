package assistant

import (
	"encoding/json"
	"testing"

	"github.com/BlueHeisenberg/lore"
)

// TestRememberSchemaConfidenceMatchesLore ties the confidence enum the model is sent to
// lore's own vocabulary.
//
// rememberSchema has to stay a verbatim JSON literal — promptdoc_test.go compares it to
// docs/PROMPT.md character for character, and that is the more valuable of the two
// contracts — so the vocabulary cannot be built from lore's constants at compile time.
// What it can be is checked, which is the whole difference between two literals that
// happen to agree and two that are known to.
//
// lore.Confidence is closed: a value outside it is ErrInvalidArgument, not a free-form
// tag. So a model told about a fifth value would emit one that lore refuses at the write,
// after the member has already been shown a proposal — and a model *not* told about a
// value lore has gained would simply never produce it, which is the silent half and the
// reason this checks both directions. Neither is visible by reading either file.
func TestRememberSchemaConfidenceMatchesLore(t *testing.T) {
	t.Parallel()

	var schema struct {
		Properties struct {
			Confidence struct {
				Enum []string `json:"enum"`
			} `json:"confidence"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rememberSchema), &schema); err != nil {
		t.Fatalf("rememberSchema is not valid JSON: %v", err)
	}

	got := schema.Properties.Confidence.Enum
	want := []lore.Confidence{lore.Experimental, lore.Provisional, lore.Validated, lore.Hardened}
	if len(got) != len(want) {
		t.Fatalf("rememberSchema offers %d confidence values, lore has %d: %v vs %v", len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != string(w) {
			t.Errorf("rememberSchema confidence[%d] = %q, lore's is %q — the schema is docs/PROMPT.md's, so fix the document and the constant together", i, got[i], w)
		}
	}

	// The other direction, and the one a length check alone would miss if lore ever
	// renamed a value rather than adding one: every value offered must be one lore
	// will actually accept.
	for _, v := range got {
		if !lore.Confidence(v).Valid() {
			t.Errorf("rememberSchema offers confidence %q, which lore rejects as ErrInvalidArgument", v)
		}
	}
}
