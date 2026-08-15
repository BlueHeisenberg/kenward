// Package routing decides which machine answers a turn.
//
// Endpoints carry tier tags; a conversation carries an ordered chain of tiers it is
// permitted to use. The chain is the privacy policy: routing walks it in order and,
// when it is exhausted, refuses. It never widens the chain, for any reason.
package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Endpoint is one OpenAI-compatible backend.
type Endpoint struct {
	Name    string
	BaseURL string
	Model   string
	// No credential appears here. An endpoint's key may come from a file, an
	// environment variable or a systemd credential, and which one is a question this
	// package deliberately cannot answer: the completer is constructed with a
	// resolver, resolves at the point of use, and retains nothing. A field naming one
	// of three possible sources would be misinformation the moment an operator chose
	// a different one.
	// Tags are the tiers this endpoint belongs to.
	Tags []string
	// Timeout bounds a single completion.
	Timeout time.Duration
}

// Message is one turn in the prompt.
type Message struct {
	Role    string // system | user | assistant | tool
	Content string
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string
	Description string
	// Schema is the JSON Schema for the tool's arguments.
	Schema json.RawMessage
}

// ToolCall is a tool invocation the model asked for.
type ToolCall struct {
	ID   string
	Name string
	// Arguments is the raw JSON the model produced. It is deliberately not decoded
	// here: a malformed call must be a parsing decision made by the caller that
	// understands the tool, not a routing failure.
	Arguments json.RawMessage
}

// Request is a completion request, independent of which endpoint serves it.
type Request struct {
	Messages  []Message
	MaxTokens int
	// Temperature is a pointer so that zero can be expressed. Temperature 0 is
	// meaningful — it is what a turn wants when it is about to make a tool call and
	// determinism matters more than variety — and a plain float64 makes it
	// indistinguishable from "unset", which serialises as the provider's default of
	// roughly 1.0. Nil means unset.
	Temperature *float64
	// Tools the model may call. Empty means none are offered.
	Tools []ToolSpec
}

// Finish reasons, as reported by the endpoint. The raw provider string is preserved on
// Completion for anything a provider invents.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// Completion is what came back, plus which endpoint produced it.
type Completion struct {
	Text      string
	ToolCalls []ToolCall
	// FinishReason is why generation stopped. It matters beyond diagnostics:
	// FinishContentFilter means the model declined, which is a final answer and not
	// an availability problem, so a caller must not respond by trying another
	// machine. Doing so would walk the tier chain handing the same content to each
	// endpoint in turn, and for a chain ending in a cloud tier that means sending a
	// provider content a local model refused.
	FinishReason string
	Endpoint     string
	Tier         string
	Latency      time.Duration
}

// NoBackendError reports that every tier in the chain was exhausted.
//
// It carries what was attempted so the refusal shown to the member can name the tiers
// and the machines, rather than being a generic failure.
type NoBackendError struct {
	// Chain is the ordered tier chain that was permitted.
	Chain []string
	// Tried lists endpoint names that were attempted or skipped.
	Tried []string
}

func (e *NoBackendError) Error() string {
	return fmt.Sprintf("routing: no reachable endpoint in tiers [%s] (tried: %s)",
		strings.Join(e.Chain, ", "), strings.Join(e.Tried, ", "))
}

// Router selects an endpoint and produces a completion.
//
// Implementations must honour these semantics exactly:
//
//   - Walk chain in order. Within a tier, skip endpoints in cooldown, connect-probe
//     the rest so a powered-off machine is a fast skip rather than a hang, and try
//     survivors in least-recently-used order.
//   - Fall through to the next tier only when the current one yields nothing.
//   - Return *NoBackendError when the chain is exhausted. Never consult an endpoint
//     outside the chain.
//   - Fail over before the first token only. Once a response has begun there is no
//     clean retry, and retrying produces spliced or duplicated output.
type Router interface {
	Complete(ctx context.Context, chain []string, req Request) (Completion, error)
}
