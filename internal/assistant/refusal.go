// Refusal text. When routing exhausts a scope's tier chain the node emits the
// refusal directly — a model that cannot be reached cannot explain why it cannot be
// reached. Refusal text is a product surface: it says what happened and why, names
// what was tried, and never implies a capability that does not exist. Every string
// here is golden-tested; changing one is a deliberate fixture edit.

package assistant

import (
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// refusalText renders a *routing.NoBackendError as the member sees it: the tiers that
// were permitted, the endpoints that were attempted, and the fact that the chain is a
// boundary the node will not cross — the chain is the privacy policy, and a refusal
// that hid it would read as a malfunction rather than a promise kept.
func refusalText(sc domain.Scope, e *routing.NoBackendError) string {
	if len(e.Chain) == 0 {
		return "No machine is allowed to answer this conversation — its tier chain is empty. Ask whoever runs this node to configure one."
	}

	whose := "your allowed tiers"
	if sc.Kind == domain.ScopeGroup {
		whose = "the household's allowed tiers"
	}
	tierWord := "those tiers"
	if len(e.Chain) == 1 {
		tierWord = "that tier"
	}
	tried := "I tried " + naturalJoin(backtickAll(e.Tried)) + "."
	if len(e.Tried) == 0 {
		tried = "I found no endpoints to try."
	}

	var b strings.Builder
	b.WriteString("No machine in ")
	b.WriteString(whose)
	b.WriteString(" (")
	b.WriteString(strings.Join(backtickAll(e.Chain), ", "))
	b.WriteString(") is reachable right now — ")
	b.WriteString(tried)
	b.WriteString(" This conversation is limited to ")
	b.WriteString(tierWord)
	b.WriteString(", so I won't send it anywhere else. Wake one of them and ask again.")
	return b.String()
}

// backtickAll wraps each name in backticks so tier and machine names read as names,
// not as prose.
func backtickAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "`" + n + "`"
	}
	return out
}

// naturalJoin joins a list the way a sentence does: "a", "a and b", "a, b and c".
func naturalJoin(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
