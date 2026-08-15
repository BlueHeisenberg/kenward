// The remember tool: its specification on the routing seam and the defensive parsing
// of what the model does with it.
//
// The schema is docs/PROMPT.md's, verbatim. Proposals arrive as native tool calls;
// routing.ToolCall.Arguments is raw JSON precisely because a malformed call is a
// parsing decision for the caller that understands the tool, and the rules here are
// fixed: a malformed call is dropped with a log line, never a crashed turn and never
// a write. An unknown target degrades to unsure, which is safe because unsure only
// means the member is asked where the entry goes — and no write happens without the
// member's button press regardless.

package assistant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// rememberToolName is the one tool this package offers the model.
const rememberToolName = "remember"

// rememberSchema is the input schema from docs/PROMPT.md, verbatim.
const rememberSchema = `{
  "type": "object",
  "required": ["title", "body", "target"],
  "properties": {
    "title":      {"type": "string", "description": "Short, specific, and searchable later."},
    "body":       {"type": "string", "description": "The fact itself, stated plainly and out of context — it will be read a year from now with none of this conversation around it."},
    "domain":     {"type": "string", "description": "A coarse category, e.g. household/logistics."},
    "confidence": {"type": "string", "enum": ["experimental", "provisional", "validated", "hardened"]},
    "markers":    {"type": "array", "items": {"type": "string"}},
    "target":     {"type": "string", "enum": ["personal", "shared", "unsure"]}
  }
}`

// rememberTools is the tool list attached to every turn's request.
func rememberTools() []routing.ToolSpec {
	return []routing.ToolSpec{{
		Name:        rememberToolName,
		Description: "Propose storing something in memory. The member confirms before anything is written.",
		Schema:      json.RawMessage(rememberSchema),
	}}
}

// rememberCall mirrors the tool schema. Unknown fields are tolerated: models
// decorate, and a decoration is not a malformation.
type rememberCall struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     string   `json:"domain"`
	Confidence string   `json:"confidence"`
	Markers    []string `json:"markers"`
	Target     string   `json:"target"`
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
		// An unknown target is not a reason to lose the proposal: unsure means the
		// member is asked where it goes, and the member decides everything anyway.
		warn = joinWarn(warn, fmt.Sprintf("unknown target %q treated as unsure", call.Target))
	}

	confidence := call.Confidence
	if confidence == "" {
		// lore enforces its confidence vocabulary at write time; an empty value
		// would fail after the member already said yes. Provisional is the honest
		// default for something a model inferred mid-conversation.
		confidence = "provisional"
	}

	return &capture.Proposal{
		Draft: memory.Draft{
			Domain:     call.Domain,
			Title:      call.Title,
			Body:       call.Body,
			Confidence: confidence,
			Markers:    call.Markers,
		},
		Target: target,
	}, warn
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
