package assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// The household group is a conversation kenward is a participant in, not the
// counterparty to. It listens to all of it and answers only the part addressed to it.
//
// Every other conversation it runs — a member's private chat, a member's own agent
// wherever that agent lives — is one it is the counterparty to, and every message in
// one of those is addressed to it by definition.

// groupAside is an ordinary message between two members of the household, in the
// household chat, with nothing in it that names kenward. groupInbound is its
// addressed twin.
func groupAside(text string) transport.Inbound {
	in := groupInbound(text)
	in.Addressed = false
	return in
}

// An addressed message in the group is an ordinary turn and always was. The three
// wire shapes that set the flag are internal/transport's to tell apart; here they
// are one fact, and the assertion is that the gate lets it through.
func TestAddressedGroupMessageProducesATurn(t *testing.T) {
	for _, text := range []string{
		"@kenward_bot what did we decide about the boiler?",
		"what did we decide about the boiler?", // a reply to one of kenward's messages
		"/reset@kenward_bot",
	} {
		t.Run(text, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.unit.Handle(context.Background(), groupInbound(text)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(rig.router.reqs) != 1 {
				t.Errorf("router calls = %d, want 1", len(rig.router.reqs))
			}
			if got := rig.tr.sentTexts(); len(got) != 1 {
				t.Errorf("sent %d messages, want 1: %q", len(got), got)
			}
		})
	}
}

func TestGroupAsideProducesNoReplyAndNoModelCall(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), groupAside("are you picking Mei up or am I?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The router assertion is the one that matters. A gate that merely swallowed the
	// reply would still have spent a model call, its latency and its money on every
	// sentence the family said to each other.
	if n := len(rig.router.reqs); n != 0 {
		t.Errorf("router was called %d times for an unaddressed group message; want 0", n)
	}
	if got := rig.tr.sentTexts(); len(got) != 0 {
		t.Errorf("sent %d messages for an unaddressed group message: %q", len(got), got)
	}
	if n := rig.tr.askCount(); n != 0 {
		t.Errorf("capture asked %d questions about an unaddressed group message; want 0", n)
	}
	if n := len(rig.mem.searches); n != 0 {
		t.Errorf("retrieval ran %d searches for an unaddressed group message; want 0", n)
	}
}

// Hearing and answering are different things. The aside is context for the question
// that comes later, so it has to be in the ring when that question is assembled.
func TestGroupAsideStillReachesTheHistoryRing(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	const aside = "we said we would replace the boiler in the spring, not now"
	if err := rig.unit.Handle(context.Background(), groupAside(aside)); err != nil {
		t.Fatalf("Handle (aside): %v", err)
	}
	// Heard, and not answered. Both halves, because the ring is only interesting
	// once the reply is gone: a turn that answers the aside would put it in history
	// as a side effect and prove nothing about listening.
	if n := len(rig.router.reqs); n != 0 {
		t.Fatalf("router was called %d times for the aside; want 0", n)
	}
	if got := rig.tr.sentTexts(); len(got) != 0 {
		t.Fatalf("the aside was answered with %q", got)
	}
	if err := rig.unit.Handle(context.Background(), groupInbound("@kenward_bot what did we decide about the boiler?")); err != nil {
		t.Fatalf("Handle (question): %v", err)
	}
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("the addressed question never reached the router")
	}
	var seen bool
	for _, m := range req.Messages {
		if strings.Contains(m.Content, aside) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the unaddressed message never reached the prompt; messages were %+v", req.Messages)
	}
}

// A private chat is addressed by definition: kenward is the counterparty, there is
// nobody else in the room, and nothing here changes for it.
func TestPrivateChatAnswersEveryMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope domain.Scope
	}{
		{"member's own agent", testDirectScope()},
		{"kenward one to one", testHouseholdScope()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(tc.scope), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			in := transport.Inbound{ChatID: testMemberChat, UserID: testUserID, Text: "morning", MessageID: 1}
			if err := rig.unit.Handle(context.Background(), in); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(rig.router.reqs) != 1 {
				t.Errorf("router calls = %d, want 1", len(rig.router.reqs))
			}
			if got := rig.tr.sentTexts(); len(got) != 1 {
				t.Errorf("sent %d messages, want 1: %q", len(got), got)
			}
		})
	}
}

// The gate is not "is this a group". It is "is this a conversation kenward shares
// with several people who talk to each other" — which is scope kind, and only ever
// ScopeGroup. A member's own agent in a group is that member's conversation wherever
// it happens, so it answers everything.
//
// scope.Resolve cannot produce this pairing today: a member's bot in any group chat
// is ErrNotEnrolled. This is therefore a unit test of the predicate and not an
// end-to-end one, and it is here so the gate stays keyed to the scope when that
// changes.
func TestMembersOwnAgentInAGroupAnswersEveryMessage(t *testing.T) {
	sc := testDirectScope()
	sc.ChatID = testGroupChat
	rig, err := newTestRig(fixedResolver(sc), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), groupAside("are you picking Mei up or am I?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(rig.router.reqs) != 1 {
		t.Errorf("router calls = %d, want 1: a member's own agent answers its own group", len(rig.router.reqs))
	}
	if got := rig.tr.sentTexts(); len(got) != 1 {
		t.Errorf("sent %d messages, want 1: %q", len(got), got)
	}
}
