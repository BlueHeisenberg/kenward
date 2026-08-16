package routing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"
)

// fixedKey is a KeyFunc handing every endpoint the same key.
func fixedKey(key string) KeyFunc {
	return func(Endpoint) (string, error) { return key, nil }
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

	c := NewHTTPCompleter(nil, fixedKey("sekrit"), nil)
	e := Endpoint{
		Name: "srv", BaseURL: srv.URL + "/v1", Model: "m1",
		Timeout: time.Second,
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

	c := NewHTTPCompleter(nil, nil, nil)
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

// TestHTTPCompleterAPIErrorScrubbed proves that a 400 whose body echoes the
// request never surfaces that content through the error's default rendering —
// the string that reaches a log by default. The completer passes keel's error
// through unmodified, so this test stands guard over keel/llm's own guarantee:
// APIError.Error() renders status, type, code and endpoint only, and the
// provider's prose is reachable solely through the explicit Detail() method.
func TestHTTPCompleterAPIErrorScrubbed(t *testing.T) {
	const secret = "the user's private message"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A provider error that echoes request content, which must never
		// surface in the returned error's default rendering.
		io.WriteString(w, `{"error":{"message":"bad input: `+secret+`","type":"invalid_request_error","code":"invalid"}}`)
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, nil, nil)
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
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error text echoes message content")
	}
	// The prose is unlisted, not lost: a caller that asks by name still gets
	// the provider's words, so disclosure is a decision rather than a default.
	if !strings.Contains(ae.Detail(), secret) {
		t.Fatal("Detail() should carry the provider's prose on explicit request")
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

	c := NewHTTPCompleter(nil, nil, nil)
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

	c := NewHTTPCompleter(nil, nil, nil)
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

// TestHTTPCompleterToolSpecOnWire asserts an offered tool reaches the provider
// in the OpenAI tools shape, so the schema documented in the prompt design is
// what the model actually sees rather than fiction.
func TestHTTPCompleterToolSpecOnWire(t *testing.T) {
	var gotTools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools json.RawMessage `json:"tools"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		if err := json.Unmarshal(body.Tools, &gotTools); err != nil {
			t.Errorf("tools field: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)
	c := NewHTTPCompleter(nil, nil, nil)
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	_, err := c.Complete(context.Background(), e, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Name: "remember", Description: "propose a memory", Schema: schema}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotTools) != 1 {
		t.Fatalf("got %d tools on the wire, want 1", len(gotTools))
	}
	tool := gotTools[0]
	if tool.Type != "function" || tool.Function.Name != "remember" || tool.Function.Description != "propose a memory" {
		t.Fatalf("tool on wire = %+v", tool)
	}
	if string(tool.Function.Parameters) != string(schema) {
		t.Fatalf("parameters = %s, want %s", tool.Function.Parameters, schema)
	}
}

// TestHTTPCompleterToolCallsReturned asserts a tool-calling response populates
// Completion.ToolCalls with the arguments preserved byte for byte, and that
// FinishReason carries the provider's reason.
func TestHTTPCompleterToolCallsReturned(t *testing.T) {
	// Canonical JSON, so keel's sanitizer classifies it clean and leaves the
	// bytes untouched.
	const args = `{"a":1,"b":"two"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_7","type":"function","function":{"name":"remember","arguments":"`+
			strings.ReplaceAll(args, `"`, `\"`)+`"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	c := NewHTTPCompleter(nil, nil, nil)
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	comp, err := c.Complete(context.Background(), e, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(comp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(comp.ToolCalls))
	}
	call := comp.ToolCalls[0]
	if call.ID != "call_7" || call.Name != "remember" {
		t.Fatalf("tool call = %+v", call)
	}
	if string(call.Arguments) != args {
		t.Fatalf("arguments = %s, want byte-for-byte %s", call.Arguments, args)
	}
	if comp.FinishReason != FinishToolCalls {
		t.Fatalf("FinishReason = %q, want %q", comp.FinishReason, FinishToolCalls)
	}
}

// TestHTTPCompleterMalformedToolArgs asserts that a model emitting broken
// argument JSON still yields a Completion rather than an error: what to make
// of bad arguments is a decision for the caller that understands the tool, not
// a routing failure, and a turn must not be lost to a stray brace.
func TestHTTPCompleterMalformedToolArgs(t *testing.T) {
	cases := []struct {
		name string
		args string // JSON-escaped as it appears inside the response body
	}{
		{"stray trailing brace", `{\"path\":\"/w\"}}`},
		{"unparseable garbage", `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"remember","arguments":"`+
					tc.args+`"}}]},"finish_reason":"tool_calls"}]}`)
			}))
			defer srv.Close()

			c := NewHTTPCompleter(nil, nil, nil)
			e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
			comp, err := c.Complete(context.Background(), e, Request{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete returned %v, want a Completion despite malformed arguments", err)
			}
			if len(comp.ToolCalls) != 1 {
				t.Fatalf("got %d tool calls, want 1", len(comp.ToolCalls))
			}
			if !json.Valid(comp.ToolCalls[0].Arguments) {
				t.Fatalf("arguments %s do not parse; the raw-JSON contract requires a parseable string", comp.ToolCalls[0].Arguments)
			}
		})
	}
}

// TestHTTPCompleterFinishReason asserts FinishReason is populated on every
// completion — the ordinary case and the filtered-with-partial-content case
// alike. FinishContentFilter on a completion is what lets the layers above
// tell a model that declined from a machine that failed, so it must never be
// dropped in the mapping.
func TestHTTPCompleterFinishReason(t *testing.T) {
	cases := []struct {
		name string
		body string
		text string
		want string
	}{
		{"ordinary stop",
			`{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`,
			"hello", FinishStop},
		{"filtered with partial content",
			`{"choices":[{"message":{"content":"partial"},"finish_reason":"content_filter"}]}`,
			"partial", FinishContentFilter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := NewHTTPCompleter(nil, nil, nil)
			e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
			comp, err := c.Complete(context.Background(), e, Request{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.Text != tc.text || comp.FinishReason != tc.want {
				t.Fatalf("comp = {Text:%q FinishReason:%q}, want {%q %q}",
					comp.Text, comp.FinishReason, tc.text, tc.want)
			}
		})
	}
}

// TestHTTPCompleterKeyResolutionError pins that a key which cannot be resolved
// is a caller-visible configuration fault: the request is never sent, the
// resolver's error survives errors.Is through the wrapping, and the failure
// never reads as an unreachable machine — trying a different endpoint cannot
// conjure a key.
func TestHTTPCompleterKeyResolutionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request sent despite key resolution failure")
	}))
	defer srv.Close()

	errResolve := errors.New("endpoints[0].api_key_file: no such file")
	c := NewHTTPCompleter(nil, func(Endpoint) (string, error) { return "", errResolve }, nil)
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	_, err := c.Complete(context.Background(), e, Request{})
	if !errors.Is(err, errResolve) {
		t.Fatalf("got %v, want the resolver's error wrapped for the caller", err)
	}
	if !strings.Contains(err.Error(), "srv") {
		t.Fatalf("error should name the endpoint: %v", err)
	}
	if shouldFailover(err) {
		t.Fatal("a configuration error must not trigger failover")
	}
}

// TestHTTPCompleterNoKeySendsNoAuth pins the unauthenticated path: with no
// KeyFunc at all, and with a KeyFunc reporting "no key needed", the request
// carries no Authorization header rather than an empty or fabricated one.
func TestHTTPCompleterNoKeySendsNoAuth(t *testing.T) {
	keyFuncs := map[string]KeyFunc{
		"nil KeyFunc":      nil,
		"empty resolution": fixedKey(""),
	}
	for name, kf := range keyFuncs {
		t.Run(name, func(t *testing.T) {
			gotAuth := "unset"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			c := NewHTTPCompleter(nil, kf, nil)
			e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
			if _, err := c.Complete(context.Background(), e, Request{
				Messages: []Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if gotAuth != "" {
				t.Fatalf("Authorization header = %q, want none", gotAuth)
			}
		})
	}
}

// recordingHandler is a slog.Handler that keeps every record it receives, so a
// test can assert not just that a log line was emitted, but that it reached
// THIS handler rather than whatever slog.Default() happened to be at the time.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(name string) slog.Handler       { return h }

func (h *recordingHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// TestHTTPCompleterRepairedToolArgsReachInjectedLogger pins the wiring this
// package is responsible for: NewHTTPCompleter must hand its logger parameter
// to keel/llm, so that keel's own warnings (like the tool-call-argument-repair
// warning tested here) land on kenward's configured handler rather than on
// slog.Default() — which would mean they escape as loose text on stderr
// instead of going through whatever structured handler the operator set up.
// Without passing logger through to llm.Options.Logger, this test fails
// because the record never reaches h.
func TestHTTPCompleterRepairedToolArgsReachInjectedLogger(t *testing.T) {
	// A stray trailing brace after otherwise-valid arguments: keel's sanitizer
	// repairs this shape rather than dropping it, and logs the repair.
	const args = `{\"path\":\"/w\"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"remember","arguments":"`+
			args+`"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	h := &recordingHandler{}
	c := NewHTTPCompleter(nil, nil, slog.New(h))
	e := Endpoint{Name: "srv", BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	if _, err := c.Complete(context.Background(), e, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	rec, ok := h.find("llm: repaired malformed tool call arguments")
	if !ok {
		t.Fatal("repair warning did not reach the completer's injected logger handler")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
}
