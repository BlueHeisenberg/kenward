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

	tests := []struct {
		name    string
		cfg     *config.Config
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
			got, err := scope.Resolve(tt.cfg, tt.in)
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
	// The invariant table: a group scope is nil-membered, and a direct scope is not.
	if (got.Kind == domain.ScopeGroup) != (got.Member == nil) {
		t.Errorf("Member is nil iff Kind is ScopeGroup is violated: kind=%v member=%v", got.Kind, got.Member)
	}
}

// TestGroupScopeNeverExposesAPrivateSpace is the narrow, direct statement of the rule
// the whole memory model rests on, tested against the member most likely to break it:
// one who is speaking in the group.
func TestGroupScopeNeverExposesAPrivateSpace(t *testing.T) {
	cfg := household()

	for _, m := range cfg.Members {
		t.Run(m.ID, func(t *testing.T) {
			got, err := scope.Resolve(cfg, inGroup(groupChatID, m.TelegramID))
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
			t.Errorf("group scope writes to private space %q", p)
		}
		for i, r := range s.Read {
			if r == p {
				t.Errorf("group scope reads private space %q at Read[%d]", p, i)
			}
		}
	}
}

// TestGroupScopeNeverExposesAPrivateSpaceOverGeneratedConfigs is the property the
// implementation contract requires to be asserted over generated configurations rather
// than over one hand-written household: whatever the shape of the household and
// whoever is speaking, a group scope reads and writes the shared space and nothing
// else.
func TestGroupScopeNeverExposesAPrivateSpaceOverGeneratedConfigs(t *testing.T) {
	rng := rand.New(rand.NewSource(20260815))

	const configs = 200
	for i := 0; i < configs; i++ {
		cfg := generateHousehold(rng)
		private := privateSpaces(cfg)
		enrolled := enrolledIDs(cfg)

		for _, in := range generateInbounds(rng, cfg) {
			got, err := scope.Resolve(cfg, in)
			if err != nil {
				if !errors.Is(err, scope.ErrNotEnrolled) {
					t.Fatalf("config %d, inbound %+v: error = %v, want ErrNotEnrolled", i, in, err)
				}
				continue
			}
			if got.Kind != domain.ScopeGroup {
				// Direct scopes may name exactly one private space: the sender's own.
				if got.Member == nil {
					t.Fatalf("config %d, inbound %+v: direct scope with nil Member", i, in)
				}
				for _, p := range private {
					if p == got.Member.Private {
						continue
					}
					if got.Write == p {
						t.Fatalf("config %d, inbound %+v: %q writes to %q, another member's private space", i, in, got.Member.ID, p)
					}
					for _, r := range got.Read {
						if r == p {
							t.Fatalf("config %d, inbound %+v: %q reads %q, another member's private space", i, in, got.Member.ID, p)
						}
					}
				}
				continue
			}
			if got.Member != nil {
				t.Fatalf("config %d, inbound %+v: group scope carries member %+v", i, in, *got.Member)
			}
			// The admission gate: reaching a group scope at all requires the sender to
			// be an enrolled member. Being in the household's Telegram group is not
			// enrolment, and the shared space is not open to whoever was added to it.
			if !enrolled[in.UserID] {
				t.Fatalf("config %d, inbound %+v: group scope served to a sender who is not an enrolled member", i, in)
			}
			if got.AllowsPrivateCapture() {
				t.Fatalf("config %d, inbound %+v: group scope allows private capture", i, in)
			}
			if got.Write != domain.SpaceID(cfg.Household.SharedSpace) {
				t.Fatalf("config %d, inbound %+v: group Write = %q, want the shared space %q", i, in, got.Write, cfg.Household.SharedSpace)
			}
			if len(got.Read) != 1 || got.Read[0] != domain.SpaceID(cfg.Household.SharedSpace) {
				t.Fatalf("config %d, inbound %+v: group Read = %v, want exactly [%q]", i, in, got.Read, cfg.Household.SharedSpace)
			}
			assertNoPrivateSpace(t, got, private)
		}
	}
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
	if rng.Intn(7) == 0 {
		cfg.Household.GroupChatID = 0
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

	got, err := scope.Resolve(cfg, direct(mariaID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	got.Tiers[0] = "compromised"
	got.Read[0] = "compromised"
	if cfg.Members[1].Tiers[0] != "local" {
		t.Errorf("editing Scope.Tiers changed the configuration: %v", cfg.Members[1].Tiers)
	}

	again, err := scope.Resolve(cfg, direct(mariaID))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if again.Tiers[0] != "local" || again.Read[0] != "maria-private" {
		t.Errorf("second Resolve() saw mutated state: tiers=%v read=%v", again.Tiers, again.Read)
	}

	group, err := scope.Resolve(cfg, inGroup(groupChatID, davidID))
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
	got, err := scope.Resolve(cfg, direct(davidID))
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

	unclaimed, err := scope.Resolve(cfg, direct(0))
	strangerScope, strangerErr := scope.Resolve(cfg, direct(424242))

	if !errors.Is(err, scope.ErrNotEnrolled) || !errors.Is(strangerErr, scope.ErrNotEnrolled) {
		t.Fatalf("errors = (%v, %v), want both ErrNotEnrolled", err, strangerErr)
	}
	if !reflect.DeepEqual(unclaimed, strangerScope) {
		t.Fatalf("unclaimed member and stranger produced different results: %+v vs %+v", unclaimed, strangerScope)
	}
}
