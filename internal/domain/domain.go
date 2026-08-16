// Package domain holds kenward's core types.
//
// It depends on nothing else in the module, and nothing in it knows about Telegram,
// lore, MCP, HTTP or any provider. Everything downstream is expressed in these terms.
package domain

import "time"

// MemberID is a stable internal identifier for a household member. It is deliberately
// not the Telegram user id: the Telegram binding can change (a member re-claims from a
// new account) without the member's identity or their memory space changing.
type MemberID string

// SpaceID identifies a lore space. kenward always passes a SpaceID explicitly on every
// memory call and never relies on implicit routing, so this type appears in every
// retrieval and capture path.
type SpaceID string

// Member is one human in the household.
type Member struct {
	// ID is stable for the life of the household.
	ID MemberID
	// Name is what the assistant calls them.
	Name string
	// TelegramID is zero until the member has claimed an invite.
	TelegramID int64
	// Private is the member's two-member lore space: them and the node.
	Private SpaceID
	// Tiers is the ordered chain of endpoint tiers this member's private
	// conversations may use. A chain that names only local tiers is the mechanism
	// behind the privacy claim: when none is reachable, kenward refuses rather than
	// falling through to a provider.
	Tiers []string
	// BotTokenEnv names the environment variable holding this member's own bot
	// token. Used in isolated mode only; empty in simple mode, where the household
	// shares one bot.
	BotTokenEnv string
	// EnrolledAt is zero until the member has claimed an invite.
	EnrolledAt time.Time
}

// Enrolled reports whether the member has completed the claim flow and can be served.
func (m Member) Enrolled() bool { return m.TelegramID != 0 }

// Household is the group itself: the shared space and the group chat that writes to it.
type Household struct {
	Name string
	// Shared is the lore space every member belongs to.
	Shared SpaceID
	// GroupChatID is the Telegram group mapped to the shared space. A group the bot
	// is added to but which is not this id is not served.
	GroupChatID int64
	// Tiers is the ordered endpoint tier chain for group conversations.
	Tiers []string
}

// ScopeKind distinguishes a private conversation from the household group.
type ScopeKind int

const (
	// ScopeUnknown is the zero value and is never valid for a resolved Scope.
	ScopeUnknown ScopeKind = iota
	// ScopeDirect is a one-to-one conversation between an enrolled member and
	// their own assistant. It is the only kind that touches a private space.
	ScopeDirect
	// ScopeGroup is the household group conversation.
	ScopeGroup
	// ScopeHousehold is a private conversation with the household's own agent —
	// kenward — rather than with the member's own.
	//
	// It exists only where a household gave every member an agent of their own, so
	// that there is something for kenward to be separate from. It reads and writes
	// the household's shared memory and nothing else, exactly as a group scope
	// does, and it exists for the two things the group chat makes impossible: adding
	// to the household's memory without notifying everybody, and asking what the
	// household knows without asking in front of everybody.
	ScopeHousehold
)

func (k ScopeKind) String() string {
	switch k {
	case ScopeDirect:
		return "direct"
	case ScopeGroup:
		return "group"
	case ScopeHousehold:
		return "household"
	default:
		return "unknown"
	}
}

// Scope is the resolved answer to "who is this, and what may this conversation touch".
//
// Producing a Scope is the authorization decision. Everything downstream obeys it and
// re-derives nothing: retrieval reads exactly Read, capture writes exactly Write, and
// routing walks exactly Tiers. If a code path needs to consult the configuration again
// to decide what it may access, that path is wrong.
type Scope struct {
	Kind ScopeKind
	// Member is who is asking, and is nil if and only if Kind is ScopeGroup — the
	// one kind with no single asker, because the household chat is the household
	// speaking.
	//
	// Carrying a member is not a licence to read one. ScopeHousehold carries a
	// member and reads the shared space alone: kenward has to know who is asking in
	// order to authorise them and to address them by name, and knowing that is a
	// different thing from being allowed anywhere near their private memory. What a
	// conversation may touch is Read, Write and Tiers below, and nothing downstream
	// derives a space from Member.
	Member *Member
	// Write is the single space captures land in.
	Write SpaceID
	// Read is the ordered set of spaces retrieval may search, primary first. For a
	// group or household scope it contains the shared space and nothing else.
	Read []SpaceID
	// Tiers is the ordered endpoint tier chain this conversation may use.
	Tiers []string
	// ChatID is the Telegram chat this scope was resolved from.
	ChatID int64
}

// TouchesPrivateMemory reports whether this conversation reads or writes a member's
// private space.
//
// It is the positive statement of the boundary, and it is deliberately a property of
// the kind rather than a comparison of spaces: ScopeDirect is the only kind whose
// Write is a member's own space and whose Read begins with it. Every other kind —
// the household group, and a member's private chat with kenward — reads and writes
// the household's shared space, which belongs to everybody and is nobody's private
// memory.
//
// Stated this way round on purpose. "Not a group" was true of the boundary while
// there were two kinds and stopped being true the moment a third arrived that carries
// a member and must still never reach one; anything asking "is this the group?" to
// decide what it may touch is asking the wrong question and would have silently
// admitted ScopeHousehold to a private space.
func (s Scope) TouchesPrivateMemory() bool { return s.Kind == ScopeDirect }

// AllowsPrivateCapture reports whether a capture in this scope may offer to save to a
// member's private space.
//
// A conversation may offer a private destination exactly when it already has one. A
// scope that reads only the household's shared memory offering a "personal"
// destination would turn a shared conversation into a write path into a private
// space, which is the one thing the memory model exists to prevent — and that is as
// true of a member's private chat with kenward, where they might reasonably expect
// otherwise, as it is of the household chat, where nobody would.
func (s Scope) AllowsPrivateCapture() bool { return s.TouchesPrivateMemory() }
