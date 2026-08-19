package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// leoTelegramID is the account of a member with no memory of their own. They sit
// beside david in every configuration below, because the property that matters is not
// only that they are served the household's memory — that is easy — but that the
// member in the next row of the file is unaffected by their being there.
const leoTelegramID = int64(222)

// sharedOnlyConfig is simpleTestConfig with such a member added.
func sharedOnlyConfig() *config.Config {
	cfg := simpleTestConfig()
	cfg.Members = append(cfg.Members, config.MemberConfig{
		ID: "leo", Name: "Leo", TelegramID: leoTelegramID, SharedOnly: true,
		EnrolledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return cfg
}

// unitKeys is every unit this supervisor built, as a set, without healthByMember's
// collapsing of a household unit onto the group's key: the whole point here is that
// the two are different units.
func unitKeys(t *testing.T, s Supervisor) map[unitKey]State {
	t.Helper()
	hs, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	out := make(map[unitKey]State, len(hs))
	for _, h := range hs {
		out[unitKey{member: h.Member, group: h.Group}] = h.State
	}
	return out
}

// TestSimpleSharedOnlyMemberHasNoUnitOfTheirOwn is the shape of the whole feature in
// simple mode: they have a unit, it is the one that answers on the household's bot out
// of the household's memory, and the unit that would have had a private space in it
// was never built.
//
// Both halves matter. A member with no private conversation who still got a member
// unit would be a unit whose scope resolves to the household's every turn — which
// works, and puts their conversation in the same history ring as their group
// conversation would have been, because scopeUnitKey and the unit that received the
// message would then disagree about which conversation this is.
func TestSimpleSharedOnlyMemberHasNoUnitOfTheirOwn(t *testing.T) {
	h := newSimpleHarness(t, sharedOnlyConfig(), nil)
	h.start(t)
	defer h.stop(t)

	units := unitKeys(t, h.sup)
	if _, ok := units[unitKey{member: "leo"}]; ok {
		t.Error("a shared_only member was given a unit of their own; there is no private conversation for it to serve")
	}
	if state, ok := units[unitKey{member: "leo", group: true}]; !ok {
		t.Error("a shared_only member has no unit at all; their chat with kenward is unanswered")
	} else if state != StateReady {
		t.Errorf("their unit is %v, want StateReady", state)
	}
	// And the household is otherwise exactly as it was.
	if _, ok := units[unitKey{member: "david"}]; !ok {
		t.Error("the full member beside them lost their own unit")
	}
	if _, ok := units[unitKey{group: true}]; !ok {
		t.Error("the group unit is missing")
	}
}

// TestSimpleSharedOnlyMemberIsAnsweredInBothConversations is the user-visible half:
// they talk to kenward in the family group and in a chat of their own, in simple mode,
// and both are answered out of the shared space and nowhere else.
func TestSimpleSharedOnlyMemberIsAnsweredInBothConversations(t *testing.T) {
	h := newSimpleHarness(t, sharedOnlyConfig(), nil)
	h.start(t)
	defer h.stop(t)

	h.fake.Inject(transport.Inbound{ChatID: leoTelegramID, UserID: leoTelegramID, Text: "hello", MessageID: 1})
	waitFor(t, "reply in their own chat", func() bool { return len(h.fake.Sent()) >= 1 })

	h.fake.Inject(transport.Inbound{ChatID: groupChatID, UserID: leoTelegramID, Text: "hi all", MessageID: 2, IsGroup: true, Addressed: true})
	waitFor(t, "reply in the group", func() bool { return len(h.fake.Sent()) >= 2 })

	var private, shared bool
	for _, o := range h.fake.Sent() {
		switch o.ChatID {
		case leoTelegramID:
			private = true
			// The household's chain, not a member's: everything in this
			// conversation is the household's material.
			if body := replyBody(o.Text); body != "via:cloud" {
				t.Errorf("their private chat answered %q, want the household's chain via:cloud", body)
			}
		case groupChatID:
			shared = true
		}
	}
	if !private {
		t.Error("no reply in their own chat with kenward")
	}
	if !shared {
		t.Error("no reply in the household group")
	}

	// Nothing they said reached anybody's private space, in either conversation.
	for _, sp := range h.mem.searchedSpaces() {
		if strings.HasSuffix(string(sp), "-private") {
			t.Errorf("a turn for a member with no private memory searched %q", sp)
		}
	}
}

// TestSharedOnlyMemberGetsNoPod is the isolated half of "no pod in either mode",
// asserted against the one place in the codebase that decides how many pods there are.
//
// Also asserted: nothing about the pods the other members get changes. A skip written
// into that loop is one `continue` away from skipping the wrong row.
func TestSharedOnlyMemberGetsNoPod(t *testing.T) {
	cfg := isolatedTestConfig()
	cfg.Members = append(cfg.Members, config.MemberConfig{
		ID: "leo", Name: "Leo", TelegramID: 3, SharedOnly: true,
	})

	sup, err := newIsolated(cfg, isolatedTestOptions(newFakeBackend()), "linux")
	if err != nil {
		t.Fatalf("newIsolated: %v", err)
	}
	got := make(map[domain.MemberID]bool)
	groupPods := 0
	for _, p := range sup.pods {
		if p.key.group && p.key.member == "" {
			groupPods++
			continue
		}
		got[p.key.member] = true
	}
	if got["leo"] {
		t.Error("a shared_only member was given a pod; they hold no bot token, no key and no lore volume for one to carry")
	}
	for _, id := range []domain.MemberID{"david", "eve", "ana"} {
		if !got[id] {
			t.Errorf("member %q lost their pod", id)
		}
	}
	if groupPods != 1 {
		t.Errorf("group pods = %d, want exactly 1: it is where both of their conversations run", groupPods)
	}
}

// TestIsolatedGroupPodAnswersASharedOnlyMemberPrivately is the other half of the same
// fact. Their pod does not exist, so somebody else's process has to hold their private
// chat with kenward, and it is the one holding the household's bot.
func TestIsolatedGroupPodAnswersASharedOnlyMemberPrivately(t *testing.T) {
	cfg := isolatedTestConfig()
	cfg.Household.Agents = config.AgentsShared
	cfg.Members = append(cfg.Members, config.MemberConfig{
		ID: "leo", Name: "Leo", TelegramID: 3, SharedOnly: true,
	})

	h := newSingleHarness(t, cfg, func(o *SingleOptions) {
		o.Member = ""
		o.Group = true
	})
	h.start(t, "group")
	defer h.stop(t)

	units := unitKeys(t, h.sup)
	if _, ok := units[unitKey{member: "leo", group: true}]; !ok {
		t.Fatal("the group pod does not serve a shared_only member's chat with kenward; nothing else can, because they have no pod")
	}
	// Under one shared agent, which is what makes this the interesting case: the
	// household scope did not exist here at all before, so this unit is reached for
	// a reason that is a fact about the member rather than about household.agents.
	if cfg.AgentPerMember() {
		t.Fatal("this test is meant to run under one shared agent")
	}
	// And no unit for anybody else's private chat, which is what one shared agent
	// means: every full member's own conversation belongs to their own pod.
	for _, id := range []domain.MemberID{"david", "eve"} {
		if _, ok := units[unitKey{member: id, group: true}]; ok {
			t.Errorf("the group pod built a household unit for %q, who has an assistant of their own", id)
		}
	}
}

// TestSingleRefusesASharedOnlyMember: there is no pod for them to be, so a process
// told to be one is refused rather than started empty.
//
// Started empty is the failure worth naming. The pod would poll a bot token it does
// not have, serve nobody, and report itself healthy — a green check over a member
// whose messages are going to a process that was never meant to exist.
func TestSingleRefusesASharedOnlyMember(t *testing.T) {
	cfg := isolatedTestConfig()
	cfg.Members = append(cfg.Members, config.MemberConfig{
		ID: "leo", Name: "Leo", TelegramID: 3, SharedOnly: true,
	})

	_, err := NewSingle(cfg, SingleOptions{Member: "leo"})
	if err == nil {
		t.Fatal("NewSingle accepted a shared_only member; there is no pod for them to be")
	}
	for _, want := range []string{"leo", "shared_only", "group pod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q; an operator has to be told where their conversations do run", err, want)
		}
	}
}
