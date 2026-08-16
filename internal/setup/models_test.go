package setup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDefaultModelsProbeReadsMaxModelLen against a server shaped like vLLM's.
//
// The window is the point. --max-model-len is routinely far below what the model card
// advertises, and it is the server that refuses the request, so a household that typed
// the card's number gets a provider error in front of a member. Reading it is the
// difference.
func TestDefaultModelsProbeReadsMaxModelLen(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// Trimmed from a real vLLM response, extra fields and all: the decoder has
		// to ignore what it does not know, because every OpenAI-compatible server
		// puts something different here.
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"qwen3.6-27b-awq","object":"model","created":1,"owned_by":"vllm",
			 "root":"/models/qwen","parent":null,"max_model_len":262144,
			 "permission":[{"id":"modelperm-1","allow_sampling":true}]}
		]}`))
	}))
	defer srv.Close()

	got, err := DefaultModelsProbe(t.Context(), srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("models = %+v, want one", got)
	}
	if got[0].ID != "qwen3.6-27b-awq" {
		t.Errorf("id = %q", got[0].ID)
	}
	if got[0].ContextWindow != 262144 {
		t.Errorf("context window = %d, want 262144", got[0].ContextWindow)
	}
}

// TestModelsProbeTreatsAServerThatPublishesNoWindowAsFine.
//
// llama.cpp and ollama publish nothing here. That is not a failure and must not be
// reported as one: the endpoint keeps whatever number the operator gave it, or kenward's
// own default.
func TestModelsProbeTreatsAServerThatPublishesNoWindowAsFine(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-3.1-8b","object":"model"}]}`))
	}))
	defer srv.Close()

	got, err := DefaultModelsProbe(t.Context(), srv.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContextWindow != 0 {
		t.Fatalf("models = %+v, want one with no window", got)
	}
}

// TestModelsProbeNamesWhatWentWrong.
//
// A 401 is a missing key and a 404 is a base_url with the /v1 left off, and the person
// reading the answer is looking at the field they just typed. "did not answer" would be
// true and useless.
func TestModelsProbeNamesWhatWentWrong(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorised", http.StatusUnauthorized, `{}`, "401"},
		{"not found", http.StatusNotFound, `{}`, "404"},
		{"not a model list", http.StatusOK, `<html>nope`, "not a model list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := DefaultModelsProbe(t.Context(), srv.URL+"/v1", "")
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestModelsProbeSendsNoAuthorizationWithoutAKey: a local machine on the household's own
// network usually has nothing in front of it, and sending an empty bearer token is a way
// to be refused by a server that would otherwise have answered.
func TestModelsProbeSendsNoAuthorizationWithoutAKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("an Authorization header was sent with no key configured")
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, err := DefaultModelsProbe(t.Context(), srv.URL+"/v1", "   "); err != nil {
		t.Fatal(err)
	}
}
