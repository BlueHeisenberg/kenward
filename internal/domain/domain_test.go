package domain

import (
	"testing"
	"time"
)

func TestMemberEnrolled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m    Member
		want bool
	}{
		{"zero value is not enrolled", Member{}, false},
		{"named but unclaimed is not enrolled", Member{ID: "david", Name: "David"}, false},
		{"a bound telegram id is enrolment", Member{ID: "david", TelegramID: 12345}, true},
		{
			"a timestamp alone is not enrolment",
			Member{ID: "david", EnrolledAt: time.Unix(1, 0)},
			false,
		},
		{"a negative id still counts as bound", Member{ID: "david", TelegramID: -1}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.m.Enrolled(); got != tc.want {
				t.Errorf("Enrolled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestScopeAllowsPrivateCapture pins the invariant that a group conversation can never
// offer to write into a private space. If this test is ever changed to make a group
// scope return true, the household chat becomes a write path into someone's private
// memory, which is the single thing the memory model exists to prevent.
func TestScopeAllowsPrivateCapture(t *testing.T) {
	t.Parallel()

	member := Member{ID: "david", Name: "David", TelegramID: 1, Private: "david-private"}

	cases := []struct {
		name string
		s    Scope
		want bool
	}{
		{
			"direct scope may offer a private destination",
			Scope{Kind: ScopeDirect, Member: &member, Write: "david-private"},
			true,
		},
		{
			"group scope may not",
			Scope{Kind: ScopeGroup, Write: "household", Read: []SpaceID{"household"}},
			false,
		},
		{
			"a group scope carrying a member is still a group scope",
			Scope{Kind: ScopeGroup, Member: &member, Write: "household"},
			false,
		},
		{
			// The case the third scope adds, and the one this predicate exists to
			// get right: a private conversation, a member known by name, and no
			// private space anywhere in it. Offering a personal destination here
			// would resolve to the household's shared space or to nothing, and
			// either is a member told their note went somewhere it did not.
			"household scope carries a member and still may not",
			Scope{Kind: ScopeHousehold, Member: &member, Write: "household", Read: []SpaceID{"household"}},
			false,
		},
		{
			"an unresolved scope offers nothing",
			Scope{},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.AllowsPrivateCapture(); got != tc.want {
				t.Errorf("AllowsPrivateCapture() = %v, want %v", got, tc.want)
			}
			// The two are one statement said twice: a conversation may offer a
			// private destination exactly when it already touches one. They must
			// never be able to disagree.
			if got := tc.s.TouchesPrivateMemory(); got != tc.want {
				t.Errorf("TouchesPrivateMemory() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestScopeKindString covers the zero value explicitly: an unresolved ScopeKind must
// render as something obviously wrong rather than as a plausible scope, because this
// string ends up in operator logs.
func TestScopeKindString(t *testing.T) {
	t.Parallel()

	cases := map[ScopeKind]string{
		ScopeUnknown:   "unknown",
		ScopeDirect:    "direct",
		ScopeGroup:     "group",
		ScopeHousehold: "household",
		ScopeKind(99):  "unknown",
		ScopeKind(-1):  "unknown",
	}

	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("ScopeKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

// TestScopeZeroValueIsNotUsable guards against a resolver that forgets to set Kind. A
// Scope with the zero Kind must not look like a valid direct scope to anything
// downstream.
func TestScopeZeroValueIsNotUsable(t *testing.T) {
	t.Parallel()

	var s Scope

	if s.Kind != ScopeUnknown {
		t.Fatalf("zero Scope.Kind = %v, want ScopeUnknown", s.Kind)
	}
	if s.AllowsPrivateCapture() {
		t.Error("zero Scope allows private capture; it must not")
	}
	if s.TouchesPrivateMemory() {
		t.Error("zero Scope claims to touch private memory; it must not")
	}
	if len(s.Read) != 0 {
		t.Errorf("zero Scope.Read = %v, want empty", s.Read)
	}
	if s.Write != "" {
		t.Errorf("zero Scope.Write = %q, want empty", s.Write)
	}
}
