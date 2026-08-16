package scope_test

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// The household used by the table below. Two ordinary members, one who has not claimed
// their invite yet, and two whose Telegram ids are adjacent so that an off-by-one in a
// lookup would show up as a member being served someone else's memory.
const (
	groupChatID = int64(-1001234567890)

	davidID   = int64(1001)
	mariaID   = int64(1002)
	adjacentA = int64(5000)
	adjacentB = int64(5001)
)

func household() *config.Config {
	return &config.Config{
		Mode: config.ModeSimple,
		Household: config.HouseholdConfig{
			Name:        "Home",
			SharedSpace: "household",
			GroupChatID: groupChatID,
			Tiers:       []string{"local", "cloud"},
		},
		Telegram: config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", TelegramID: davidID, PrivateSpace: "david-private", Tiers: []string{"local"}},
			{ID: "maria", Name: "Maria", TelegramID: mariaID, PrivateSpace: "maria-private", Tiers: []string{"local", "cloud"}},
			{ID: "sam", Name: "Sam", TelegramID: 0, PrivateSpace: "sam-private", Tiers: []string{"local"}},
			{ID: "ann", Name: "Ann", TelegramID: adjacentA, PrivateSpace: "ann-private", Tiers: []string{"local"}},
			{ID: "bea", Name: "Bea", TelegramID: adjacentB, PrivateSpace: "bea-private", Tiers: []string{"cloud"}},
		},
	}
}

// privateSpaces lists every private space in a configuration, so a test can assert that
// none of them appears somewhere it must not.
//
// A private space that has been configured to be the shared space is left out: that
// collision is a configuration fault, caught by config validation, and no resolution
// rule can un-publish a space the operator has declared shared. The property that still
// holds there — and is asserted separately — is that a group scope reads and writes
// exactly household.shared_space, whatever else that name has been given to.
func privateSpaces(cfg *config.Config) []domain.SpaceID {
	shared := domain.SpaceID(cfg.Household.SharedSpace)
	out := make([]domain.SpaceID, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		if p := domain.SpaceID(m.PrivateSpace); p != shared {
			out = append(out, p)
		}
	}
	return out
}

// enrolledIDs is the set of Telegram accounts a configuration has claimed. Zero is
// never in it, however many members are still waiting to claim their invite.
func enrolledIDs(cfg *config.Config) map[int64]bool {
	out := make(map[int64]bool, len(cfg.Members))
	for _, m := range cfg.Members {
		if m.TelegramID != 0 {
			out[m.TelegramID] = true
		}
	}
	return out
}

func direct(userID int64) transport.Inbound {
	return transport.Inbound{
		ChatID: userID, // Telegram gives a private chat the user's own id.
		UserID: userID,
		Text:   "hello",
		At:     time.Unix(0, 0),
	}
}

func inGroup(chatID, userID int64) transport.Inbound {
	return transport.Inbound{ChatID: chatID, UserID: userID, Text: "hello", IsGroup: true, At: time.Unix(0, 0)}
}

type want struct {
	kind     domain.ScopeKind
	memberID domain.MemberID // empty means Member must be nil
	write    domain.SpaceID
	read     []domain.SpaceID
	tiers    []string
	chatID   int64
}

func TestResolve(t *testing.T) {
	// A household with no group configured at all: group_chat_id is still zero.
	noGroup := household()
	noGroup.Household.GroupChatID = 0

	// A misconfiguration validation rejects, tested here anyway because Resolve is the
	// last line of defence and is also called with configurations it did not validate:
	// a member whose private space *is* the shared space.
	collided := household()
	collided.Members[0].PrivateSpace = collided.Household.SharedSpace

	// A household whose shared space is empty.
	noShared := household()
	noShared.Household.SharedSpace = ""

	// The same household with an agent for every member, which is the only
	// arrangement in which a private conversation with kenward exists. It is
	// isolated mode because one agent each needs one bot each.
	perMember := household()
	perMember.Mode = config.ModeIsolated
	perMember.Household.Agents = config.AgentsPerMember

	// And the combination the file may state and the node cannot deliver: one agent
	// each behind simple mode's single bot. It must resolve exactly as one agent
	// does, because the alternative is every member's private chat silently becoming
	// the household's.
	perMemberSimple := household()
	perMemberSimple.Household.Agents = config.AgentsPerMember

	tests := []struct {
		name string
		cfg  *config.Config
		// bot is which of the household's bots the message arrived on. The zero
		// value is the household's own, which under one agent is also everybody's
		// own assistant — so every case written before the third scope existed
		// keeps meaning exactly what it meant.
		bot     domain.MemberID
		in      transport.Inbound
		wantErr bool
		want    want
	}{
		{
			name:    "nil config is fail-closed",
			cfg:     nil,
			in:      direct(davidID),
			wantErr: true,
		},
		{
			name:    "unknown user in a direct chat",
			cfg:     household(),
			in:      direct(999999),
			wantErr: true,
		},
		{
			name:    "unknown user whose id is zero",
			cfg:     household(),
			in:      direct(0),
			wantErr: true,
		},
		{
			name:    "unenrolled member: telegram_id 0 must not be matched by a zero sender",
			cfg:     household(),
			in:      transport.Inbound{ChatID: 4242, UserID: 0, Text: "hello"},
			wantErr: true,
		},
		{
			name: "enrolled member in a direct chat",
			cfg:  household(),
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private", "household"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
		{
			name: "second enrolled member gets their own space and their own chain",
			cfg:  household(),
			in:   direct(mariaID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "maria",
				write:    "maria-private",
				read:     []domain.SpaceID{"maria-private", "household"},
				tiers:    []string{"local", "cloud"},
				chatID:   mariaID,
			},
		},
		{
			name: "adjacent telegram ids resolve to their own members: lower",
			cfg:  household(),
			in:   direct(adjacentA),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "ann",
				write:    "ann-private",
				read:     []domain.SpaceID{"ann-private", "household"},
				tiers:    []string{"local"},
				chatID:   adjacentA,
			},
		},
		{
			name: "adjacent telegram ids resolve to their own members: upper",
			cfg:  household(),
			in:   direct(adjacentB),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "bea",
				write:    "bea-private",
				read:     []domain.SpaceID{"bea-private", "household"},
				tiers:    []string{"cloud"},
				chatID:   adjacentB,
			},
		},
		{
			name:    "one past the highest adjacent id is a stranger",
			cfg:     household(),
			in:      direct(adjacentB + 1),
			wantErr: true,
		},
		{
			name:    "one below the lowest adjacent id is a stranger",
			cfg:     household(),
			in:      direct(adjacentA - 1),
			wantErr: true,
		},
		{
			name: "the configured group, message from a member",
			cfg:  household(),
			in:   inGroup(groupChatID, davidID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			name: "the configured group, message from a second enrolled member",
			cfg:  household(),
			in:   inGroup(groupChatID, mariaID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			// Being in the group chat is not enrolment: any member can add anyone to
			// a Telegram group, and the shared space holds household knowledge.
			name:    "the configured group, message from a member who has not claimed",
			cfg:     household(),
			in:      inGroup(groupChatID, 0),
			wantErr: true,
		},
		{
			name:    "the configured group, message from someone who is not a member",
			cfg:     household(),
			in:      inGroup(groupChatID, 999999),
			wantErr: true,
		},
		{
			name:    "the configured group, sender id one away from a member's",
			cfg:     household(),
			in:      inGroup(groupChatID, adjacentB+1),
			wantErr: true,
		},
		{
			name:    "a group that is not the configured one, message from a member",
			cfg:     household(),
			in:      inGroup(-100999, davidID),
			wantErr: true,
		},
		{
			name:    "a group that is not the configured one, message from a stranger",
			cfg:     household(),
			in:      inGroup(-100999, 999999),
			wantErr: true,
		},
		{
			name:    "a group whose id is one away from the configured one",
			cfg:     household(),
			in:      inGroup(groupChatID+1, davidID),
			wantErr: true,
		},
		{
			name:    "no group configured: a group chat with id zero must not match",
			cfg:     noGroup,
			in:      inGroup(0, davidID),
			wantErr: true,
		},
		{
			name: "no group configured: direct messages still resolve",
			cfg:  noGroup,
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private", "household"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
		{
			// The chat and the flag disagree, which the package doc has always said
			// is a rejection. The only way to produce one is a group_chat_id that is
			// really somebody's private chat, and answering it either way is a guess
			// about whether the household is listening.
			name:    "the configured group's chat id on a message that is not a group message",
			cfg:     household(),
			in:      transport.Inbound{ChatID: groupChatID, UserID: davidID, Text: "hello"},
			wantErr: true,
		},
		{
			// The same fault seen from the direction that costs something: a
			// group_chat_id misconfigured to a member's own Telegram id. Their
			// direct messages must not be answered into the shared space, where the
			// whole household reads them.
			name: "group chat id set to a member's own telegram id",
			cfg: func() *config.Config {
				c := household()
				c.Household.GroupChatID = davidID
				return c
			}(),
			in:      direct(davidID),
			wantErr: true,
		},
		{
			name: "member's chat id differing from their user id still resolves by sender",
			cfg:  household(),
			in:   transport.Inbound{ChatID: 77, UserID: davidID, Text: "hello"},
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private", "household"},
				tiers:    []string{"local"},
				chatID:   77,
			},
		},
		{
			name: "private space colliding with the shared space is not read twice",
			cfg:  collided,
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "household",
				read:     []domain.SpaceID{"household"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
		{
			name: "private space colliding with the shared space still yields a shared-only group scope",
			cfg:  collided,
			in:   inGroup(groupChatID, davidID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			name: "one agent each: a member's private message on the household bot is kenward's conversation",
			cfg:  perMember,
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeHousehold,
				memberID: "david",
				write:    "household",
				read:     []domain.SpaceID{"household"},
				// The household's chain, not David's: everything in this
				// conversation is the household's material.
				tiers:  []string{"local", "cloud"},
				chatID: davidID,
			},
		},
		{
			name: "one agent each: the same message on the member's own bot is their own conversation",
			cfg:  perMember,
			bot:  "david",
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private", "household"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
		{
			name:    "one agent each: a member reaching another member's bot is a stranger to it",
			cfg:     perMember,
			bot:     "david",
			in:      direct(mariaID),
			wantErr: true,
		},
		{
			name:    "one agent each: a stranger reaching a member's bot",
			cfg:     perMember,
			bot:     "david",
			in:      direct(999999),
			wantErr: true,
		},
		{
			name:    "one agent each: an unenrolled member reaching the household bot",
			cfg:     perMember,
			in:      direct(0),
			wantErr: true,
		},
		{
			name:    "one agent each: a member's own bot in the household group is not kenward",
			cfg:     perMember,
			bot:     "david",
			in:      inGroup(groupChatID, davidID),
			wantErr: true,
		},
		{
			name: "one agent each: the household group is unchanged and still carries no member",
			cfg:  perMember,
			in:   inGroup(groupChatID, davidID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			name: "one agent each in simple mode cannot be delivered, so a direct chat stays the member's own",
			cfg:  perMemberSimple,
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private", "household"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
		{
			name: "empty shared space leaves a direct scope reading only the private one",
			cfg:  noShared,
			in:   direct(davidID),
			want: want{
				kind:     domain.ScopeDirect,
				memberID: "david",
				write:    "david-private",
				read:     []domain.SpaceID{"david-private"},
				tiers:    []string{"local"},
				chatID:   davidID,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scope.Resolve(tt.cfg, tt.bot, tt.in)
			if tt.wantErr {
				if !errors.Is(err, scope.ErrNotEnrolled) {
					t.Fatalf("Resolve() error = %v, want ErrNotEnrolled", err)
				}
				if !reflect.DeepEqual(got, domain.Scope{}) {
					t.Fatalf("Resolve() returned %+v alongside an error; a rejection must return the zero Scope", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			checkScope(t, got, tt.want)
		})
	}
}

func checkScope(t *testing.T, got domain.Scope, w want) {
	t.Helper()
	if got.Kind != w.kind {
		t.Errorf("Kind = %v, want %v", got.Kind, w.kind)
	}
	if got.Write != w.write {
		t.Errorf("Write = %q, want %q", got.Write, w.write)
	}
	if !reflect.DeepEqual(got.Read, w.read) {
		t.Errorf("Read = %v, want %v", got.Read, w.read)
	}
	if !reflect.DeepEqual(got.Tiers, w.tiers) {
		t.Errorf("Tiers = %v, want %v", got.Tiers, w.tiers)
	}
	if got.ChatID != w.chatID {
		t.Errorf("ChatID = %d, want %d", got.ChatID, w.chatID)
	}
	switch {
	case w.memberID == "":
		if got.Member != nil {
			t.Errorf("Member = %+v, want nil", *got.Member)
		}
	case got.Member == nil:
		t.Errorf("Member = nil, want member %q", w.memberID)
	case got.Member.ID != w.memberID:
		t.Errorf("Member.ID = %q, want %q", got.Member.ID, w.memberID)
	}
	// The invariant table, restated rather than special-cased. Member is nil exactly
	// in the group, which is the one scope with no single asker; every other scope
	// knows who is speaking, including the one that may not touch anything of theirs.
	if (got.Kind == domain.ScopeGroup) != (got.Member == nil) {
		t.Errorf("Member is nil iff Kind is ScopeGroup is violated: kind=%v member=%v", got.Kind, got.Member)
	}
	// And the other half: knowing who is asking is not access. Exactly one kind
	// touches a private space, and the two predicates that gate on it agree.
	if got.TouchesPrivateMemory() != (got.Kind == domain.ScopeDirect) {
		t.Errorf("TouchesPrivateMemory() = %v for kind %v; only a direct scope touches a private space", got.TouchesPrivateMemory(), got.Kind)
	}
	if got.AllowsPrivateCapture() != got.TouchesPrivateMemory() {
		t.Errorf("AllowsPrivateCapture() = %v but TouchesPrivateMemory() = %v; a scope may offer a private destination exactly when it has one",
			got.AllowsPrivateCapture(), got.TouchesPrivateMemory())
	}
}

// TestGroupScopeNeverExposesAPrivateSpace is the narrow, direct statement of the rule
// the whole memory model rests on, tested against the member most likely to break it:
// one who is speaking in the group.
func TestGroupScopeNeverExposesAPrivateSpace(t *testing.T) {
	cfg := household()

	for _, m := range cfg.Members {
		t.Run(m.ID, func(t *testing.T) {
			got, err := scope.Resolve(cfg, "", inGroup(groupChatID, m.TelegramID))
			if m.TelegramID == 0 {
				// An unclaimed member is not admitted to the group either.
				if !errors.Is(err, scope.ErrNotEnrolled) {
					t.Fatalf("Resolve() error = %v, want ErrNotEnrolled", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if got.Kind != domain.ScopeGroup {
				t.Fatalf("Kind = %v, want ScopeGroup: a member speaking in the household chat is speaking to the household", got.Kind)
			}
			if got.Member != nil {
				t.Errorf("Member = %+v, want nil", *got.Member)
			}
			if got.AllowsPrivateCapture() {
				t.Error("AllowsPrivateCapture() = true in a group scope")
			}
			assertNoPrivateSpace(t, got, privateSpaces(cfg))
		})
	}
}

func assertNoPrivateSpace(t *testing.T, s domain.Scope, private []domain.SpaceID) {
	t.Helper()
	for _, p := range private {
		if p == "" {
			continue
		}
		if s.Write == p {
			t.Errorf("%s scope writes to private space %q", s.Kind, p)
		}
		for i, r := range s.Read {
			if r == p {
				t.Errorf("%s scope reads private space %q at Read[%d]", s.Kind, p, i)
			}
		}
	}
}

// TestGroupScopeNeverExposesAPrivateSpaceOverGeneratedConfigs is the property the
// implementation contract requires to be asserted over generated configurations rather
// than over one hand-written household: whatever the shape of the household and
// whoever is speaking, a scope that is the household's reads and writes the shared
// space and nothing else.
//
// It is run over every bot the household could have, because which bot a message
// arrived on is now part of the decision, and a household scope is only ever reached
// on the household's own. Sweeping the bots is also what makes the second property
// here checkable at all: a member's own bot serves that member and refuses everybody
// else, so a household of six is six chances for a lookup to answer the wrong person.
func TestGroupScopeNeverExposesAPrivateSpaceOverGeneratedConfigs(t *testing.T) {
	rng := rand.New(rand.NewSource(20260815))

	const configs = 200
	for i := 0; i < configs; i++ {
		cfg := generateHousehold(rng)
		private := privateSpaces(cfg)
		enrolled := enrolledIDs(cfg)
		shared := domain.SpaceID(cfg.Household.SharedSpace)

		for _, in := range generateInbounds(rng, cfg) {
			for _, bot := range generateBots(cfg) {
				got, err := scope.Resolve(cfg, bot, in)
				if err != nil {
					if !errors.Is(err, scope.ErrNotEnrolled) {
						t.Fatalf("config %d, bot %q, inbound %+v: error = %v, want ErrNotEnrolled", i, bot, in, err)
					}
					continue
				}
				where := fmt.Sprintf("config %d, bot %q, inbound %+v", i, bot, in)

				// Anything at all that resolved: the sender is an enrolled member.
				// Being in the household's Telegram group is not enrolment, and
				// neither is knowing a bot's name.
				if !enrolled[in.UserID] {
					t.Fatalf("%s: %s scope served to a sender who is not an enrolled member", where, got.Kind)
				}

				if got.TouchesPrivateMemory() {
					// Direct scopes may name exactly one private space: the sender's own.
					if got.Member == nil {
						t.Fatalf("%s: direct scope with nil Member", where)
					}
					// And they are only ever reached on that member's own bot, or on
					// the household's while it is also serving as everybody's.
					if bot != "" && bot != got.Member.ID {
						t.Fatalf("%s: %q's own conversation was resolved on %q's bot", where, got.Member.ID, bot)
					}
					for _, p := range private {
						if p == got.Member.Private {
							continue
						}
						if got.Write == p {
							t.Fatalf("%s: %q writes to %q, another member's private space", where, got.Member.ID, p)
						}
						for _, r := range got.Read {
							if r == p {
								t.Fatalf("%s: %q reads %q, another member's private space", where, got.Member.ID, p)
							}
						}
					}
					continue
				}

				// Everything else is the household's conversation, and every one of
				// them reads and writes the shared space alone. Asserted before the
				// per-kind rules on purpose: this is the property that must hold for
				// a kind nobody has thought of yet.
				if got.AllowsPrivateCapture() {
					t.Fatalf("%s: %s scope allows private capture", where, got.Kind)
				}
				if got.Write != shared {
					t.Fatalf("%s: %s Write = %q, want the shared space %q", where, got.Kind, got.Write, shared)
				}
				if len(got.Read) != 1 || got.Read[0] != shared {
					t.Fatalf("%s: %s Read = %v, want exactly [%q]", where, got.Kind, got.Read, shared)
				}
				assertNoPrivateSpace(t, got, private)

				// The household's conversations happen on the household's bot. A
				// member's own bot has no part in either of them.
				if bot != "" {
					t.Fatalf("%s: a %s scope was resolved on a member's own bot", where, got.Kind)
				}

				switch got.Kind {
				case domain.ScopeGroup:
					if got.Member != nil {
						t.Fatalf("%s: group scope carries member %+v", where, *got.Member)
					}
					// A group scope is only ever reached from a group message. The
					// generated households above sometimes give group_chat_id a
					// member's own chat id, which is the only way a direct message
					// can carry it — and answering that into the shared space
					// publishes their private conversation to the household.
					if !in.IsGroup {
						t.Fatalf("%s: a message that is not a group message resolved to a group scope", where)
					}
				case domain.ScopeHousehold:
					// It exists only where the household chose one agent each. Under
					// one agent this chat is the member's own and must resolve as it
					// always has.
					if !cfg.AgentPerMember() {
						t.Fatalf("%s: a household scope was resolved for a household that has one agent", where)
					}
					if in.IsGroup {
						t.Fatalf("%s: a group message resolved to a private conversation with kenward", where)
					}
					// It carries the member — that is the whole difference from a
					// group scope, and the assertions above have already established
					// that carrying one buys no access to anything of theirs.
					if got.Member == nil {
						t.Fatalf("%s: household scope with nil Member; kenward must know who is asking", where)
					}
					if got.Member.TelegramID != in.UserID {
						t.Fatalf("%s: household scope names member %q, who is not the sender", where, got.Member.ID)
					}
				default:
					t.Fatalf("%s: unexpected scope kind %v", where, got.Kind)
				}
			}
		}
	}
}

// generateBots lists every bot a household could have a message arrive on: the
// household's own, each member's, and an id belonging to nobody.
func generateBots(cfg *config.Config) []domain.MemberID {
	out := []domain.MemberID{"", "nobody"}
	for _, m := range cfg.Members {
		out = append(out, domain.MemberID(m.ID))
	}
	return out
}

// generateHousehold builds a random but coherent household: a random number of members,
// some claimed and some not, random Telegram ids including deliberately adjacent ones,
// and a shared space that sometimes collides with a member's private space so the
// property is tested against misconfiguration too.
func generateHousehold(rng *rand.Rand) *config.Config {
	n := 1 + rng.Intn(6)
	cfg := &config.Config{
		Mode: config.ModeSimple,
		Household: config.HouseholdConfig{
			Name:        "Generated",
			SharedSpace: fmt.Sprintf("shared-%d", rng.Intn(1000)),
			GroupChatID: -1000000000000 - int64(rng.Intn(100000)),
			Tiers:       []string{"local", "cloud"},
		},
	}
	base := int64(1 + rng.Intn(100000))
	for i := 0; i < n; i++ {
		var tg int64
		if rng.Intn(4) != 0 {
			// Adjacent ids are the interesting case for a lookup bug.
			tg = base + int64(i)
		}
		cfg.Members = append(cfg.Members, config.MemberConfig{
			ID:           fmt.Sprintf("m%d", i),
			Name:         fmt.Sprintf("Member %d", i),
			TelegramID:   tg,
			PrivateSpace: fmt.Sprintf("private-%d-%d", rng.Intn(1000), i),
			Tiers:        []string{"local"},
		})
	}
	if rng.Intn(5) == 0 {
		// A configuration validation would reject. Resolve must still hold the line.
		cfg.Household.SharedSpace = cfg.Members[rng.Intn(n)].PrivateSpace
	}
	if rng.Intn(5) == 0 {
		// A group_chat_id that is really a member's own chat — a misconfiguration
		// validation rejects, and one the generator could not previously produce
		// because every group id it made was negative and every member id positive.
		// It is the collision that costs something: the group is matched on chat id
		// before the sender is looked at, so this is a member's direct messages
		// arriving at the household's scope.
		for _, m := range cfg.Members {
			if m.TelegramID != 0 {
				cfg.Household.GroupChatID = m.TelegramID
				break
			}
		}
	}
	if rng.Intn(7) == 0 {
		cfg.Household.GroupChatID = 0
	}
	// Roughly half the households give every member an agent of their own, which is
	// the only arrangement in which a private conversation with kenward exists. It
	// takes isolated mode with it: one agent each needs one bot each, and simple
	// mode has one for everybody — see config.AgentPerMember, which refuses the
	// combination rather than serving a member's private chat as the household's.
	if rng.Intn(2) == 0 {
		cfg.Mode = config.ModeIsolated
		cfg.Household.Agents = config.AgentsPerMember
	}
	return cfg
}

// generateInbounds produces the messages worth throwing at a generated household: every
// member direct and in the group, strangers in both, and neighbouring chat and user ids.
func generateInbounds(rng *rand.Rand, cfg *config.Config) []transport.Inbound {
	var out []transport.Inbound
	g := cfg.Household.GroupChatID
	for _, m := range cfg.Members {
		out = append(out,
			direct(m.TelegramID),
			inGroup(g, m.TelegramID),
			inGroup(g+1, m.TelegramID),
			inGroup(g-1, m.TelegramID),
			transport.Inbound{ChatID: g, UserID: m.TelegramID}, // group id without the flag
			direct(m.TelegramID+1),
			direct(m.TelegramID-1),
		)
	}
	stranger := int64(900000000 + rng.Intn(1000))
	out = append(out,
		direct(stranger),
		inGroup(g, stranger),
		inGroup(int64(rng.Intn(1000)), stranger),
		transport.Inbound{},
	)
	return out
}

// TestResolveDoesNotAliasConfiguration guards the copy in Resolve: a caller that edits
// the tier chain it was handed must not be editing the household's privacy policy.
func TestResolveDoesNotAliasConfiguration(t *testing.T) {
	cfg := household()

	got, err := scope.Resolve(cfg, "", direct(mariaID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	got.Tiers[0] = "compromised"
	got.Read[0] = "compromised"
	if cfg.Members[1].Tiers[0] != "local" {
		t.Errorf("editing Scope.Tiers changed the configuration: %v", cfg.Members[1].Tiers)
	}

	again, err := scope.Resolve(cfg, "", direct(mariaID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if again.Tiers[0] != "local" || again.Read[0] != "maria-private" {
		t.Errorf("second Resolve() saw mutated state: tiers=%v read=%v", again.Tiers, again.Read)
	}

	group, err := scope.Resolve(cfg, "", inGroup(groupChatID, davidID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	group.Tiers[0] = "compromised"
	if cfg.Household.Tiers[0] != "local" {
		t.Errorf("editing a group Scope.Tiers changed the configuration: %v", cfg.Household.Tiers)
	}
}

// TestDirectScopeReadOrder pins the order the invariant table gives: the member's own
// space first, the household's second. Retrieval keeps results grouped in this order,
// so reversing it would quietly reorder what the assistant sees first.
func TestDirectScopeReadOrder(t *testing.T) {
	cfg := household()
	got, err := scope.Resolve(cfg, "", direct(davidID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []domain.SpaceID{"david-private", "household"}
	if !reflect.DeepEqual(got.Read, want) {
		t.Fatalf("Read = %v, want %v", got.Read, want)
	}
	if !got.AllowsPrivateCapture() {
		t.Error("AllowsPrivateCapture() = false in a direct scope")
	}
}

// TestUnenrolledMemberIsIndistinguishableFromAStranger states the enrolment rule from
// the other side: a member row exists, but until they claim it they are served exactly
// as a stranger is — with silence.
func TestUnenrolledMemberIsIndistinguishableFromAStranger(t *testing.T) {
	cfg := household()

	unclaimed, err := scope.Resolve(cfg, "", direct(0))
	strangerScope, strangerErr := scope.Resolve(cfg, "", direct(424242))

	if !errors.Is(err, scope.ErrNotEnrolled) || !errors.Is(strangerErr, scope.ErrNotEnrolled) {
		t.Fatalf("errors = (%v, %v), want both ErrNotEnrolled", err, strangerErr)
	}
	if !reflect.DeepEqual(unclaimed, strangerScope) {
		t.Fatalf("unclaimed member and stranger produced different results: %+v vs %+v", unclaimed, strangerScope)
	}
}
