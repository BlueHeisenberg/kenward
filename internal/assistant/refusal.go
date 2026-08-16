// Refusal text. When routing exhausts a scope's tier chain the node emits the
// refusal directly — a model that cannot be reached cannot explain why it cannot be
// reached. Refusal text is a product surface: it says what happened and why, names
// what was tried, and never implies a capability that does not exist. Every string
// here is golden-tested; changing one is a deliberate fixture edit.

package assistant

import (
	"errors"

	"github.com/BlueHeisenberg/keel/llm"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// refusalText renders a *routing.NoBackendError as the member sees it: the tiers that
// were permitted, the endpoints that were attempted, and the fact that the chain is a
// boundary the node will not cross — the chain is the privacy policy, and a refusal
// that hid it would read as a malfunction rather than a promise kept.
func (u *Unit) refusalText(sc domain.Scope, e *routing.NoBackendError) string {
	if len(e.Chain) == 0 {
		return u.problem(u.cat.RefusalEmptyChain)
	}

	// Whose chain was walked, which is the household's for every conversation that
	// is the household's — the group chat and a member's private chat with kenward
	// alike. Saying "your allowed tiers" in the second would name a policy the member
	// did not set and cannot change from there.
	//
	// In several languages this value carries the preposition, already contracted,
	// because the alternative is ungrammatical: Catalan de + els is dels and
	// Portuguese de + os is dos. The template must not add one.
	whose := u.cat.WhoseDirect
	if !sc.TouchesPrivateMemory() {
		whose = u.cat.WhoseGroup
	}
	return u.problem(u.cat.RefusalAssembled(
		whose,
		u.cat.Chain(e.Chain),
		u.cat.Tried(e.Tried),
		u.cat.TierWord(len(e.Chain)),
	))
}

// Notices for turns the model could not answer. Like the refusals they are emitted
// by the node — there is no model to phrase them — and they exist because a member
// who sends a message always gets a reply: silence is the one response that teaches
// a household the assistant is broken and unpredictable. Golden-tested in English.
//
// modelBusy covers rate limiting: transient, and retrying is genuinely the right
// advice.
func (u *Unit) modelBusy() string { return u.problem(u.cat.ModelBusy) }

// misconfigured covers failures no retry will fix — a rejected key, an unknown
// model, a request the endpoint refuses to parse (400 and llm.ErrInvalidRequest are
// both that request, rejected on either side of the wire). The member cannot repair
// any of these; the operator can.
func (u *Unit) misconfigured() string { return u.problem(u.cat.Misconfigured) }

// turnFailed covers everything else. It promises nothing it does not know: the
// message arrived, no answer was produced.
func (u *Unit) turnFailed() string { return u.problem(u.cat.TurnFailed) }

// reasoningOnly covers a turn the model spent thinking and never finished: it
// produced a reasoning trace and no answer. Nothing is broken — the machine
// answered, the model ran — so this deliberately does not read as an outage, and it
// does not read as the model declining either. The one thing the member can actually
// do is give the model less to chew on; more room to answer in is the operator's
// knob, not theirs, so it is not offered as advice they cannot take.
func (u *Unit) reasoningOnly() string { return u.problem(u.cat.ReasoningOnly) }

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
func (u *Unit) completionFailureText(err error) string {
	var ee *llm.EmptyResponseError
	if errors.As(err, &ee) {
		switch {
		case ee.FinishReason == llm.FinishContentFilter:
			return u.contentFilter()
		case ee.Reasoning != "":
			return u.reasoningOnly()
		}
	}
	var ae *llm.APIError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case 429:
			return u.modelBusy()
		case 400, 401, 403, 404:
			// 400 is the endpoint saying it will not parse this request — the same
			// permanent fault as ErrInvalidRequest, caught one hop later. Advising a
			// retry for it would send the member back to a wall.
			return u.misconfigured()
		}
		return u.turnFailed()
	}
	if errors.Is(err, llm.ErrInvalidRequest) {
		return u.misconfigured()
	}
	return u.turnFailed()
}

// Marking each name as code and joining the list are both the catalogue's job now,
// and neither moved there for tidiness.
//
// The names used to be wrapped in literal backticks, which is what a refusal looks
// like when its author expects a Markdown renderer and the transport sets no parse
// mode: the member read "`workshop` was unavailable", backticks and all. The intent
// was right and the mechanism was missing. transport.Code escapes the name as well,
// which matters because a tier or machine name comes from the household's own
// configuration file — and the catalogue applies it, because Arabic has to put an
// isolate around each name and the isolate belongs beside the markup, not around it.
//
// The join was English list grammar hardcoded: ", " and " and ". Spanish alternates
// y and e on the sound of the following word, Arabic prefixes و with no space after
// it, and Chinese uses 、 for the separator and for the conjunction both. There is no
// shared implementation to keep.
