package routing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"
)

func lookupFixed(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestHTTPCompleterSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody struct {
		Model       string   `json:"model"`
		Stream      bool     `json:"stream"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &gotBody); err != nil {
			t.Errorf("request body: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, lookupFixed(map[string]string{"TEST_KEY": "sekrit"}))
	e := Endpoint{
		Name: "srv", BaseURL: srv.URL + "/v1", Model: "m1",
		APIKeyEnv: "TEST_KEY", Timeout: time.Second,
	}
	comp, err := c.Complete(context.Background(), e, Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Text != "hello" {
		t.Fatalf("Text = %q, want hello", comp.Text)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sekrit" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBody.Model != "m1" || gotBody.Stream ||
		gotBody.MaxTokens == nil || *gotBody.MaxTokens != 64 ||
		len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "hi" {
		t.Fatalf("request body = %+v", gotBody)
	}
	if gotBody.Temperature != nil {
		t.Fatalf("temperature = %v, want unset when Request.Temperature is nil", *gotBody.Temperature)
	}
}

// TestHTTPCompleterExplicitZeroTemperature pins the reason Request.Temperature
// is a pointer: an explicit zero — what a deterministic tool-calling turn asks
// for — must be serialised as "temperature":0, not dropped by omitempty and
// silently replaced with the provider's default.
func TestHTTPCompleterExplicitZeroTemperature(t *testing.T) {
	type probe struct {
		set   bool
		value float64
	}
	var got probe
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Temperature *float64 `json:"temperature"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		if body.Temperature != nil {
			got = probe{set: true, value: *body.Temperature}
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, nil)
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	zero := 0.0
	if _, err := c.Complete(context.Background(), e, Request{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: &zero,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !got.set {
		t.Fatal("explicit temperature 0 was dropped from the request body")
	}
	if got.value != 0 {
		t.Fatalf("temperature = %v, want 0", got.value)
	}
}

func TestHTTPCompleterAPIErrorScrubbed(t *testing.T) {
	const secret = "the user's private message"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A provider error that echoes request content, which must never
		// surface in the returned error.
		io.WriteString(w, `{"error":{"message":"bad input: `+secret+`","type":"invalid_request_error","code":"invalid"}}`)
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, nil)
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	_, err := c.Complete(context.Background(), e, Request{
		Messages: []Message{{Role: "user", Content: secret}},
	})
	var ae *llm.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("got %v, want *llm.APIError", err)
	}
	if ae.StatusCode != http.StatusBadRequest || ae.Type != "invalid_request_error" || ae.Code != "invalid" {
		t.Fatalf("APIError = %+v", ae)
	}
	if ae.Message != "" || ae.Body != "" {
		t.Fatalf("APIError not scrubbed: Message=%q Body=%q", ae.Message, ae.Body)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error text echoes message content")
	}
	if shouldFailover(err) {
		t.Fatal("a 400 must not trigger failover")
	}
}

func TestHTTPCompleterConnectRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + l.Addr().String()
	l.Close()

	c := NewHTTPCompleter(nil, nil)
	e := Endpoint{Name: "gone", BaseURL: base, Model: "m", Timeout: time.Second}
	_, err = c.Complete(context.Background(), e, Request{})
	var te *llm.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *llm.TransportError", err)
	}
	if !shouldFailover(err) {
		t.Fatal("a connection refusal must trigger failover")
	}
}

func TestHTTPCompleterTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewHTTPCompleter(nil, nil)
	e := Endpoint{Name: "slow", BaseURL: srv.URL, Model: "m", Timeout: 30 * time.Millisecond}
	_, err := c.Complete(context.Background(), e, Request{})
	var te *llm.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *llm.TransportError from per-endpoint timeout", err)
	}
	if !shouldFailover(err) {
		t.Fatal("a per-endpoint timeout must trigger failover")
	}
}

func TestHTTPCompleterMissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request sent despite missing key")
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, lookupFixed(nil))
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", APIKeyEnv: "ABSENT_KEY", Timeout: time.Second}
	_, err := c.Complete(context.Background(), e, Request{})
	if err == nil {
		t.Fatal("want error for missing key env")
	}
	if !strings.Contains(err.Error(), "ABSENT_KEY") {
		t.Fatalf("error should name the env var: %v", err)
	}
	if shouldFailover(err) {
		t.Fatal("a configuration error must not trigger failover")
	}
}
