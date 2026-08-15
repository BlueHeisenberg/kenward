// Package routing decides which machine answers a turn.
//
// Endpoints carry tier tags; a conversation carries an ordered chain of tiers it is
// permitted to use. The chain is the privacy policy: routing walks it in order and,
// when it is exhausted, refuses. It never widens the chain, for any reason.
package routing

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Endpoint is one OpenAI-compatible backend.
type Endpoint struct {
	Name    string
	BaseURL string
	Model   string
	// APIKeyEnv names the environment variable holding the key. The key itself is
	// never stored in configuration or logged.
	APIKeyEnv string
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
}

// Completion is what came back, plus which endpoint produced it.
type Completion struct {
	Text     string
	Endpoint string
	Tier     string
	Latency  time.Duration
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
