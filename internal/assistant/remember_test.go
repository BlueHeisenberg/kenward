package assistant

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/transport"
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

// TestExtractProposalDefaultsDomain is the regression test for the production
// defect: a model that omits domain (declared required in the schema, but a model
// can omit a required field too) must not lose the proposal after the member has
// already confirmed. Real lore rejects a write with an empty domain, unrecoverably,
// so extractProposal must never hand one downstream.
func TestExtractProposalDefaultsDomain(t *testing.T) {
	p, _ := extractProposal([]routing.ToolCall{call("remember", `{"title": "T", "body": "B", "target": "shared"}`)})
	if p == nil || p.Draft.Domain == "" {
		t.Fatalf("proposal %+v, want a non-empty domain default", p)
	}
}

// TestRememberWithoutDomainStillWritesAfterConfirmation is the end-to-end version of
// the same regression: a model tool call that omits domain, once the member presses
// confirm, must still reach memory.Put successfully rather than failing unrecoverably
// after the member has already committed. The fake memory in fakes_test.go rejects an
// empty domain exactly as real lore does, so this fails without the defaulting fix
// in extractProposal.
func TestRememberWithoutDomainStillWritesAfterConfirmation(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePersonal, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "I'll remember that.",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "Coffee order", "body": "David drinks oat-milk flat whites.", "target": "personal"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("I only drink oat-milk flat whites")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if rig.mem.putCount() != 1 {
		t.Fatalf("wrote %d entries, want 1 after the member confirmed", rig.mem.putCount())
	}
	if p := rig.mem.puts[0]; p.draft.Domain == "" {
		t.Errorf("wrote draft with empty domain: %+v", p.draft)
	}
}

func TestRememberSchemaIsValidJSON(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(rememberSchema), &schema); err != nil {
		t.Fatalf("remember schema is not valid JSON: %v", err)
	}
	req, ok := schema["required"].([]any)
	if !ok || len(req) != 4 {
		t.Fatalf("schema required = %v, want [title body domain target]", schema["required"])
	}
	var pub map[string]any
	if err := json.Unmarshal([]byte(publishSchema), &pub); err != nil {
		t.Fatalf("publish schema is not valid JSON: %v", err)
	}
	// The publish tool takes a title and nothing else. An id property would be an id
	// arriving from the model, which is where member text arrives.
	props, _ := pub["properties"].(map[string]any)
	if len(props) != 1 {
		t.Fatalf("publish schema properties = %v, want only title", props)
	}
	if _, ok := props["title"]; !ok {
		t.Fatalf("publish schema properties = %v, want title", props)
	}

	// The reminder tools are offered in both scopes; publish is the only one gated,
	// because a group conversation has nothing to publish from.
	if got, want := toolNames(toolSpecs(testDirectScope())), []string{"remember", "publish", "remind", "unremind"}; !slices.Equal(got, want) {
		t.Fatalf("direct tools = %v, want %v", got, want)
	}
	if got, want := toolNames(toolSpecs(testGroupScope())), []string{"remember", "remind", "unremind"}; !slices.Equal(got, want) {
		t.Fatalf("group tools = %v, want %v — publish must not be offered where it can only be dropped", got, want)
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

// TestSecondRememberCallIsToldToTheMember is the second half of the defect the
// truthfulness paragraph fixes, and it is the half no prompt can reach.
//
// In the live run that produced it, a member's private chat with kenward was sent two
// facts in one message. The model made two remember calls; extractProposal kept the
// first and logged "model made more than one remember call; using the first"; the
// member was asked about one thing and never told about the other, which was never
// proposed, never written, and never mentioned again. From inside the chat that turn is
// indistinguishable from a turn about one fact — the silent version of exactly the
// misunderstanding the prompt change is there to prevent.
//
// One question per turn is the right rule and is not what changes here. What changes is
// that the drop is now something the member can see.
func TestSecondRememberCallIsToldToTheMember(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testHouseholdScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{TimedOut: true}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "Noted.",
			ToolCalls: []routing.ToolCall{
				call("remember", `{"title": "Stopcock", "body": "The stopcock is under the stairs.", "domain": "household/logistics", "target": "shared"}`),
				call("remember", `{"title": "Key tag", "body": "The spare key tag reads fenwick-2260.", "domain": "household/logistics", "target": "shared"}`),
			},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("the stopcock is under the stairs, and the spare key tag says fenwick-2260")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// One question, as the budget says. The rule is not what is under test.
	if got := rig.tr.askCount(); got != 1 {
		t.Fatalf("asked %d questions, want 1: the per-turn budget is not what this test changes", got)
	}
	notice := lang.For("").OnlyOneProposal
	var found bool
	for _, s := range rig.tr.sentTextsRaw() {
		if strings.Contains(s, notice) {
			found = true
		}
	}
	if !found {
		t.Errorf("the member was never told that a second proposal was dropped.\nsent: %q\nwant one of them to contain %q\n\nA turn where somebody named two things and was asked about one, with no mention of the other, is indistinguishable from a turn about one thing.",
			rig.tr.sentTextsRaw(), notice)
	}
	// Nothing was written: the question timed out, which is a decline, and the
	// dropped proposal was never a question at all. The notice must never be read as
	// an announcement that something landed.
	if got := rig.mem.putCount(); got != 0 {
		t.Errorf("wrote %d entries; nothing was confirmed, so nothing may be stored", got)
	}
}

// TestSingleRememberCallSaysNothingExtra is the other half: the notice is for a real
// drop and not decoration on every capture turn. A turn that proposed one thing had
// nothing dropped, and a line saying otherwise would be its own small untruth.
func TestSingleRememberCallSaysNothingExtra(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testHouseholdScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{TimedOut: true}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text:         "Noted.",
			ToolCalls:    []routing.ToolCall{call("remember", `{"title": "Stopcock", "body": "The stopcock is under the stairs.", "domain": "household/logistics", "target": "shared"}`)},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("the stopcock is under the stairs")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	notice := lang.For("").OnlyOneProposal
	for _, s := range rig.tr.sentTextsRaw() {
		if strings.Contains(s, notice) {
			t.Errorf("a one-proposal turn told the member something was dropped: %q", s)
		}
	}
}
