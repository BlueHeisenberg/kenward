package config

import (
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// DomainHousehold converts the household section into the domain type.
func (c *Config) DomainHousehold() domain.Household {
	return domain.Household{
		Name:        c.Household.Name,
		Shared:      domain.SpaceID(c.Household.SharedSpace),
		GroupChatID: c.Household.GroupChatID,
		Tiers:       copyStrings(c.Household.Tiers),
	}
}

// AgentPerMember reports whether every member has an agent of their own, with kenward
// alongside as the household's rather than as everybody's.
//
// It is the one predicate the third scope hangs on. Under one agent there is nothing
// for a private conversation with kenward to be separate from — the member's own chat
// already is one — so domain.ScopeHousehold does not exist and scope.Resolve never
// produces it.
//
// Simple mode answers false whatever the file says, and that is a refusal rather than
// a default. One agent each needs one bot per member, because two agents sharing one
// Telegram contact are one agent; simple mode runs the household behind a single bot
// token, so honouring the setting there would resolve every member's private chat to
// kenward's and quietly take away the private assistant they were promised. Validation
// reports the combination with the reason; this is what stands behind it for the
// configurations that reach here unvalidated.
func (c *Config) AgentPerMember() bool {
	return c.Mode == ModeIsolated && c.Household.Agents == AgentsPerMember
}

// DomainMembers converts the members section into domain types, in file order.
//
// Every slice is copied, so nothing downstream can reach back into the configuration
// and change a tier chain — the chain is the privacy policy, and it is not something a
// later package may edit in place.
func (c *Config) DomainMembers() []domain.Member {
	if len(c.Members) == 0 {
		return nil
	}
	out := make([]domain.Member, 0, len(c.Members))
	for _, m := range c.Members {
		out = append(out, m.Domain())
	}
	return out
}

// Domain converts one member. EnrolledAt is whatever MergeState folded in from the
// state file, and stays zero for a member who has not claimed an invite.
func (m MemberConfig) Domain() domain.Member {
	return domain.Member{
		ID:          domain.MemberID(m.ID),
		Name:        m.Name,
		TelegramID:  m.TelegramID,
		Private:     domain.SpaceID(m.PrivateSpace),
		Tiers:       copyStrings(m.Tiers),
		BotTokenEnv: m.BotTokenEnv,
		EnrolledAt:  m.EnrolledAt,
	}
}

// RoutingEndpoints converts the endpoints section into the routing package's type, so
// the wiring layer can hand the list straight to a router without a conversion step of
// its own.
func (c *Config) RoutingEndpoints() []routing.Endpoint {
	if len(c.Endpoints) == 0 {
		return nil
	}
	out := make([]routing.Endpoint, 0, len(c.Endpoints))
	for _, e := range c.Endpoints {
		out = append(out, routing.Endpoint{
			Name:    e.Name,
			BaseURL: e.BaseURL,
			Model:   e.Model,
			Tags:    copyStrings(e.Tags),
			Timeout: e.Timeout.Duration(),
		})
	}
	return out
}

// MemberByTelegramID finds the enrolled member bound to a Telegram user id.
//
// A member whose telegram_id is still zero is never returned, and a zero id never
// matches anyone: an unclaimed member and an unknown sender must be indistinguishable
// to everything upstream, or an unenrolled row would become a way in.
//
// Two members bound to one Telegram account resolve to nobody rather than to the first
// of them. Validation rejects that configuration, but this is the lookup scope.Resolve
// makes its authorization decision with, and Resolve is documented as being called with
// configurations it did not validate. First-match-wins would answer one of two people's
// messages into the other's private space; answering neither is the only reading that is
// not a guess about whose memory this is.
func (c *Config) MemberByTelegramID(id int64) (domain.Member, bool) {
	if id == 0 {
		return domain.Member{}, false
	}
	found := -1
	for i, m := range c.Members {
		if m.TelegramID != id {
			continue
		}
		if found >= 0 {
			return domain.Member{}, false
		}
		found = i
	}
	if found < 0 {
		return domain.Member{}, false
	}
	return c.Members[found].Domain(), true
}

// MemberByID finds a member by their stable internal id.
func (c *Config) MemberByID(id domain.MemberID) (domain.Member, bool) {
	for _, m := range c.Members {
		if domain.MemberID(m.ID) == id {
			return m.Domain(), true
		}
	}
	return domain.Member{}, false
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
