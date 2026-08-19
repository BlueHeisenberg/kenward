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

	// leo has no memory of their own: no private space, no assistant, no pod. Every
	// conversation they have is the household's.
	leoID = int64(1003)
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
			{ID: "leo", Name: "Leo", TelegramID: leoID, SharedOnly: true},
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

	// A member who is supposed to have a private space and has not got one. Refused
	// by config validation; Resolve is the last line of defence and is documented as
	// being called with configurations it did not validate.
	missingSpace := household()
	missingSpace.Members[0].PrivateSpace = ""

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
			// The whole feature, in simple mode: a member with no memory of their
			// own, in a private chat, on the household's bot. Today this household
			// has one agent, so every other member here resolves to ScopeDirect on
			// this exact input; leo cannot, because there is no space for a direct
			// scope to name.
			name: "shared_only member in a direct chat, simple mode: the household scope",
			cfg:  household(),
			in:   direct(leoID),
			want: want{
				kind:     domain.ScopeHousehold,
				memberID: "leo",
				write:    "household",
				read:     []domain.SpaceID{"household"},
				tiers:    []string{"local", "cloud"},
				chatID:   leoID,
			},
		},
		{
			// And the same answer under one agent each, where it is the answer
			// every member gets. The point is that it did not change: this scope
			// is a fact about the member, not about the household's arrangement.
			name: "shared_only member in a direct chat, one agent each: the same scope",
			cfg:  perMember,
			in:   direct(leoID),
			want: want{
				kind:     domain.ScopeHousehold,
				memberID: "leo",
				write:    "household",
				read:     []domain.SpaceID{"household"},
				tiers:    []string{"local", "cloud"},
				chatID:   leoID,
			},
		},
		{
			name: "shared_only member in the household group is admitted",
			cfg:  household(),
			in:   inGroup(groupChatID, leoID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			name: "shared_only member in the household group, one agent each",
			cfg:  perMember,
			in:   inGroup(groupChatID, leoID),
			want: want{
				kind:   domain.ScopeGroup,
				write:  "household",
				read:   []domain.SpaceID{"household"},
				tiers:  []string{"local", "cloud"},
				chatID: groupChatID,
			},
		},
		{
			// They have no bot of their own, so a message from them on somebody
			// else's is a message on a bot that is not theirs, and gets what
			// anybody else gets on a bot that is not theirs.
			name:    "shared_only member on another member's bot",
			cfg:     perMember,
			bot:     "david",
			in:      direct(leoID),
			wantErr: true,
		},
		{
			// A pod that somehow came up claiming to be theirs. There is no such
			// pod — the supervisor skips them and NewSingle refuses the selection —
			// and if there were, it would hold no token and no key. Refused at the
			// boundary as well, so that the answer does not depend on the wiring
			// having got it right.
			name:    "shared_only member on a bot bearing their own id",
			cfg:     perMember,
			bot:     "leo",
			in:      direct(leoID),
			wantErr: true,
		},
		{
			// The failure the explicit flag exists to prevent, at the boundary.
			// This member is not shared_only: the household believes they have a
			// private space and the line is missing. Neither reading is safe —
			// ScopeDirect would write their private notes to the empty space id,
			// and the household scope would publish them to everybody — so this
			// is the third answer.
			name:    "a member with no private space who is not shared_only is served nothing",
			cfg:     missingSpace,
			in:      direct(davidID),
			wantErr: true,
		},
		{
			// And the same member in the group, where the fault costs nothing: a
			// group scope names no private space for anyone, so there is nothing
			// for a missing one to break. Refusing here would take a member out of
			// the household chat over a line that has no bearing on it.
			name: "the same member is still admitted to the group",
			cfg:  missingSpace,
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

	// A property nothing generated is a property nothing proved. These count the
	// three arrangements this test exists to cover, and the assertions after the
	// loop fail if the generator stopped producing one of them — which is how a
	// property test quietly becomes decoration.
	var (
		sawSharedOnlyHousehold int // shared_only member, one shared agent
		sawSharedOnlyPerMember int // shared_only member, one agent each
		sawBrokenRefused       int // missing private space, served nothing
	)

	const configs = 200
	for i := 0; i < configs; i++ {
		cfg := generateHousehold(rng)
		private := privateSpaces(cfg)
		enrolled := enrolledIDs(cfg)
		sharedOnly := sharedOnlyIDs(cfg)
		broken := brokenIDs(cfg)
		shared := domain.SpaceID(cfg.Household.SharedSpace)

		sawBrokenRefused += countBrokenRefusals(t, cfg, broken)

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

				// A member whose private space went missing is served no private
				// conversation. In the group they are a member like any other —
				// nothing there names a private space for anybody — so the rule is
				// about the chats that would have had to name one.
				if broken[in.UserID] && !in.IsGroup && got.Kind != domain.ScopeGroup {
					t.Fatalf("%s: a member with a missing private space was served a %s scope; the safe answer is none", where, got.Kind)
				}

				// Anything at all that resolved: the sender is an enrolled member.
				// Being in the household's Telegram group is not enrolment, and
				// neither is knowing a bot's name.
				if !enrolled[in.UserID] {
					t.Fatalf("%s: %s scope served to a sender who is not an enrolled member", where, got.Kind)
				}

				// The narrow statement of the new member's whole guarantee, asserted
				// before anything else is looked at and against the sender rather
				// than against the scope's own kind: whatever Resolve decided this
				// is, if the person who sent it has no memory of their own then it
				// must not be a scope that touches one. Asked this way round because
				// a bug that produced a direct scope for them would answer the
				// kind-based questions below quite happily.
				if sharedOnly[in.UserID] && got.TouchesPrivateMemory() {
					t.Fatalf("%s: a shared_only member was given a %s scope, which touches a private space", where, got.Kind)
				}

				if got.TouchesPrivateMemory() {
					// Direct scopes may name exactly one private space: the sender's own.
					if got.Member == nil {
						t.Fatalf("%s: direct scope with nil Member", where)
					}
					// A direct scope with no space to write to is the failure the
					// explicit flag exists to make impossible. It would either
					// error at the store or, worse, be defaulted somewhere
					// downstream into a space that belongs to somebody.
					if got.Member.Private == "" || got.Write == "" {
						t.Fatalf("%s: direct scope for %q names no private space (Write = %q)", where, got.Member.ID, got.Write)
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
					if got.Member == nil {
						t.Fatalf("%s: household scope with nil Member; kenward must know who is asking", where)
					}
					// Two disjoint reasons, and no third. Under one agent each,
					// this is every member's chat with kenward. For a member with
					// no assistant of their own it is their only conversation, in
					// either arrangement. Anything else reaching this kind means a
					// member who does have an agent of their own has quietly lost
					// their private chat to the household's memory, which is the
					// downgrade this scope must never be the result of.
					if !cfg.AgentPerMember() && !got.Member.SharedOnly {
						t.Fatalf("%s: a household scope was resolved for %q, who has an agent of their own in a household with one agent", where, got.Member.ID)
					}
					if in.IsGroup {
						t.Fatalf("%s: a group message resolved to a private conversation with kenward", where)
					}
					// It carries the member — that is the whole difference from a
					// group scope, and the assertions above have already established
					// that carrying one buys no access to anything of theirs.
					if got.Member.TelegramID != in.UserID {
						t.Fatalf("%s: household scope names member %q, who is not the sender", where, got.Member.ID)
					}
					if got.Member.SharedOnly {
						if cfg.AgentPerMember() {
							sawSharedOnlyPerMember++
						} else {
							sawSharedOnlyHousehold++
						}
					}
				default:
					t.Fatalf("%s: unexpected scope kind %v", where, got.Kind)
				}
			}
		}
	}

	// The one that is easy to lose. The household scope existed only under one agent
	// each until a member with no assistant of their own needed it, and it is now
	// reached in simple mode too; a generator that stopped producing simple-mode
	// households with such a member would leave the whole of that path unasserted
	// while the test still passed.
	coverage(t, "shared_only member under one shared agent", sawSharedOnlyHousehold)
	coverage(t, "shared_only member under one agent each", sawSharedOnlyPerMember)
	coverage(t, "member with a missing private space refused", sawBrokenRefused)
}

// countBrokenRefusals is how many direct messages from members with a missing private
// space were refused, which is the only way to observe that rule: it produces an error
// rather than a scope, so the loop above sees nothing to assert against.
func countBrokenRefusals(t *testing.T, cfg *config.Config, broken map[int64]bool) int {
	t.Helper()
	n := 0
	for id := range broken {
		if _, err := scope.Resolve(cfg, "", direct(id)); errors.Is(err, scope.ErrNotEnrolled) {
			n++
		}
	}
	return n
}

// sharedOnlyIDs is the set of Telegram accounts belonging to members with no memory of
// their own. Zero is never in it, for the same reason it is never in enrolledIDs.
func sharedOnlyIDs(cfg *config.Config) map[int64]bool {
	out := make(map[int64]bool, len(cfg.Members))
	for _, m := range cfg.Members {
		if m.TelegramID != 0 && m.SharedOnly {
			out[m.TelegramID] = true
		}
	}
	return out
}

// brokenIDs is the set of Telegram accounts belonging to members who are supposed to
// have a private space and have not got one. They are the configuration fault that
// shared_only exists to be distinguishable from, and the property is that they are
// served no private conversation at all rather than either of the two answers that
// would look reasonable.
func brokenIDs(cfg *config.Config) map[int64]bool {
	out := make(map[int64]bool, len(cfg.Members))
	for _, m := range cfg.Members {
		if m.TelegramID != 0 && !m.SharedOnly && m.PrivateSpace == "" {
			out[m.TelegramID] = true
		}
	}
	return out
}

// coverage is the guard that the loop above tested what it claims to. Split out so the
// three counts read as one statement rather than as three anonymous integers at the
// bottom of a two-hundred-iteration loop.
func coverage(t *testing.T, name string, n int) {
	t.Helper()
	if n == 0 {
		t.Errorf("the generated households produced no %s; this property was not exercised", name)
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
		m := config.MemberConfig{
			ID:           fmt.Sprintf("m%d", i),
			Name:         fmt.Sprintf("Member %d", i),
			TelegramID:   tg,
			PrivateSpace: fmt.Sprintf("private-%d-%d", rng.Intn(1000), i),
			Tiers:        []string{"local"},
		}
		switch rng.Intn(8) {
		case 0:
			// A member with no memory of their own, as the household declared
			// them: shared_only, and everything a private conversation needs
			// absent. The generator produces them beside full members in the same
			// household on purpose — the property that matters is not that they
			// are served the shared space, which is easy, but that the member
			// sitting next to them in the file is unaffected.
			m.SharedOnly = true
			m.PrivateSpace = ""
			m.Tiers = nil
		case 1:
			// And the fault the flag exists to be distinguishable from: a member
			// nobody marked shared_only whose private space is missing. Validation
			// rejects it; Resolve must not turn it into either of the two answers
			// that look plausible.
			m.PrivateSpace = ""
		}
		cfg.Members = append(cfg.Members, m)
	}
	if rng.Intn(5) == 0 {
		// A configuration validation would reject. Resolve must still hold the line.
		// Skipped when the member drawn has no private space at all: setting the
		// household's shared space to "" would make every scope in the household
		// nameless, which is a different fault and one the generator has no way to
		// say anything useful about.
		if p := cfg.Members[rng.Intn(n)].PrivateSpace; p != "" {
			cfg.Household.SharedSpace = p
		}
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
