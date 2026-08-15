package assistant

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

func TestEstimateTokensIsConservativeForASCII(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abc", 1},
		{"abcd", 2},
		{"héllo", 3}, // 4 ASCII -> 2, é -> 1
		{"日本語", 3},   // one per non-ASCII rune
	}
	for _, tc := range tests {
		if got := estimateTokens(tc.in); got != tc.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// budgetGroups builds two full retrieval groups with bodies large enough that each
// entry costs a predictable ~100 estimated tokens.
func budgetGroups() []spaceGroup {
	body := strings.Repeat("water the plants every second day. ", 9) // ~315 ASCII chars
	mk := func(space domain.SpaceID, title string) memory.Entry {
		return entry(space, title, body, "validated")
	}
	return []spaceGroup{
		{space: "david-private", entries: []memory.Entry{
			mk("david-private", "p1"), mk("david-private", "p2"),
		}},
		{space: "household", entries: []memory.Entry{
			mk("household", "s1"), mk("household", "s2"), mk("household", "s3"),
		}},
	}
}

func budgetUnit(t *testing.T, contextBudget int) *Unit {
	t.Helper()
	opts := testOptions()
	opts.ContextBudget = contextBudget
	opts.MaxTokens = 16
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	return rig.unit
}

func TestBudgetOverflowDropsFromEndOfSharedFirstAndSaysSo(t *testing.T) {
	sc := testDirectScope()

	// Measure the full prompt, then set a budget that forces roughly two entries out.
	full := estimateRequestTokens(budgetUnit(t, 1_000_000).assemble(sc, budgetGroups(), "question").Messages)
	u := budgetUnit(t, full+16-150)

	req := u.assemble(sc, budgetGroups(), "question")
	sys := req.Messages[0].Content

	// Shared entries go first, from the end, never from the middle.
	if !strings.Contains(sys, "- s1 [validated]") {
		t.Error("s1 dropped; drops must come from the end, not the front")
	}
	if strings.Contains(sys, "- s3 [validated]") {
		t.Error("s3 survived; the last shared entry must be the first dropped")
	}
	// Private entries are untouched while shared can still give way.
	for _, want := range []string{"- p1 [validated]", "- p2 [validated]"} {
		if !strings.Contains(sys, want) {
			t.Errorf("private entry %q dropped before the shared group was exhausted", want)
		}
	}
	// The drop is stated, in the shared section, not hidden.
	if !strings.Contains(sys, "more entries were retrieved but dropped to fit the context budget)") &&
		!strings.Contains(sys, "more entry was retrieved but dropped to fit the context budget)") {
		t.Errorf("dropped entries not disclosed in the prompt:\n%s", sys)
	}
	shared := sys[strings.Index(sys, "## From the household's shared memory"):]
	if !strings.Contains(strings.SplitN(shared, "\n\n", 2)[0], "dropped to fit the context budget") {
		t.Error("drop disclosure is not inside the shared section")
	}
}

func TestBudgetTrimsHistoryBeforeRetrievedMemory(t *testing.T) {
	sc := testDirectScope()
	pleasantry := strings.Repeat("thanks, that was helpful. ", 20) // ~520 chars ≈ 175 tokens

	full := func() int {
		u := budgetUnit(t, 1_000_000)
		u.history.add(pleasantry, "you're welcome")
		return estimateRequestTokens(u.assemble(sc, budgetGroups(), "question").Messages)
	}()

	u := budgetUnit(t, full+16-100)
	u.history.add(pleasantry, "you're welcome")
	req := u.assemble(sc, budgetGroups(), "question")
	sys := req.Messages[0].Content

	// Dropping the history turn was enough; every entry survives, nothing is
	// disclosed as dropped, and the pleasantry is gone from the messages.
	for _, want := range []string{"- p1", "- p2", "- s1", "- s2", "- s3"} {
		if !strings.Contains(sys, want) {
			t.Errorf("entry %q was dropped before history", want)
		}
	}
	if strings.Contains(sys, "dropped to fit the context budget") {
		t.Error("entries disclosed as dropped although history should have gone first")
	}
	for _, m := range req.Messages {
		if m.Content == pleasantry {
			t.Error("history survived a budget that required trimming it")
		}
	}
}

func TestBudgetExhaustedKeepsNonNegotiableParts(t *testing.T) {
	sc := testDirectScope()
	u := budgetUnit(t, 32) // absurdly small: everything elastic must go
	req := u.assemble(sc, budgetGroups(), "question")
	sys := req.Messages[0].Content

	if !strings.Contains(sys, "You are kenward, a household assistant.") {
		t.Error("identity was trimmed; it is not elastic")
	}
	if !strings.Contains(sys, "This is a private conversation with David.") {
		t.Error("scope disclosure was trimmed; it is not elastic")
	}
	if last := req.Messages[len(req.Messages)-1]; last.Content != "question" {
		t.Error("the member's message was trimmed; it is not elastic")
	}
	if strings.Contains(sys, "[validated]") {
		t.Error("entries survived a budget that cannot hold them")
	}
	// With every entry dropped, the sections state the drop rather than claiming
	// nothing was found.
	if strings.Contains(sys, emptyGroupText) {
		t.Error("a fully dropped group claims (nothing relevant found), which is false")
	}
	for _, want := range []string{
		"(3 more entries were retrieved but dropped to fit the context budget)",
		"(2 more entries were retrieved but dropped to fit the context budget)",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("missing drop disclosure %q\n%s", want, sys)
		}
	}
}
