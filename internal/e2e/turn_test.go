package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/capture"
)

// TestDirectMessageRoundTripsAndPromptCarriesBothMemories drives one member's
// message through the whole turn — scope resolution, retrieval, prompt assembly,
// routing over the real pool, reply — and checks that what the endpoint received
// is what retrieval found.
func TestDirectMessageRoundTripsAndPromptCarriesBothMemories(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.mem.seed(davidSpace, entry("p1", "Bin day", "Recycling goes out on Tuesday night.", "seasonal"))
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Recycling goes out Tuesday night.", FinishReason: "stop"}
	})

	h.start()
	h.tr.InjectText(davidChatID, davidTelegramID, "when do the bins go out?", false)
	sent := h.waitForReply(davidChatID, 1)

	if got := sent[0].Text; got != "Recycling goes out Tuesday night." {
		t.Errorf("reply = %q, want the model's text", got)
	}
	if sent[0].ReplyTo != 0 {
		t.Errorf("direct reply quoted message %d; direct chats do not quote", sent[0].ReplyTo)
	}

	// The scope's Read set is [private, shared] and nothing else. Another
	// member's private space appearing here would mean scope resolution or the
	// supervisor's per-unit wiring had crossed two members over.
	searched := h.mem.searchedSpaces()
	if len(searched) != 2 {
		t.Fatalf("searched %v, want exactly the member's two spaces", searched)
	}
	if !containsSpace(searched, davidSpace) || !containsSpace(searched, sharedSpace) {
		t.Errorf("searched %v, want %s and %s", searched, davidSpace, sharedSpace)
	}
	if containsSpace(h.mem.touchedSpaces(), meiSpace) {
		t.Errorf("David's turn touched %s; one member's turn must never reach another's private space", meiSpace)
	}

	req := h.local.last(t)
	system := req.System()
	// Retrieval is concurrent by design, one goroutine per space, so the order
	// the two searches happen to be recorded in is not a property worth
	// asserting. What is guaranteed — "ordered: primary first" — is observable
	// where it matters: in the prompt the model actually reads.
	privateAt := strings.Index(system, "David's private memory")
	sharedAt := strings.Index(system, "the household's shared memory")
	switch {
	case privateAt < 0:
		t.Error("system prompt has no private memory section")
	case sharedAt < 0:
		t.Error("system prompt has no shared memory section")
	case privateAt > sharedAt:
		t.Error("shared memory is rendered before private memory; scope order is primary first")
	}
	if !strings.Contains(system, "Recycling goes out on Tuesday night.") {
		t.Error("system prompt does not carry the private entry that retrieval returned")
	}
	if !strings.Contains(system, "The side gate code is 4417.") {
		t.Error("system prompt does not carry the shared entry that retrieval returned")
	}
	if strings.Contains(system, "Appointment on the 3rd.") {
		t.Errorf("system prompt carries an entry from %s, which this scope never searched", meiSpace)
	}
	if req.UserText() != "when do the bins go out?" {
		t.Errorf("user message = %q, want the member's own words", req.UserText())
	}
	// A direct conversation is offered both tools: remember, and publish for the
	// promotion flow. The group is offered only remember — asserted where the group
	// turn is tested.
	if len(req.Tools) != 2 || req.Tools[0].Function.Name != "remember" || req.Tools[1].Function.Name != "publish" {
		t.Errorf("tools on the wire = %+v, want remember and publish", req.Tools)
	}
}

// TestGroupConversationSearchesOnlySharedSpace is the most important assertion
// in this package. The household chat must not be a way to read a private
// memory, and the check is on which spaces were reached rather than on what the
// model said: a group turn that searched a private space and happened to find
// nothing has already broken the invariant.
func TestGroupConversationSearchesOnlySharedSpace(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.mem.seed(davidSpace, entry("p1", "David's PIN", "The card PIN is 9931."))
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The side gate code is 4417.", FinishReason: "stop"}
	})

	h.start()
	h.tr.InjectText(groupChatID, davidTelegramID, "what's the gate code?", true)
	h.waitForReply(groupChatID, 1)

	// Every call of every kind, not just searches: a write into a private space
	// from the group would be just as bad as a read out of one.
	for _, sp := range h.mem.touchedSpaces() {
		if sp != sharedSpace {
			t.Errorf("group turn touched space %s; a group scope may only ever reach %s", sp, sharedSpace)
		}
	}
	if searched := h.mem.searchedSpaces(); len(searched) != 1 {
		t.Errorf("group turn made %d space searches (%v), want exactly one", len(searched), searched)
	}

	system := h.local.last(t).System()
	if strings.Contains(system, "9931") || strings.Contains(system, "Appointment on the 3rd.") {
		t.Error("group prompt carries a private entry; the household chat must not see one")
	}
	if !strings.Contains(system, "You cannot see any") {
		t.Error("group prompt is missing the group scope disclosure")
	}
}

// TestUnknownSenderGetsNothingAtAll checks the silence a stranger is owed. Not a
// refusal and not an error: replying at all confirms to whoever found the bot's
// username that it serves a real household.
func TestUnknownSenderGetsNothingAtAll(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()

	h.tr.InjectText(strangerChatID, strangerUserID, "hello? who is this?", false)
	// A message from a real member behind it. The mux dispatches in order, so
	// once this one has been answered the stranger's has certainly been routed
	// — which makes the assertions below about absence, not about timing.
	h.tr.InjectText(davidChatID, davidTelegramID, "morning", false)
	h.waitForReply(davidChatID, 1)

	if got := h.sentTo(strangerChatID); len(got) != 0 {
		t.Errorf("stranger received %d message(s): %+v; an unenrolled sender gets silence", len(got), got)
	}
	// One request, David's. A second would mean the stranger's message reached
	// retrieval and an inference endpoint before being rejected.
	if n := h.local.count(); n != 1 {
		t.Errorf("provider saw %d requests, want 1; the stranger's message must never reach a model", n)
	}
	if n := len(h.mem.recorded()); n != 2 {
		t.Errorf("memory saw %d calls, want 2 (David's two spaces); the stranger's message must never reach lore", n)
	}
}

// TestLocalOnlyChainRefusesInsteadOfReachingCloud is the privacy promise the
// tier chain exists to make: when nothing in the chain answers, the node says so
// rather than widening. The assertion that the cloud endpoint saw zero requests
// is the one that matters — a refusal that had already leaked the prompt would
// still read correctly to the member.
func TestLocalOnlyChainRefusesInsteadOfReachingCloud(t *testing.T) {
	h := newHarness(t, harnessOptions{
		memberTiers:    []string{"local"},
		householdTiers: []string{"local"},
		localDown:      true,
		withCloud:      true,
	})
	h.start()

	h.tr.InjectText(davidChatID, davidTelegramID, "is the heating on?", false)
	sent := h.waitForReply(davidChatID, 1)

	text := sent[0].Text
	for _, want := range []string{"`local`", "`attic`", "won't send it anywhere else"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q does not contain %q", text, want)
		}
	}
	if h.cloud.count() != 0 {
		t.Errorf("cloud endpoint received %d requests; a local-only chain must never widen to a provider", h.cloud.count())
	}
}

// TestCaptureWritesNothingWithoutConfirmation covers the rule that has no
// configuration flag: a proposed memory is written only when the member who
// spoke presses the button that says so.
func TestCaptureWritesNothingWithoutConfirmation(t *testing.T) {
	proposing := func(wireRequest) providerReply {
		return providerReply{
			Text:         "Noted.",
			FinishReason: "tool_calls",
			ToolCalls: []providerToolCall{{
				Name:      "remember",
				Arguments: rememberArgs("Boiler serviced", "The boiler was serviced in March.", "personal"),
			}},
		}
	}

	t.Run("declined", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.local.setReply(proposing)
		h.tr.AnswerWithChoice(capture.ChoiceDecline)
		h.start()

		h.tr.InjectText(davidChatID, davidTelegramID, "the boiler was serviced", false)
		waitFor(t, "the capture question", func() bool { return len(h.tr.Asked()) >= 1 })
		// A second turn is the barrier: the unit serialises turns, so its reply
		// cannot arrive until the first turn, capture included, has finished.
		h.tr.InjectText(davidChatID, davidTelegramID, "anything else?", false)
		h.waitForReply(davidChatID, 2)

		if puts := h.mem.putCalls(); len(puts) != 0 {
			t.Errorf("declined proposal wrote %+v; a decline must write nothing", puts)
		}
	})

	t.Run("timed out", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.local.setReply(proposing)
		// The Fake's default is already a timeout; saying so makes the intent
		// of the test legible rather than incidental.
		h.tr.AnswerWithTimeout()
		h.start()

		h.tr.InjectText(davidChatID, davidTelegramID, "the boiler was serviced", false)
		waitFor(t, "the capture question", func() bool { return len(h.tr.Asked()) >= 1 })
		h.tr.InjectText(davidChatID, davidTelegramID, "anything else?", false)
		h.waitForReply(davidChatID, 2)

		if puts := h.mem.putCalls(); len(puts) != 0 {
			t.Errorf("expired question wrote %+v; a timeout is a decline, never an accept", puts)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.local.setReply(proposing)
		h.tr.AnswerWithChoice(capture.ChoicePersonal)
		h.start()

		h.tr.InjectText(davidChatID, davidTelegramID, "the boiler was serviced", false)
		// Two messages: the model's reply and capture's confirmation.
		h.waitForReply(davidChatID, 2)

		puts := h.mem.putCalls()
		if len(puts) != 1 {
			t.Fatalf("accepted proposal produced %d writes, want 1: %+v", len(puts), puts)
		}
		if got := puts[0].Spaces; len(got) != 1 || got[0] != davidSpace {
			t.Errorf("wrote to %v, want the member's private space %s", got, davidSpace)
		}
		if puts[0].Title != "Boiler serviced" {
			t.Errorf("wrote title %q, want the proposal's", puts[0].Title)
		}
		q, ok := h.tr.LastAsked()
		if !ok {
			t.Fatal("no capture question was asked")
		}
		if q.AllowedUserID != davidTelegramID {
			t.Errorf("question was answerable by %d, want only %d; another member must not be able to route this capture",
				q.AllowedUserID, davidTelegramID)
		}
	})
}

// TestGroupCaptureNeverOffersAPersonalDestination checks the button set, not the
// outcome. In the household chat a personal destination must not be offered at
// all, whatever the model proposed — a choice that is never rendered cannot be
// pressed, and that is the only durable form of this guarantee.
func TestGroupCaptureNeverOffersAPersonalDestination(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{
			Text:         "Got it.",
			FinishReason: "tool_calls",
			ToolCalls: []providerToolCall{{
				Name: "remember",
				// The model asks for a private destination on purpose.
				Arguments: rememberArgs("Gate code changed", "The side gate code is now 5520.", "personal"),
			}},
		}
	})
	h.tr.AnswerWithChoice(capture.ChoiceShared)
	h.start()

	h.tr.InjectText(groupChatID, davidTelegramID, "the gate code changed to 5520", true)
	h.waitForReply(groupChatID, 2)

	asked := h.tr.Asked()
	if len(asked) != 1 {
		t.Fatalf("asked %d questions, want 1", len(asked))
	}
	for _, c := range asked[0].Choices {
		if c.ID == capture.ChoicePersonal {
			t.Errorf("group capture offered %q; the household chat may never write to a private space", c.ID)
		}
	}
	puts := h.mem.putCalls()
	if len(puts) != 1 {
		t.Fatalf("group capture produced %d writes, want 1: %+v", len(puts), puts)
	}
	if got := puts[0].Spaces; len(got) != 1 || got[0] != sharedSpace {
		t.Errorf("group capture wrote to %v, want %s", got, sharedSpace)
	}
	for _, sp := range h.mem.touchedSpaces() {
		if sp == davidSpace || sp == meiSpace {
			t.Errorf("group capture reached private space %s", sp)
		}
	}
}

// TestStopDrainsTheTurnInFlight checks that shutdown finishes the conversation
// it is in the middle of. Cutting a turn in half means a member who asked
// something gets nothing back and never learns why, and in the capture path it
// means a confirmed write that never happened.
func TestStopDrainsTheTurnInFlight(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.local.setDelay(300 * time.Millisecond)
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Answered after the drain began.", FinishReason: "stop"}
	})
	h.start()

	h.tr.InjectText(davidChatID, davidTelegramID, "slow question", false)

	// Wait until the turn is genuinely mid-flight: the endpoint has the request
	// and has not answered yet.
	select {
	case <-h.local.received:
	case <-time.After(waitTimeout):
		t.Fatal("provider never received the request")
	}

	if err := h.stop(); err != nil {
		t.Errorf("Stop returned %v, want a clean drain", err)
	}

	sent := h.sentTo(davidChatID)
	if len(sent) != 1 {
		t.Fatalf("after draining, chat %d had %d messages, want 1: %+v", davidChatID, len(sent), sent)
	}
	if sent[0].Text != "Answered after the drain began." {
		t.Errorf("reply = %q, want the turn's own answer", sent[0].Text)
	}
}

// TestSupervisorServesOneUnitPerMemberPlusTheGroup is a wiring smoke check: the
// household described by one YAML file becomes exactly the units it names, with
// no unit created for anyone it does not.
func TestSupervisorServesOneUnitPerMemberPlusTheGroup(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()

	health, err := h.sup.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(health) != 3 {
		t.Errorf("health reported %d units, want 3 (two members and the group): %+v", len(health), health)
	}
	if got := h.cfg.DomainHousehold().Shared; got != sharedSpace {
		t.Errorf("household shared space = %q, want %q", got, sharedSpace)
	}
	for _, m := range h.cfg.DomainMembers() {
		if m.Private == sharedSpace {
			t.Errorf("member %s's private space is the shared one", m.ID)
		}
	}
}
