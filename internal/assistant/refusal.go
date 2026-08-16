// Refusal text. When routing exhausts a scope's tier chain the node emits the
// refusal directly — a model that cannot be reached cannot explain why it cannot be
// reached. Refusal text is a product surface: it says what happened and why, names
// what was tried, and never implies a capability that does not exist. Every string
// here is golden-tested; changing one is a deliberate fixture edit.

package assistant

import (
	"errors"
	"strings"

	"github.com/BlueHeisenberg/keel/llm"

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
	// "Unavailable" is chosen over "I tried": Tried lists endpoints that were
	// attempted and endpoints skipped for cooldown or a failed probe, and claiming
	// an attempt that never happened is a small untruth in a message whose whole
	// value is being accurate.
	tried := naturalJoin(backtickAll(e.Tried)) + " were unavailable."
	switch len(e.Tried) {
	case 0:
		tried = "I found no endpoints to try."
	case 1:
		tried = backtickAll(e.Tried)[0] + " was unavailable."
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

// Notices for turns the model could not answer. Like the refusals they are emitted
// by the node — there is no model to phrase them — and they exist because a member
// who sends a message always gets a reply: silence is the one response that teaches
// a household the assistant is broken and unpredictable. Golden-tested.
const (
	// modelBusyText covers rate limiting: transient, and retrying is genuinely the
	// right advice.
	modelBusyText = "The model is busy right now. Try again in a moment."
	// misconfiguredText covers failures no retry will fix — a rejected key, an
	// unknown model, a request the endpoint refuses to parse (400 and
	// llm.ErrInvalidRequest are both that request, rejected on either side of the
	// wire). The member cannot repair any of these; the operator can.
	misconfiguredText = "Something is wrong with this household's setup — tell whoever runs it."
	// turnFailedText covers everything else. It promises nothing it does not know:
	// the message arrived, no answer was produced.
	turnFailedText = "Something went wrong reaching the model, and your message wasn't answered. Try again in a moment."
	// reasoningOnlyText covers a turn the model spent thinking and never finished:
	// it produced a reasoning trace and no answer. Nothing is broken — the machine
	// answered, the model ran — so this deliberately does not read as an outage,
	// and it does not read as the model declining either. The one thing the member
	// can actually do is give the model less to chew on; more room to answer in is
	// the operator's knob, not theirs, so it is not offered as advice they cannot
	// take.
	reasoningOnlyText = "The model spent the whole turn thinking and didn't get to an answer. Nothing is broken — try asking again, or in smaller pieces."
)

// completionFailureText classifies a router failure that is not a *NoBackendError
// into the notice the member sees. Every error maps to something — the caller sends
// the result unconditionally — because a message that produced no reply must at
// least produce the news that it didn't.
//
// The classification reads keel/llm's error vocabulary, which the routing seam
// passes through unchanged: a content-filter refusal arrives as an
// *llm.EmptyResponseError rather than a Completion whenever the declining endpoint
// sent no text alongside the finish reason, which is the common form. So does a
// turn a reasoning model spent thinking without answering — routing declines to
// fail over on either (see routing.shouldFailover), so both land here rather than
// as a *NoBackendError blaming machines that were never the problem.
//
// A decline is checked first: a model that reasoned its way to refusing still
// refused, and the refusal is the more important thing to say.
func completionFailureText(err error) string {
	var ee *llm.EmptyResponseError
	if errors.As(err, &ee) {
		switch {
		case ee.FinishReason == llm.FinishContentFilter:
			return contentFilterText
		case ee.Reasoning != "":
			return reasoningOnlyText
		}
	}
	var ae *llm.APIError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case 429:
			return modelBusyText
		case 400, 401, 403, 404:
			// 400 is the endpoint saying it will not parse this request — the same
			// permanent fault as ErrInvalidRequest, caught one hop later. Advising a
			// retry for it would send the member back to a wall.
			return misconfiguredText
		}
		return turnFailedText
	}
	if errors.Is(err, llm.ErrInvalidRequest) {
		return misconfiguredText
	}
	return turnFailedText
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
