package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ModelsTimeout bounds one /v1/models request.
//
// Longer than ProbeTimeout because this is a real HTTP round trip against a server that
// may be loading a model, and shorter than an endpoint's own completion timeout because
// nobody is waiting on an answer here — a server that does not list its models in five
// seconds is one the operator types the numbers for.
const ModelsTimeout = 5 * time.Second

// ModelInfo is one model an OpenAI-compatible server lists.
type ModelInfo struct {
	// ID is the name the server expects in a completion request, which is exactly
	// what endpoints[].model has to hold. Reading it is the point: the commonest
	// first-run failure is a model name that is close but not what the server calls
	// itself, and it surfaces as the provider's own "model not found" mid-turn.
	ID string
	// ContextWindow is the server's own max_model_len, or zero when it does not
	// publish one.
	//
	// vLLM puts it on every entry of /v1/models, and it is the number that actually
	// binds: --max-model-len is frequently far below what the model card advertises,
	// and it is the server that refuses the request. Reading it beats typing it,
	// because the operator typing it is reading it off the same server anyway — or,
	// far more often, off the model card and getting it wrong.
	//
	// Zero is not a failure. llama.cpp and ollama publish nothing here, and an
	// endpoint whose window kenward could not learn keeps whatever the operator gave
	// it or config.DefaultContextWindow.
	ContextWindow int
}

// ModelsProbe lists the models an endpoint serves. It is a function type for the same
// reason Probe is: the dashboard has to be testable against a server that answers, one
// that refuses, and one that is not there, and none of those can be arranged with a
// real network.
type ModelsProbe func(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error)

// DefaultModelsProbe asks an endpoint's /v1/models what it serves.
//
// It is deliberately not what DefaultProbe does, and the two are not interchangeable.
// DefaultProbe is a TCP connect made while somebody is still typing an address, against
// a server they have not yet agreed to talk to; this is an authenticated HTTP request
// made once, after they have. Making the first one into the second would send a request
// to whatever host a typo names.
//
// The api key is sent as a bearer token when there is one, because a provider will
// answer nothing without it. A local machine usually needs none, which is why it is a
// parameter rather than a requirement.
func DefaultModelsProbe(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"

	ctx, cancel := context.WithTimeout(ctx, ModelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("setup: %s is not an address that can be asked for its models: %w", baseURL, err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("setup: %s did not answer: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Named rather than swallowed: a 401 here is a missing key and a 404 is a
		// base_url with the /v1 left off, and both are worth telling somebody who is
		// looking at the field they just typed.
		return nil, fmt.Errorf("setup: %s answered %s", endpoint, resp.Status)
	}

	// Only the two fields that matter are decoded. An OpenAI-compatible server is
	// entitled to put anything else on these objects and several do.
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("setup: %s answered with something that is not a model list: %w", endpoint, err)
	}

	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		window := m.MaxModelLen
		if window < 0 {
			window = 0
		}
		out = append(out, ModelInfo{ID: m.ID, ContextWindow: window})
	}
	return out, nil
}
