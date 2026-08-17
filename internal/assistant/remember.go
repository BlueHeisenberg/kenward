// The two tools kenward offers the model — remember and publish — their
// specifications on the routing seam, and the defensive parsing of what the model
// does with them.
//
// The schemas are docs/PROMPT.md's, verbatim. Proposals arrive as native tool calls;
// routing.ToolCall.Arguments is raw JSON precisely because a malformed call is a
// parsing decision for the caller that understands the tool, and the rules here are
// fixed: a malformed call is dropped with a log line, never a crashed turn and never
// a write. An unknown target degrades to unsure, and unsure is the safe degradation
// rather than merely a tidy one: it is the one target that is always put to the member
// as a question, so a call this file could not read never becomes a write nobody
// chose.

package assistant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// rememberToolName is the one tool this package offers the model.
const rememberToolName = "remember"

// rememberSchema is the input schema from docs/PROMPT.md, verbatim.
//
// It used to carry a "markers" array and does not any more. Markers were the one
// field the model could write that the member was never shown — the capture question
// and the write announcement both render entryBlock, which is the title and the body
// — and that a later turn's prompt then presented back to the model as a human's
// instruction to obey (prompt.go's confidenceText, which no longer says so). A live
// run produced markers ["FOR THE WHOLE HOUSE"] on a Spanish entry with no person
// anywhere in its authorship.
//
// Removing the field rather than showing it in the question is the smaller fix and the
// better one. Showing them would cost every capture question the space, and would ask
// a member to judge a lore concept nothing else in kenward has ever explained to them;
// and the only marker the model was observed to reach for restates the destination,
// which the scope decides and this node already knows. So kenward writes none: an
// entry's audience is which space it is in, and that is a fact about the write rather
// than a string alongside it. Nothing is lost that was ever read — see confidenceText.
//
// A model that emits markers anyway is not malformed, only ignored: unknown fields are
// tolerated below, so the value is dropped with the rest of the decoration.
const rememberSchema = `{
  "type": "object",
  "required": ["title", "body", "domain", "target"],
  "properties": {
    "title":      {"type": "string", "description": "Short, specific, and searchable later."},
    "body":       {"type": "string", "description": "The fact itself, stated plainly and out of context — it will be read a year from now with none of this conversation around it."},
    "domain":     {"type": "string", "description": "A coarse category, e.g. household/logistics."},
    "confidence": {"type": "string", "enum": ["experimental", "provisional", "validated", "hardened"]},
    "aliases":    {"type": "array", "items": {"type": "string"}, "description": "The member's own words for what this is about, in the language they are speaking, when that is not English."},
    "target":     {"type": "string", "enum": ["personal", "shared", "unsure"]}
  }
}`

// publishToolName is the second tool: the member asking for an entry they already
// recorded privately to be published to the household.
const publishToolName = "publish"

// publishSchema is the input schema from docs/PROMPT.md, verbatim.
//
// It takes a title and no id, and that is the whole security design of the tool.
// lore's ids are global and lore_get is not space-scoped, so an id is a capability:
// whoever holds one can name an entry in any space. The model is a place member text
// arrives, so an id it produced would be an id the member supplied. The title is
// resolved against this turn's own retrieval instead — see publishTarget.
const publishSchema = `{
  "type": "object",
  "required": ["title"],
  "properties": {
    "title": {"type": "string", "description": "The title of the entry to publish, exactly as it appears in the private memory section above."}
  }
}`

// toolSpecs is the tool list attached to a turn's request. The publish tool is
// offered only in a direct scope: publishing from the household group is meaningless
// — the entry would already be there — and the capture engine refuses it anyway.
// Offering a tool whose every call must be dropped only teaches the model to call it.
func toolSpecs(sc domain.Scope) []routing.ToolSpec {
	specs := []routing.ToolSpec{{
		Name:        rememberToolName,
		Description: "Propose storing something in memory. A proposal for the member's private memory may be written straight away and shown to them, with an undo button; anything for the household's shared memory is written only if they confirm.",
		Schema:      json.RawMessage(rememberSchema),
	}}
	if sc.AllowsPrivateCapture() {
		specs = append(specs, routing.ToolSpec{
			Name:        publishToolName,
			Description: "Publish an entry from the member's private memory to the household. The member sees its full text and confirms before anything is published.",
			Schema:      json.RawMessage(publishSchema),
		})
	}
	// The reminder tools are offered in every scope; see remindSpecs.
	return append(specs, remindSpecs()...)
}

// rememberCall mirrors the tool schema. Unknown fields are tolerated: models
// decorate, and a decoration is not a malformation.
//
// There is deliberately no Markers field, and that is what stops a model that
// improvises one from having it stored: no field, no decode, no write. See
// rememberSchema.
type rememberCall struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	Domain     string `json:"domain"`
	Confidence string `json:"confidence"`
	// Aliases are the member's own words. The entry itself stays English — see
	// capture.Proposal.Aliases for why — and these are what make it findable by the
	// person who said it. An English conversation leaves them empty.
	Aliases []string `json:"aliases"`
	Target  string   `json:"target"`
}

// extractProposal reads the completion's tool calls and returns the proposal, if the
// model made a well-formed one. Only the first remember call is considered — the
// prompt allows one proposal per reply and the capture engine enforces the same
// bound — and calls to tools that were never offered are dropped, not guessed at.
// The returned warning is non-empty when something was dropped or repaired; the
// caller logs it.
func extractProposal(calls []routing.ToolCall) (p *capture.Proposal, warn string) {
	var payload json.RawMessage
	for _, c := range calls {
		switch c.Name {
		case publishToolName:
			continue // read by extractPublishTitle
		case remindToolName, unremindToolName:
			continue // read by extractReminders
		}
		if c.Name != rememberToolName {
			warn = joinWarn(warn, fmt.Sprintf("model called unknown tool %q; dropped", c.Name))
			continue
		}
		if payload != nil {
			warn = joinWarn(warn, "model made more than one remember call; using the first")
			continue
		}
		payload = c.Arguments
	}
	if payload == nil {
		return nil, warn
	}

	call, err := parseRemember(payload)
	if err != nil {
		return nil, joinWarn(warn, err.Error())
	}

	target := capture.TargetUnsure
	switch call.Target {
	case "personal":
		target = capture.TargetPersonal
	case "shared":
		target = capture.TargetShared
	case "unsure":
	default:
		// An unknown target is not a reason to lose the proposal, and unsure is
		// where it has to land: it is the target that is always asked about, so a
		// field this parser could not understand never decides a write on its own.
		warn = joinWarn(warn, fmt.Sprintf("unknown target %q treated as unsure", call.Target))
	}

	confidence := call.Confidence
	if confidence == "" {
		// lore enforces its confidence vocabulary at write time; an empty value
		// would fail after the member already said yes. Provisional is the honest
		// default for something a model inferred mid-conversation.
		confidence = "provisional"
	}

	rememberDomain := call.Domain
	if strings.TrimSpace(rememberDomain) == "" {
		// lore requires domain at write time, same as confidence above: an empty
		// value would fail after the member already pressed confirm, and domain is
		// declared required in the schema for exactly that reason — but a model can
		// omit a required field too, so the default is what actually prevents the
		// failure. "household/general" follows the schema's own example shape
		// (household/logistics) and reads as an honest "uncategorized" bucket.
		rememberDomain = "household/general"
	}

	// No markers: the tool does not take them and this node does not invent them, so
	// every field of the draft below is something the member is shown before or
	// immediately after it is written.
	return &capture.Proposal{
		Draft: memory.Draft{
			Domain:     rememberDomain,
			Title:      call.Title,
			Body:       call.Body,
			Confidence: confidence,
		},
		Target:  target,
		Aliases: call.Aliases,
	}, warn
}

// rememberCalls counts how many remember calls the completion made.
//
// extractProposal keeps the first and logs the rest, which is the right thing to do
// with them and the wrong place to stop. A member who mentioned two things and had one
// silently dropped has no way to know it happened: the turn looks, from the chat, like
// a turn about one thing. The count is what the reply's notice is rendered off — see
// Unit.turn — and it is gathered here rather than threaded out of extractProposal
// because a second pass over at most a handful of tool calls is cheaper than a third
// return value in four call sites.
func rememberCalls(calls []routing.ToolCall) int {
	n := 0
	for _, c := range calls {
		if c.Name == rememberToolName {
			n++
		}
	}
	return n
}

// parseRemember decodes one call's arguments, tolerating trailing junk after the
// JSON object but refusing a call missing what the schema requires.
func parseRemember(payload json.RawMessage) (rememberCall, error) {
	var call rememberCall
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	if err := dec.Decode(&call); err != nil {
		return rememberCall{}, fmt.Errorf("remember arguments are not valid JSON: %v", err)
	}
	if strings.TrimSpace(call.Title) == "" {
		return rememberCall{}, fmt.Errorf("remember call has no title")
	}
	if strings.TrimSpace(call.Body) == "" {
		return rememberCall{}, fmt.Errorf("remember call has no body")
	}
	return call, nil
}

// extractPublishTitle reads the completion's tool calls for a publish request and
// returns the title it named. Only the first call is considered, for the same reason
// as remember: one question per turn reaches the member. A malformed call returns an
// empty title and a warning for the log, never a guess.
func extractPublishTitle(calls []routing.ToolCall) (title string, warn string) {
	for _, c := range calls {
		if c.Name != publishToolName {
			continue
		}
		if title != "" {
			warn = joinWarn(warn, "model made more than one publish call; using the first")
			continue
		}
		var call struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(strings.NewReader(string(c.Arguments))).Decode(&call); err != nil {
			warn = joinWarn(warn, fmt.Sprintf("publish arguments are not valid JSON: %v", err))
			continue
		}
		if title = strings.TrimSpace(call.Title); title == "" {
			warn = joinWarn(warn, "publish call has no title")
		}
	}
	return title, warn
}

// strayRememberBlock matches a fenced block labelled remember in reply text. The
// prompt no longer teaches this encoding, but a model that has seen the tool name
// may still improvise one, and raw JSON must never reach the member.
var strayRememberBlock = regexp.MustCompile("(?s)```remember[ \t]*\r?\n(.*?)\r?\n?```")

// sanitizeReply strips improvised remember blocks from the reply text. They are not
// honoured as proposals — proposals travel as tool calls and nothing else — they are
// only removed, with a warning for the log.
func sanitizeReply(text string) (reply string, warn string) {
	if n := len(strayRememberBlock.FindAllStringIndex(text, -1)); n > 0 {
		warn = fmt.Sprintf("model wrote %d remember block(s) in its reply text; stripped, not honoured", n)
	}
	return strings.TrimSpace(strayRememberBlock.ReplaceAllString(text, "")), warn
}

func joinWarn(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}
