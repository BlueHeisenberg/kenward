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
			Name:      e.Name,
			BaseURL:   e.BaseURL,
			Model:     e.Model,
			APIKeyEnv: e.APIKeyEnv,
			Tags:      copyStrings(e.Tags),
			Timeout:   e.Timeout.Duration(),
		})
	}
	return out
}

// MemberByTelegramID finds the enrolled member bound to a Telegram user id.
//
// A member whose telegram_id is still zero is never returned, and a zero id never
// matches anyone: an unclaimed member and an unknown sender must be indistinguishable
// to everything upstream, or an unenrolled row would become a way in.
func (c *Config) MemberByTelegramID(id int64) (domain.Member, bool) {
	if id == 0 {
		return domain.Member{}, false
	}
	for _, m := range c.Members {
		if m.TelegramID == id {
			return m.Domain(), true
		}
	}
	return domain.Member{}, false
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
