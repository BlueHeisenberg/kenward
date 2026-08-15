package assistant

import (
	"encoding/json"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

func call(name, args string) routing.ToolCall {
	return routing.ToolCall{ID: "tc-1", Name: name, Arguments: json.RawMessage(args)}
}

func TestExtractProposal(t *testing.T) {
	valid := `{"title": "Bin day", "body": "Bins go out Thursday night.", "domain": "household/logistics", "confidence": "validated", "markers": ["UPDATED"], "target": "shared"}`

	tests := []struct {
		name       string
		calls      []routing.ToolCall
		wantNil    bool
		wantTarget capture.Target
		wantWarn   bool
	}{
		{
			name:    "no calls",
			calls:   nil,
			wantNil: true,
		},
		{
			name:       "valid call",
			calls:      []routing.ToolCall{call("remember", valid)},
			wantTarget: capture.TargetShared,
		},
		{
			name:     "invalid json is dropped",
			calls:    []routing.ToolCall{call("remember", `{broken`)},
			wantNil:  true,
			wantWarn: true,
		},
		{
			name:     "missing title is dropped",
			calls:    []routing.ToolCall{call("remember", `{"body": "B", "target": "shared"}`)},
			wantNil:  true,
			wantWarn: true,
		},
		{
			name:     "missing body is dropped",
			calls:    []routing.ToolCall{call("remember", `{"title": "T", "target": "shared"}`)},
			wantNil:  true,
			wantWarn: true,
		},
		{
			name:       "unknown target becomes unsure with a warning",
			calls:      []routing.ToolCall{call("remember", `{"title": "T", "body": "B", "target": "global"}`)},
			wantTarget: capture.TargetUnsure,
			wantWarn:   true,
		},
		{
			name:       "missing target becomes unsure",
			calls:      []routing.ToolCall{call("remember", `{"title": "T", "body": "B"}`)},
			wantTarget: capture.TargetUnsure,
			wantWarn:   true,
		},
		{
			name:     "unknown tool is dropped",
			calls:    []routing.ToolCall{call("forget", `{"title": "T"}`)},
			wantNil:  true,
			wantWarn: true,
		},
		{
			name: "second remember call ignored",
			calls: []routing.ToolCall{
				call("remember", valid),
				call("remember", `{"title": "Other", "body": "B", "target": "personal"}`),
			},
			wantTarget: capture.TargetShared,
			wantWarn:   true,
		},
		{
			name: "unknown tool then valid remember",
			calls: []routing.ToolCall{
				call("forget", `{}`),
				call("remember", valid),
			},
			wantTarget: capture.TargetShared,
			wantWarn:   true,
		},
		{
			name:       "trailing junk after the json is tolerated",
			calls:      []routing.ToolCall{call("remember", `{"title": "T", "body": "B", "target": "shared"} and that is all`)},
			wantTarget: capture.TargetShared,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, warn := extractProposal(tc.calls)
			if tc.wantNil {
				if p != nil {
					t.Fatalf("got proposal %+v, want none", p)
				}
			} else {
				if p == nil {
					t.Fatal("got no proposal, want one")
				}
				if p.Target != tc.wantTarget {
					t.Errorf("target %v, want %v", p.Target, tc.wantTarget)
				}
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn %q, wantWarn=%v", warn, tc.wantWarn)
			}
		})
	}
}

func TestExtractProposalPassesFieldsThrough(t *testing.T) {
	p, warn := extractProposal([]routing.ToolCall{call("remember",
		`{"title": "Bin day", "body": "Thursday.", "domain": "household/logistics", "confidence": "hardened", "markers": ["UPDATED", "CONTEXT"], "target": "shared"}`)})
	if warn != "" {
		t.Fatalf("unexpected warning %q", warn)
	}
	d := p.Draft
	if d.Title != "Bin day" || d.Body != "Thursday." || d.Domain != "household/logistics" ||
		d.Confidence != "hardened" || len(d.Markers) != 2 {
		t.Errorf("draft %+v did not pass fields through verbatim", d)
	}
}

func TestExtractProposalDefaultsConfidence(t *testing.T) {
	p, _ := extractProposal([]routing.ToolCall{call("remember", `{"title": "T", "body": "B", "target": "shared"}`)})
	if p == nil || p.Draft.Confidence != "provisional" {
		t.Fatalf("proposal %+v, want provisional confidence default", p)
	}
}

func TestRememberSchemaIsValidJSON(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(rememberSchema), &schema); err != nil {
		t.Fatalf("remember schema is not valid JSON: %v", err)
	}
	req, ok := schema["required"].([]any)
	if !ok || len(req) != 3 {
		t.Fatalf("schema required = %v, want [title body target]", schema["required"])
	}
	tools := rememberTools()
	if len(tools) != 1 || tools[0].Name != "remember" {
		t.Fatalf("tools = %+v, want exactly the remember tool", tools)
	}
}

func TestSanitizeReplyStripsStrayBlocks(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantReply string
		wantWarn  bool
	}{
		{
			name:      "plain reply untouched",
			text:      "Just a reply.",
			wantReply: "Just a reply.",
		},
		{
			name:      "improvised block stripped, not honoured",
			text:      "Noted.\n\n```remember\n{\"title\": \"T\", \"body\": \"B\", \"target\": \"shared\"}\n```",
			wantReply: "Noted.",
			wantWarn:  true,
		},
		{
			name:      "crlf block stripped",
			text:      "Noted.\r\n\r\n```remember\r\n{}\r\n```",
			wantReply: "Noted.",
			wantWarn:  true,
		},
		{
			name:      "block only leaves an empty reply",
			text:      "```remember\n{}\n```",
			wantReply: "",
			wantWarn:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply, warn := sanitizeReply(tc.text)
			if reply != tc.wantReply {
				t.Errorf("reply %q, want %q", reply, tc.wantReply)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn %q, wantWarn=%v", warn, tc.wantWarn)
			}
		})
	}
}
