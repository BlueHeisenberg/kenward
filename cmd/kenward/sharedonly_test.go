package main

import (
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
)

// sharedOnlyYAML is simpleYAML with a member who has no memory of their own.
var sharedOnlyYAML = strings.Replace(simpleYAML,
	"endpoints:\n",
	`  - id: leo
    name: Leo
    telegram_id: 11223344
    shared_only: true
endpoints:
`, 1)

// TestDoctorQualifiesThePrivacyStatementForASharedOnlyMember.
//
// The statement doctor prints opens by promising every member a memory of their own,
// and this household has somebody it is not promised to. An operator reading the whole
// block and coming away believing every member here has a private space would be
// believing it on kenward's own word, in the one document written to be checked — and
// they are the person a member would ask.
//
// Only when there is such a member: a paragraph about a kind of member nobody here is
// would be noise in the block the reader has to finish, which is the same rule the
// own-bot note follows.
func TestDoctorQualifiesThePrivacyStatementForASharedOnlyMember(t *testing.T) {
	t.Parallel()

	if sharedOnlyYAML == simpleYAML {
		t.Fatal("the fixture no longer has the shape this test edits")
	}

	const marker = "Not everyone here has a memory of their own"

	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"a household with one", sharedOnlyYAML, true},
		{"a household with none", simpleYAML, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			if code := h.run("doctor"); code != exitOK {
				t.Fatalf("exit = %d, want 0\n%s", code, h.stderr())
			}
			out := h.stdout()
			if got := strings.Contains(out, marker); got != tc.want {
				t.Errorf("statement mentions the shared_only note = %v, want %v:\n%s", got, tc.want, out)
			}
			if !tc.want {
				return
			}
			// The three facts it has to carry. Not a golden test — internal/privacy
			// owns the wording — but the claims must be present wherever the words
			// end up.
			for _, want := range []string{
				"goes to the household's shared memory",
				"private conversation, and it is not a private memory",
				"nothing is written until they say yes",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the note does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestRevocationWarningIsTrueForASharedOnlyMember.
//
// The warning names the member's lore space and says its key has not been rotated. For
// somebody who never had one it rendered `Their lore space "" has NOT been re-keyed`,
// which is a security warning about a space that does not exist — and worse, it says
// nothing about the space they really could read, which is the household's and whose
// key kenward can no more rotate than a private one. A revocation that reads as more
// complete than it is, is the failure this text exists to prevent, in both directions.
func TestRevocationWarningIsTrueForASharedOnlyMember(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	shared := enrol.Revocation{
		Member: domain.Member{ID: "leo", Name: "Leo", SharedOnly: true},
		At:     at,
	}
	full := enrol.Revocation{
		Member: domain.Member{ID: "david", Name: "David", Private: "dp"},
		Space:  "dp",
		At:     at,
	}

	got := renderRevocation(shared)
	if strings.Contains(got, `""`) {
		t.Errorf("the warning names an empty lore space:\n%s", got)
	}
	for _, want := range []string{
		"Leo is unbound",
		"They had no private memory",
		"household's shared",
		"has NOT been rotated",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}

	// And the ordinary member's warning is word for word what it was.
	if w := renderRevocation(full); !strings.Contains(w, `Their lore space "dp" has NOT been re-keyed`) {
		t.Errorf("a full member's revocation warning changed:\n%s", w)
	}
}
