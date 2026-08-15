package routing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/BlueHeisenberg/keel/llm"
)

// Completer produces one completion from one endpoint. It is the seam between
// routing policy and the wire protocol: Pool decides which endpoint to ask, a
// Completer does the asking, and tests inject fakes.
//
// Errors carry the failure class in keel/llm's vocabulary, which is the single
// source of truth for classification. A *llm.TransportError means the bytes
// never completed a round trip — refused dial, reset, TLS failure, a fired
// per-attempt timeout — and the pool may try another machine. A *llm.APIError
// means the endpoint answered non-2xx; only a 5xx status justifies failover,
// because a 4xx would be rejected identically everywhere. Anything else —
// llm.ErrInvalidRequest, a missing key variable — is returned to the caller
// as-is and never triggers failover.
type Completer interface {
	Complete(ctx context.Context, ep Endpoint, req Request) (Completion, error)
}

// NewHTTPCompleter returns the production Completer, backed by keel/llm's
// non-streaming Chat call against the OpenAI-compatible /chat/completions
// endpoint.
//
// client may be nil, in which case keel/llm builds one suitable for the job;
// per-attempt deadlines come from Endpoint.Timeout, never from the client.
// lookupEnv resolves Endpoint.APIKeyEnv to the key at call time — pass
// os.LookupEnv in production, a fake in tests; nil means os.LookupEnv. The key
// goes into the Authorization header and appears nowhere else: not in errors,
// not in logs.
func NewHTTPCompleter(client *http.Client, lookupEnv func(string) (string, bool)) Completer {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &httpCompleter{
		provider:  llm.New(llm.Options{Client: client}),
		lookupEnv: lookupEnv,
	}
}

// httpCompleter adapts one routing.Endpoint per call onto llm.Provider.Chat.
// It holds no endpoint state of its own; everything per-attempt is derived
// from the arguments.
type httpCompleter struct {
	provider  llm.Provider
	lookupEnv func(string) (string, bool)
}

// Complete sends one non-streaming chat completion request to ep and returns
// the completion text. The pool fills in Endpoint, Tier and Latency.
//
// The per-attempt deadline is carried by llm.Endpoint.Timeout, deliberately
// not by a context.WithTimeout around the call: keel/llm attributes a
// parent-context expiry to the caller and returns it bare rather than as a
// *llm.TransportError, which would stop the pool from cooling down the
// endpoint that just timed out on it. ctx stays the caller's own lifetime; an
// endpoint's slowness must come back as a fact about the endpoint.
func (c *httpCompleter) Complete(ctx context.Context, ep Endpoint, req Request) (Completion, error) {
	var key string
	if ep.APIKeyEnv != "" {
		k, ok := c.lookupEnv(ep.APIKeyEnv)
		if !ok || k == "" {
			return Completion{}, fmt.Errorf("routing: endpoint %s: environment variable %s is not set", ep.Name, ep.APIKeyEnv)
		}
		key = k
	}

	lep := llm.Endpoint{
		BaseURL: ep.BaseURL,
		Model:   ep.Model,
		APIKey:  key,
		Label:   ep.Name,
		Timeout: ep.Timeout,
	}

	lreq := llm.ChatReq{Messages: make([]llm.ChatMessage, 0, len(req.Messages))}
	for _, m := range req.Messages {
		lreq.Messages = append(lreq.Messages, llm.ChatMessage{Role: m.Role, Content: m.Content})
	}
	// A nil Temperature means "unset" and leaves the provider's default in
	// place; a non-nil value is sent as-is, explicitly including zero — the
	// pointer exists precisely so a deterministic turn can ask for 0 and have
	// it serialised rather than dropped. MaxTokens keeps zero-means-unset.
	if req.Temperature != nil {
		t := *req.Temperature
		lreq.Temperature = &t
	}
	if req.MaxTokens != 0 {
		n := req.MaxTokens
		lreq.MaxTokens = &n
	}

	resp, err := c.provider.Chat(ctx, lep, lreq)
	if err != nil {
		return Completion{}, scrubbed(err)
	}
	return Completion{Text: resp.Content}, nil
}

// scrubbed strips provider-echoed text out of an *llm.APIError before it
// leaves this package. Provider error bodies can quote request content, and
// message content never belongs in an error. Classification survives: the
// scrubbed copy is still a *llm.APIError carrying its status, type and code.
//
// TODO(keel): remove this wrapper once keel/llm's APIError.Error() renders
// status, type, code and endpoint only (change queued with keel's author).
// When dropping it, keep TestHTTPCompleterAPIErrorScrubbed and retarget it at
// the unwrapped error — the proof that an echoing 400 body never surfaces is
// the valuable half; this wrapper is only the workaround.
func scrubbed(err error) error {
	var ae *llm.APIError
	if errors.As(err, &ae) {
		clean := *ae
		clean.Message = ""
		clean.Body = ""
		return &clean
	}
	return err
}
